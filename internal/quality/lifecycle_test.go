package quality

import (
	"context"
	"errors"
	"runtime"
	"sync/atomic"
	"testing"

	"github.com/0x524A/rtspeek/pkg/rtspeek"
)

func TestCancelStopsARunningTest(t *testing.T) {
	f := newFake(analysis(), nil) // never released
	r := newRunner(t, f, 1)
	ch := r.Subscribe(context.Background())
	_, _ = r.Start("a", "rtsp://x/a", Request{})
	<-f.urls
	recv(t, ch) // running

	if !r.Cancel("a") {
		t.Fatal("Cancel on a running test = false")
	}
	if f.live.Load() != 0 {
		t.Error("the analysis was still running when Cancel returned")
	}
	run, _ := r.Get("a")
	if run.State != StateFailed || run.Error != "cancelled" || run.FinishedAt == nil || run.Analysis != nil {
		t.Errorf("run = %+v", run)
	}
	if ev := recv(t, ch); ev != (Event{Camera: "a", State: StateFailed}) {
		t.Errorf("event = %+v", ev)
	}
	if _, err := r.Start("b", "rtsp://x/b", Request{}); err != nil {
		t.Errorf("the cancelled run kept its slot: %v", err)
	}
}

func TestCancelWithNothingRunning(t *testing.T) {
	f := newFake(analysis(), nil)
	close(f.release)
	r := newRunner(t, f, 0)
	if r.Cancel("nope") {
		t.Error("Cancel on an unknown camera = true")
	}
	_, _ = r.Start("a", "rtsp://x/a", Request{})
	waitState(t, r, "a", StateDone)
	if r.Cancel("a") {
		t.Error("Cancel on a finished run = true")
	}
	if run, _ := r.Get("a"); run.State != StateDone {
		t.Errorf("Cancel changed a finished run: %+v", run)
	}
}

func TestForgetCancelsAndDropsTheResult(t *testing.T) {
	f := newFake(analysis(), nil) // never released
	r := newRunner(t, f, 0)
	_, _ = r.Start("a", "rtsp://x/a", Request{})
	<-f.urls

	r.Forget("a")
	if f.live.Load() != 0 {
		t.Error("the analysis was still running when Forget returned")
	}
	if _, ok := r.Get("a"); ok {
		t.Error("Forget left a result behind")
	}
	r.Forget("a")    // forgetting twice is fine
	r.Forget("nope") // and so is an unknown camera
}

func TestForgetDropsAFinishedResult(t *testing.T) {
	f := newFake(analysis(), nil)
	close(f.release)
	r := newRunner(t, f, 0)
	_, _ = r.Start("a", "rtsp://x/a", Request{})
	waitState(t, r, "a", StateDone)
	r.Forget("a")
	if _, ok := r.Get("a"); ok {
		t.Error("Forget left a finished result behind")
	}
}

// A run that was forgotten and replaced under the same id must never have its
// record overwritten by the old run finishing late.
func TestAForgottenRunCannotOverwriteANewerOne(t *testing.T) {
	var calls atomic.Int32
	second := make(chan struct{})
	analyze := func(ctx context.Context, _ string, _ rtspeek.AnalyzeOptions) (*rtspeek.Analysis, error) {
		if calls.Add(1) == 1 {
			<-ctx.Done() // the first run only ends when it is cancelled
			return nil, errors.New("late failure from the first run")
		}
		<-second
		return analysis(rtspeek.SeverityWarn), nil
	}
	r := New(Options{Analyze: analyze})
	t.Cleanup(r.Close)

	_, _ = r.Start("a", "rtsp://x/a", Request{})
	for calls.Load() < 1 {
		runtime.Gosched()
	}
	r.Forget("a") // returns after the first run has fully stopped

	if _, err := r.Start("a", "rtsp://x/a", Request{}); err != nil {
		t.Fatal(err)
	}
	close(second)
	run := waitState(t, r, "a", StateDone)
	if run.Verdict != VerdictWarn || run.Error != "" {
		t.Errorf("the newer run was overwritten: %+v", run)
	}
}
