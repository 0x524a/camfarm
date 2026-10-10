package rtsp

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/liberrors"

	"github.com/0x524a/camfarm/internal/fault"
)

// describeStatus opens a fresh connection, sends one DESCRIBE and returns the
// status the server answered.
func describeStatus(t *testing.T, s *Server) base.StatusCode {
	t.Helper()
	u, err := base.ParseURL(s.URL("cam-00"))
	if err != nil {
		t.Fatal(err)
	}
	c := gortsplib.Client{Scheme: u.Scheme, Host: u.Host}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _, err = c.Describe(u)
	var bad liberrors.ErrClientBadStatusCode
	if errors.As(err, &bad) {
		return bad.Code
	}
	if err != nil {
		t.Fatal(err)
	}
	return base.StatusOK
}

func TestRTSPErrorOnlyRequestAnswersTheConfiguredStatus(t *testing.T) {
	s, rec := startFaultServer(t, nil)
	if err := s.SetFaults("cam-00", []fault.Spec{{Kind: fault.KindRTSPError, On: "describe", Status: 454, OnlyRequest: 2}}, nil); err != nil {
		t.Fatal(err)
	}
	if got := describeStatus(t, s); got != base.StatusOK {
		t.Fatalf("request 1 = %v, want 200", got)
	}
	if got := describeStatus(t, s); got != 454 {
		t.Fatalf("request 2 = %v, want 454", got)
	}
	if got := describeStatus(t, s); got != base.StatusOK {
		t.Fatalf("request 3 = %v, want 200", got)
	}
	if got := rec.Counters("cam-00").ByKind["rtsp_error"].Applied; got != 1 {
		t.Fatalf("rtsp_error tally = %d, want 1", got)
	}
}

func TestSetFaultsRoutesEachKindAndClearClearsBoth(t *testing.T) {
	s, rec := startFaultServer(t, nil)
	type note struct {
		index int
		clock string
	}
	notes := make(chan note, 8)
	applied := func(i int, clock string) { notes <- note{i, clock} }

	specs := []fault.Spec{
		{Kind: fault.KindFrameDrop, Rate: 1},
		{Kind: fault.KindRTSPError, On: "all", Status: 503, Rate: 1},
	}
	if err := s.SetFaults("cam-00", specs, applied); err != nil {
		t.Fatal(err)
	}
	// Effect order: the control class is live at once, the frame class at the
	// next frame boundary.
	if n := <-notes; n.clock != "request" {
		t.Fatalf("first applied clock = %q, want request", n.clock)
	}
	if n := <-notes; n.clock != "frame" {
		t.Fatalf("second applied clock = %q, want frame", n.clock)
	}
	if got := describeStatus(t, s); got != 503 {
		t.Fatalf("DESCRIBE = %v, want 503", got)
	}
	awaitCond(t, "frame drops", func() bool { return rec.Counters("cam-00").ByKind["frame_drop"].Applied > 0 })

	if err := s.SetFaults("cam-00", nil, applied); err != nil {
		t.Fatal(err)
	}
	// A mixed clear on a camera with no readers reports both classes it removed.
	if n := <-notes; n.clock != "request" {
		t.Fatalf("first clear clock = %q, want request", n.clock)
	}
	if n := <-notes; n.clock != "frame" {
		t.Fatalf("second clear clock = %q, want frame", n.clock)
	}
	if got := describeStatus(t, s); got != base.StatusOK {
		t.Fatalf("DESCRIBE after clear = %v, want 200", got)
	}
	before := rec.Counters("cam-00").FramesServed
	awaitCond(t, "frames to flow again", func() bool { return rec.Counters("cam-00").FramesServed > before })
}

func TestSetFaultsControlOnlyAppliesImmediatelyWithTheRequestClock(t *testing.T) {
	s, _ := startFaultServer(t, nil)
	_ = describeStatus(t, s) // one request already decided
	var clock string
	var index int
	err := s.SetFaults("cam-00", []fault.Spec{{Kind: fault.KindRTSPError, On: "play", Status: 503, Rate: 1}}, func(i int, c string) { index, clock = i, c })
	if err != nil {
		t.Fatal(err)
	}
	if clock != "request" || index != 1 {
		t.Fatalf("applied (%d, %q), want (1, request) synchronously", index, clock)
	}
}

func TestSetFaultsLeavesAnUnchangedClassAloneWhenTheOtherChanges(t *testing.T) {
	s, _ := startFaultServer(t, nil)
	var mu sync.Mutex
	var clocks []string
	applied := func(_ int, clock string) { mu.Lock(); clocks = append(clocks, clock); mu.Unlock() }
	got := func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), clocks...) }
	rtspErr := fault.Spec{Kind: fault.KindRTSPError, On: "describe", Status: 454, OnlyRequest: 2}
	if err := s.SetFaults("cam-00", []fault.Spec{rtspErr}, applied); err != nil {
		t.Fatal(err)
	}
	if got := describeStatus(t, s); got != base.StatusOK {
		t.Fatalf("request 1 = %v, want 200", got)
	}
	// Control-only -> mixed: only the frame class changes. If the control
	// engine were rebuilt, its OnlyRequest count would restart and request 2
	// below would not be the 454.
	both := []fault.Spec{rtspErr, {Kind: fault.KindFrameDrop, Rate: 0.5}}
	if err := s.SetFaults("cam-00", both, applied); err != nil {
		t.Fatal(err)
	}
	awaitCond(t, "the frame notification", func() bool { return len(got()) >= 2 })
	// The first set was control-only (request); the second changed only frame.
	if c := got(); len(c) != 2 || c[0] != "request" || c[1] != "frame" {
		t.Fatalf("clocks = %v, want [request frame]", c)
	}
	if got := describeStatus(t, s); got != 454 {
		t.Fatalf("request 2 = %v, want 454 (control engine must not restart)", got)
	}
	// Resubmitting the same set changes nothing and notifies nothing.
	if err := s.SetFaults("cam-00", both, applied); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // a wrongly queued pump swap would report by now
	if c := got(); len(c) != 2 {
		t.Fatalf("an unchanged set was notified: %v", c)
	}
}

func TestAnswerErrorAlwaysCarriesAReasonPhrase(t *testing.T) {
	s, _ := startFaultServer(t, nil)
	for _, c := range []struct {
		status int
		want   string
	}{{454, ""}, {418, "Client Error"}, {599, "Server Error"}} {
		res := s.answerError(fault.MethodDescribe, "cam-00", fault.ControlDecision{Request: 1, Error: &fault.ErrorDecision{Status: c.status}})
		if int(res.StatusCode) != c.status {
			t.Fatalf("status = %v, want %d", res.StatusCode, c.status)
		}
		if c.want == "" {
			if _, known := base.StatusMessages[res.StatusCode]; !known {
				t.Fatalf("%d is expected to be in gortsplib's table", c.status)
			}
			continue
		}
		if !strings.Contains(res.StatusMessage, c.want) {
			t.Fatalf("%d message = %q, want %q", c.status, res.StatusMessage, c.want)
		}
	}
}
