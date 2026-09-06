package obs

import (
	"sync"
	"testing"
)

func TestCountersPerCamera(t *testing.T) {
	r := New(100)

	r.Frame("a")
	r.Frame("a")
	r.Frame("b")
	r.Loop("a")
	r.Fault("b", 7, "frame_drop", "decided")

	if got := r.Counters("a"); got.FramesServed != 2 || got.LoopsCompleted != 1 || got.FaultsFired != 0 {
		t.Errorf("camera a: %+v", got)
	}
	if got := r.Counters("b"); got.FramesServed != 1 || got.LoopsCompleted != 0 || got.FaultsFired != 1 {
		t.Errorf("camera b: %+v", got)
	}
	if got := r.Counters("unknown"); got != (Counters{}) {
		t.Errorf("unknown camera: %+v, want zero", got)
	}
}

// Spec section 8.2: a fault firing records enough to reproduce it.
func TestFaultEventsCarryFrameIndexAndKind(t *testing.T) {
	r := New(100)
	r.Fault("front-door", 42, "keyframe_starvation", "decided; effect not implemented")

	ev := r.Events()
	if len(ev) != 1 {
		t.Fatalf("events = %d, want 1", len(ev))
	}
	if ev[0].CameraID != "front-door" || ev[0].FrameIndex != 42 || ev[0].Kind != "keyframe_starvation" {
		t.Fatalf("event = %+v", ev[0])
	}
	if ev[0].Seq != 1 {
		t.Errorf("Seq = %d, want 1", ev[0].Seq)
	}
}

// Only faults are logged as events. Frames are far too many to log and are
// counted instead.
func TestFramesAreCountedNotLogged(t *testing.T) {
	r := New(100)
	for i := 0; i < 50; i++ {
		r.Frame("a")
	}
	if n := len(r.Events()); n != 0 {
		t.Fatalf("events = %d, want 0", n)
	}
	if got := r.Counters("a").FramesServed; got != 50 {
		t.Fatalf("FramesServed = %d, want 50", got)
	}
}

// A fleet may run for hours. The log is bounded, and says so rather than
// silently forgetting.
func TestEventLogIsBoundedAndReportsDrops(t *testing.T) {
	r := New(4)
	for i := 0; i < 10; i++ {
		r.Fault("a", i, "frame_drop", "")
	}
	ev := r.Events()
	if len(ev) != 4 {
		t.Fatalf("events = %d, want 4", len(ev))
	}
	// The most recent must be kept: a failing test cares about what just
	// happened.
	if ev[len(ev)-1].FrameIndex != 9 {
		t.Errorf("last event frame = %d, want 9", ev[len(ev)-1].FrameIndex)
	}
	// Events() is documented oldest-first. Pin the whole sequence, not just the
	// tail: checking only the last element would not catch a reversed log.
	want := []int{6, 7, 8, 9}
	for i, w := range want {
		if ev[i].FrameIndex != w {
			t.Fatalf("event[%d].FrameIndex = %d, want %d (full order = %v)", i, ev[i].FrameIndex, w, frameIndices(ev))
		}
	}
	if got := r.Dropped(); got != 6 {
		t.Errorf("Dropped() = %d, want 6", got)
	}
}

// frameIndices extracts FrameIndex from a slice of Events, for failure
// messages.
func frameIndices(ev []Event) []int {
	out := make([]int, len(ev))
	for i, e := range ev {
		out[i] = e.FrameIndex
	}
	return out
}

func TestEventsReturnsACopy(t *testing.T) {
	r := New(10)
	r.Fault("a", 1, "k", "")
	ev := r.Events()
	ev[0].CameraID = "mutated"
	if r.Events()[0].CameraID != "a" {
		t.Fatal("Events() exposed internal storage")
	}
}

func TestConcurrentUse(t *testing.T) {
	r := New(1000)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				r.Frame("a")
				r.Fault("a", j, "frame_drop", "")
				_ = r.Counters("a")
				_ = r.Events()
			}
		}()
	}
	wg.Wait()
	if got := r.Counters("a").FramesServed; got != 800 {
		t.Fatalf("FramesServed = %d, want 800", got)
	}
}
