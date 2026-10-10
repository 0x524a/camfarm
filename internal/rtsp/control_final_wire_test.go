package rtsp

import (
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/base"

	"github.com/0x524a/camfarm/internal/fault"
	"github.com/0x524a/camfarm/internal/seed"
)

// I1: an rtsp_error without a status answers the catalogue default on the wire.
func TestRTSPErrorWithoutStatusAnswers503OnTheWire(t *testing.T) {
	s, _ := startFaultServer(t, nil)
	if err := s.SetFaults("cam-00", []fault.Spec{{Kind: fault.KindRTSPError, On: "describe", Rate: 1}}, nil); err != nil {
		t.Fatal(err)
	}
	if got := describeStatus(t, s); got != base.StatusServiceUnavailable {
		t.Fatalf("DESCRIBE = %v, want 503", got)
	}
}

// I2: clearing the control class disarms a pending teardown.
func TestClearDisarmsAPendingTeardown(t *testing.T) {
	s, rec := startFaultServer(t, nil)
	if err := s.SetFaults("cam-00", []fault.Spec{{Kind: fault.KindTeardown, AfterFrames: 20, Mode: "close", Rate: 1}}, nil); err != nil {
		t.Fatal(err)
	}
	c := playClient(t, s.URL("cam-00"), func() {})
	if err := s.SetFaults("cam-00", nil, nil); err != nil {
		t.Fatal(err)
	}
	before := rec.Counters("cam-00").FramesServed
	awaitCond(t, "frames", func() bool { return rec.Counters("cam-00").FramesServed >= before+40 })
	select {
	case err := <-c.Done():
		t.Fatalf("session torn down after the fault was cleared: %v", err)
	default:
	}
	if got := rec.Counters("cam-00").ByKind["teardown"].Applied; got != 0 {
		t.Fatalf("teardown tally = %d, want 0", got)
	}
}

// I2: re-injecting a changed teardown drops the entry armed by the old one.
func TestChangedTeardownDisarmsTheOldEntry(t *testing.T) {
	s, rec := startFaultServer(t, nil)
	_ = s.SetFaults("cam-00", []fault.Spec{{Kind: fault.KindTeardown, AfterFrames: 5, Mode: "close", Rate: 1}}, nil)
	c := playClient(t, s.URL("cam-00"), func() {})
	// Replace at once with a far-off teardown: the old 5-frame entry must go.
	if err := s.SetFaults("cam-00", []fault.Spec{{Kind: fault.KindTeardown, AfterFrames: 100000, Mode: "close", Rate: 1}}, nil); err != nil {
		t.Fatal(err)
	}
	before := rec.Counters("cam-00").FramesServed
	awaitCond(t, "frames", func() bool { return rec.Counters("cam-00").FramesServed >= before+30 })
	select {
	case err := <-c.Done():
		t.Fatalf("stale armed entry tore the session down: %v", err)
	default:
	}
}

// I2: a stale entry from a removed camera must not hit a camera re-added under
// the same ID.
func TestRemoveCameraDropsArmedEntries(t *testing.T) {
	s, rec := startFaultServer(t, nil)
	_ = s.SetFaults("cam-00", []fault.Spec{{Kind: fault.KindTeardown, AfterFrames: 100000, Mode: "close", Rate: 1}}, nil)
	_ = playClient(t, s.URL("cam-00"), func() {})
	if err := s.RemoveCamera("cam-00"); err != nil {
		t.Fatal(err)
	}
	s.armMu.Lock()
	n := len(s.armed)
	s.armMu.Unlock()
	if n != 0 {
		t.Fatalf("%d armed entries survive RemoveCamera", n)
	}
	if err := s.AddCamera(CameraConfig{ID: "cam-00", Media: testMedia(t), Seed: seed.Seed(22)}); err != nil {
		t.Fatal(err)
	}
	c := playClient(t, s.URL("cam-00"), func() {})
	before := rec.Counters("cam-00").FramesServed
	awaitCond(t, "frames", func() bool { return rec.Counters("cam-00").FramesServed >= before+20 })
	select {
	case err := <-c.Done():
		t.Fatalf("re-added camera's session was torn down: %v", err)
	default:
	}
}

// I3: an errored PLAY still takes its teardown draw, so later PLAYs arm exactly
// as they would without the error.
func TestErroredPlayDoesNotShiftTheTeardownSequence(t *testing.T) {
	const plays = 8
	// The reference: the engine's own sequence, one draw per PLAY.
	ref := fault.NewControlEngine(seed.Seed(21).Stream("fault"))
	_ = ref.Set([]fault.Spec{{Kind: fault.KindTeardown, AfterFrames: 2, Mode: "close", Rate: 0.5}})
	want := make([]bool, plays)
	for i := range want {
		want[i] = ref.ArmPlay() != nil
	}

	s, rec := startFaultServer(t, []fault.Spec{
		{Kind: fault.KindTeardown, AfterFrames: 2, Mode: "close", Rate: 0.5},
		{Kind: fault.KindRTSPError, On: "play", Status: 454, OnlyRequest: 1},
	})
	// PLAY 1 is answered with the error.
	u, _ := base.ParseURL(s.URL("cam-00"))
	c, _ := newTestClient(u)
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	desc, _, err := c.Describe(u)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SetupAll(desc.BaseURL, desc.Medias); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Play(nil); err == nil {
		t.Fatal("PLAY 1 was not refused")
	}
	c.Close()

	for i := 1; i < plays; i++ {
		pc := playClient(t, s.URL("cam-00"), func() {})
		before := rec.Counters("cam-00").FramesServed
		awaitCond(t, "frames", func() bool { return rec.Counters("cam-00").FramesServed >= before+8 })
		torn := false
		select {
		case <-pc.Done():
			torn = true
		case <-time.After(50 * time.Millisecond):
		}
		if torn != want[i] {
			t.Fatalf("PLAY %d torn down = %v, want %v (reference %v)", i+1, torn, want[i], want)
		}
		pc.c.Close()
	}
}
