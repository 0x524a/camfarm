package quality

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0x524A/rtspeek/pkg/rtspeek"
)

// fake is the Analyze seam. A call blocks until release is closed or its
// context ends, so a test decides exactly when a run finishes.
type fake struct {
	release chan struct{}
	res     *rtspeek.Analysis
	err     error
	live    atomic.Int32
	urls    chan string
	opts    chan rtspeek.AnalyzeOptions
}

func newFake(res *rtspeek.Analysis, err error) *fake {
	return &fake{
		release: make(chan struct{}), res: res, err: err,
		urls: make(chan string, 64), opts: make(chan rtspeek.AnalyzeOptions, 64),
	}
}

func (f *fake) analyze(ctx context.Context, url string, o rtspeek.AnalyzeOptions) (*rtspeek.Analysis, error) {
	f.live.Add(1)
	defer f.live.Add(-1)
	f.urls <- url
	f.opts <- o
	select {
	case <-f.release:
		return f.res, f.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func analysis(severities ...string) *rtspeek.Analysis {
	a := &rtspeek.Analysis{}
	for _, s := range severities {
		a.Findings = append(a.Findings, rtspeek.Finding{Severity: s, Code: "x_" + s, Message: s})
	}
	return a
}

func waitState(t *testing.T, r *Runner, cam string, want State) Run {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if run, ok := r.Get(cam); ok && run.State == want {
			return run
		}
		if time.Now().After(deadline) {
			run, ok := r.Get(cam)
			t.Fatalf("camera %q never reached %q (have %+v, ok=%v)", cam, want, run, ok)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func newRunner(t *testing.T, f *fake, max int) *Runner {
	t.Helper()
	r := New(Options{MaxConcurrent: max, Analyze: f.analyze})
	t.Cleanup(r.Close)
	return r
}

func TestStartAppliesDefaultsAndPassesTheURL(t *testing.T) {
	f := newFake(analysis(), nil)
	r := newRunner(t, f, 0)

	run, err := r.Start("cam", "rtsp://127.0.0.1:1/cam", Request{})
	if err != nil {
		t.Fatal(err)
	}
	if run.State != StateRunning || run.Camera != "cam" || run.StartedAt.IsZero() || run.FinishedAt != nil {
		t.Errorf("run = %+v", run)
	}
	if run.Request != (Request{DurationSeconds: 5, Transport: TransportAuto}) {
		t.Errorf("request after defaults = %+v", run.Request)
	}
	if u := <-f.urls; u != "rtsp://127.0.0.1:1/cam" {
		t.Errorf("analysed %q", u)
	}
	if o := <-f.opts; o.Duration != 5*time.Second || o.Transport != rtspeek.TransportAuto {
		t.Errorf("options = %+v", o)
	}

	close(f.release)
	done := waitState(t, r, "cam", StateDone)
	if done.Verdict != VerdictOK || done.FinishedAt == nil || done.Analysis == nil || done.Error != "" {
		t.Errorf("done = %+v", done)
	}
}

func TestVerdictIsTheWorstSeverity(t *testing.T) {
	for name, tc := range map[string]struct {
		in   []string
		want string
	}{
		"no findings":      {nil, VerdictOK},
		"info only":        {[]string{rtspeek.SeverityInfo}, VerdictOK},
		"warning":          {[]string{rtspeek.SeverityInfo, rtspeek.SeverityWarn}, VerdictWarn},
		"error beats warn": {[]string{rtspeek.SeverityWarn, rtspeek.SeverityError, rtspeek.SeverityInfo}, VerdictError},
		"unknown severity": {[]string{"mystery"}, VerdictOK},
	} {
		if got := verdict(analysis(tc.in...)); got != tc.want {
			t.Errorf("%s: verdict = %q, want %q", name, got, tc.want)
		}
	}
}

func TestFailedRuns(t *testing.T) {
	for name, tc := range map[string]struct {
		res     *rtspeek.Analysis
		err     error
		wantErr string
		keepAn  bool
	}{
		"analyser error":       {nil, errors.New("dial refused"), "dial refused", false},
		"analysis carries err": {&rtspeek.Analysis{Error: "setup failed"}, nil, "setup failed", true},
		"no analysis":          {nil, nil, "no analysis", false},
	} {
		f := newFake(tc.res, tc.err)
		close(f.release)
		r := newRunner(t, f, 0)
		if _, err := r.Start("cam", "rtsp://x/cam", Request{}); err != nil {
			t.Fatal(err)
		}
		run := waitState(t, r, "cam", StateFailed)
		if run.Error == "" || !strings.Contains(run.Error, tc.wantErr) || run.Verdict != "" || run.FinishedAt == nil {
			t.Errorf("%s: run = %+v", name, run)
		}
		if (run.Analysis != nil) != tc.keepAn {
			t.Errorf("%s: analysis kept = %v, want %v", name, run.Analysis != nil, tc.keepAn)
		}
	}
}

func TestStartValidatesTheRequest(t *testing.T) {
	f := newFake(analysis(), nil)
	close(f.release)
	r := newRunner(t, f, 0)
	for name, req := range map[string]Request{
		"too short":       {DurationSeconds: 0.5},
		"too long":        {DurationSeconds: 31},
		"negative":        {DurationSeconds: -1},
		"not a number":    {DurationSeconds: math.NaN()},
		"infinite":        {DurationSeconds: math.Inf(1)},
		"unknown network": {Transport: "sctp"},
	} {
		if _, err := r.Start("cam", "rtsp://x", req); !errors.Is(err, ErrBadRequest) {
			t.Errorf("%s: err = %v, want ErrBadRequest", name, err)
		}
	}
	if _, ok := r.Get("cam"); ok || f.live.Load() != 0 || len(f.urls) != 0 {
		t.Error("a rejected request must not create a run or call the analyser")
	}
	for name, req := range map[string]Request{
		"lower bound": {DurationSeconds: 1, Transport: TransportUDP},
		"upper bound": {DurationSeconds: 30, Transport: TransportTCP},
	} {
		if _, err := r.Start(name, "rtsp://x", req); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestOnlyOneRunPerCamera(t *testing.T) {
	f := newFake(analysis(), nil)
	r := newRunner(t, f, 0)
	if _, err := r.Start("a", "rtsp://x/a", Request{}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Start("a", "rtsp://x/a", Request{}); !errors.Is(err, ErrRunning) {
		t.Errorf("second start on a = %v, want ErrRunning", err)
	}
	if _, err := r.Start("b", "rtsp://x/b", Request{}); err != nil {
		t.Errorf("start on another camera = %v", err)
	}
	close(f.release)
	waitState(t, r, "a", StateDone)
	if _, err := r.Start("a", "rtsp://x/a", Request{}); err != nil {
		t.Errorf("start after the run finished = %v", err)
	}
}

func TestGlobalCapAndSlotRelease(t *testing.T) {
	f := newFake(analysis(), nil)
	r := newRunner(t, f, 0) // 0 means the default of 2
	for _, id := range []string{"a", "b"} {
		if _, err := r.Start(id, "rtsp://x/"+id, Request{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.Start("c", "rtsp://x/c", Request{}); !errors.Is(err, ErrBusy) {
		t.Fatalf("third start = %v, want ErrBusy", err)
	}
	close(f.release)
	waitState(t, r, "a", StateDone)
	waitState(t, r, "b", StateDone)
	if _, err := r.Start("c", "rtsp://x/c", Request{}); err != nil {
		t.Errorf("start after slots freed = %v", err)
	}
}

func TestSlotIsReleasedWhenTheAnalyserFails(t *testing.T) {
	f := newFake(nil, errors.New("boom"))
	close(f.release)
	r := newRunner(t, f, 1)
	if _, err := r.Start("a", "rtsp://x/a", Request{}); err != nil {
		t.Fatal(err)
	}
	waitState(t, r, "a", StateFailed)
	if _, err := r.Start("b", "rtsp://x/b", Request{}); err != nil {
		t.Errorf("a failed run kept its slot: %v", err)
	}
}

func TestGetUnknownCamera(t *testing.T) {
	r := newRunner(t, newFake(nil, nil), 0)
	if _, ok := r.Get("nope"); ok {
		t.Error("Get on an unknown camera must report false")
	}
}

func TestANewRunReplacesThePreviousResult(t *testing.T) {
	f := newFake(analysis(rtspeek.SeverityWarn), nil)
	close(f.release)
	r := newRunner(t, f, 0)
	_, _ = r.Start("a", "rtsp://x/a", Request{})
	waitState(t, r, "a", StateDone)

	f2 := newFake(analysis(), nil) // a second run that stays blocked
	r.analyze = f2.analyze
	run, err := r.Start("a", "rtsp://x/a", Request{})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := r.Get("a")
	if got.State != StateRunning || got.Analysis != nil || got.FinishedAt != nil || got.Verdict != "" || run.State != StateRunning {
		t.Errorf("after restart: %+v", got)
	}
}

func recv(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("event channel closed")
		}
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("no event")
	}
	return Event{}
}

func TestEventsArriveInOrder(t *testing.T) {
	f := newFake(analysis(rtspeek.SeverityWarn), nil)
	r := newRunner(t, f, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := r.Subscribe(ctx)

	_, _ = r.Start("a", "rtsp://x/a", Request{})
	if ev := recv(t, ch); ev != (Event{Camera: "a", State: StateRunning}) {
		t.Errorf("first event = %+v", ev)
	}
	close(f.release)
	if ev := recv(t, ch); ev != (Event{Camera: "a", State: StateDone, Verdict: VerdictWarn}) {
		t.Errorf("second event = %+v", ev)
	}
}

func TestASlowSubscriberIsDroppedAndTheRunnerKeepsWorking(t *testing.T) {
	f := newFake(analysis(), nil)
	close(f.release)
	r := newRunner(t, f, 0)
	ch := r.Subscribe(context.Background()) // never read until the end

	for i := 0; i < eventBuffer; i++ { // two events per run: far more than the buffer
		id := string(rune('a' + i))
		if _, err := r.Start(id, "rtsp://x/"+id, Request{}); err != nil {
			t.Fatal(err)
		}
		waitState(t, r, id, StateDone)
	}
	n := 0
	deadline := time.After(5 * time.Second)
	for open := true; open; {
		select {
		case _, ok := <-ch:
			open = ok
			if ok {
				n++
			}
		case <-deadline:
			t.Fatal("the slow subscriber's channel was never closed")
		}
	}
	if n > eventBuffer {
		t.Errorf("a dropped subscriber received %d events, buffer is %d", n, eventBuffer)
	}
	if _, err := r.Start("z", "rtsp://x/z", Request{}); err != nil {
		t.Errorf("the runner stopped working after dropping a subscriber: %v", err)
	}
}

func TestSubscribeEndsWithItsContext(t *testing.T) {
	r := newRunner(t, newFake(nil, nil), 0)
	ctx, cancel := context.WithCancel(context.Background())
	ch := r.Subscribe(ctx)
	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("got an event instead of a close")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("channel not closed after the context ended")
	}
}

func TestCloseCancelsEveryRunAndWaits(t *testing.T) {
	f := newFake(analysis(), nil) // never released
	r := New(Options{Analyze: f.analyze})
	ch := r.Subscribe(context.Background())
	_, _ = r.Start("a", "rtsp://x/a", Request{})
	_, _ = r.Start("b", "rtsp://x/b", Request{})
	for len(f.urls) < 2 {
		time.Sleep(time.Millisecond)
	}

	r.Close()
	if n := f.live.Load(); n != 0 {
		t.Fatalf("%d analyses still running after Close", n)
	}
	if run, _ := r.Get("a"); run.State != StateFailed || run.Error != "cancelled" {
		t.Errorf("run after Close = %+v", run)
	}
	if _, err := r.Start("c", "rtsp://x/c", Request{}); !errors.Is(err, ErrClosed) {
		t.Errorf("Start after Close = %v, want ErrClosed", err)
	}
	for range ch { // must terminate: Close ends every subscription
	}
	r.Close() // idempotent
}
