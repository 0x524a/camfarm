package rtsp

import (
	"errors"
	"testing"

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
	if n := <-notes; n.clock != "frame" {
		t.Fatalf("first applied clock = %q, want frame", n.clock)
	}
	if n := <-notes; n.clock != "request" {
		t.Fatalf("second applied clock = %q, want request", n.clock)
	}
	if got := describeStatus(t, s); got != 503 {
		t.Fatalf("DESCRIBE = %v, want 503", got)
	}
	awaitCond(t, "frame drops", func() bool { return rec.Counters("cam-00").ByKind["frame_drop"].Applied > 0 })

	if err := s.SetFaults("cam-00", nil, applied); err != nil {
		t.Fatal(err)
	}
	<-notes
	<-notes
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
