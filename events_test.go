package camfarm

import (
	"context"
	"strings"
	"testing"
	"time"
)

func recv(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case e, ok := <-ch:
		if !ok {
			t.Fatal("channel closed")
		}
		return e
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for an event")
		return Event{}
	}
}

func TestSubscribeDeliversEventsInOrder(t *testing.T) {
	f := StartT(t, minimalSpec())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := f.Subscribe(ctx)

	if _, err := f.AddCamera(CameraSpec{ID: "extra"}); err != nil {
		t.Fatalf("AddCamera: %v", err)
	}
	cam, _ := f.Camera("extra")
	if err := cam.Inject(FaultSpec{Kind: "frame_drop", Rate: 1}); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	waitUntil(t, "inject to be journaled", func() bool { return len(f.Journal()) >= 2 })
	if err := cam.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	waitUntil(t, "clear to be journaled", func() bool { return len(f.Journal()) >= 3 })
	if err := f.RemoveCamera("extra"); err != nil {
		t.Fatalf("RemoveCamera: %v", err)
	}

	want := []EventType{EventCameraAdded, EventFaultInjected, EventFaultCleared, EventCameraRemoved}
	var last uint64
	for _, w := range want {
		e := recv(t, ch)
		if e.Type != w || e.Camera != "extra" {
			t.Fatalf("event = %+v, want %s for extra", e, w)
		}
		if e.Seq <= last {
			t.Fatalf("seq %d not increasing after %d", e.Seq, last)
		}
		last = e.Seq
	}
}

func TestSubscribeChannelClosesWhenContextEnds(t *testing.T) {
	f := StartT(t, minimalSpec())
	ctx, cancel := context.WithCancel(context.Background())
	ch := f.Subscribe(ctx)
	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("got an event instead of a close")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("channel not closed after ctx cancel")
	}
}

func TestStalledSubscriberIsDroppedWithoutBlocking(t *testing.T) {
	f := StartT(t, minimalSpec())
	ch := f.Subscribe(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 500; i++ {
			f.publish(Event{Type: EventCameraAdded, Camera: "x", FrameIndex: -1})
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("publish blocked on a subscriber that never reads")
	}
	// The stalled subscriber's channel ends up closed once drained.
	deadline := time.After(10 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("stalled subscriber was not closed")
		}
	}
}

func TestReplayLineCarriesTheJournal(t *testing.T) {
	f := StartT(t, minimalSpec())
	before := f.ReplayLine()
	if strings.Contains(before, "journal=") {
		t.Fatalf("fresh fleet replay line has a journal: %q", before)
	}
	if _, err := f.AddCamera(CameraSpec{ID: "extra"}); err != nil {
		t.Fatalf("AddCamera: %v", err)
	}
	after := f.ReplayLine()
	if !strings.Contains(after, "journal=1:") {
		t.Fatalf("replay line = %q, want journal=1:<hash>", after)
	}
	if !strings.HasPrefix(after, before) {
		t.Errorf("the seed and spec hash prefix changed: %q vs %q", after, before)
	}
}

func TestJournalRecordsFrameIndexForFaults(t *testing.T) {
	f := StartT(t, minimalSpec())
	cam, _ := f.Camera("front-door")
	waitUntil(t, "frames", func() bool { return cam.Stats().FramesServed > 3 })
	_ = cam.Inject(FaultSpec{Kind: "frame_drop", Rate: 0.5})
	waitUntil(t, "inject journaled", func() bool { return len(f.Journal()) == 1 })

	e := f.Journal()[0]
	if e.Action != "inject" || e.Camera != "front-door" || e.FrameIndex < 3 {
		t.Fatalf("journal entry = %+v", e)
	}
	if e.Clock != "frame" {
		t.Fatalf("clock = %q, want frame", e.Clock)
	}
}

func TestJournalClocksFaultChangesByClass(t *testing.T) {
	f := StartT(t, minimalSpec())
	cam, _ := f.Camera("front-door")
	waitUntil(t, "frames", func() bool { return cam.Stats().FramesServed > 3 })

	if err := cam.Inject(FaultSpec{Kind: "rtsp_error", On: "describe", Status: 503, Rate: 1}); err != nil {
		t.Fatalf("Inject rtsp_error: %v", err)
	}
	if err := cam.Inject(FaultSpec{Kind: "frame_drop", Rate: 0.5}); err != nil {
		t.Fatalf("Inject frame_drop: %v", err)
	}
	waitUntil(t, "journal", func() bool { return len(f.Journal()) >= 2 })

	j := f.Journal()
	if j[0].Clock != "request" || j[0].Fault.Kind != "rtsp_error" {
		t.Errorf("first entry = %+v, want a request-clocked rtsp_error", j[0])
	}
	// The second inject changes only the frame class, so it adds exactly one
	// entry: the active rtsp_error is not re-announced.
	if len(j) != 2 || j[1].Clock != "frame" || j[1].Fault.Kind != "frame_drop" {
		t.Errorf("journal = %+v, want a frame-clocked frame_drop second", j)
	}
}

// Journal order must equal effect order: a mixed clear reports request then
// frame, and a control change issued right after lands after the request
// entry. The deferred frame entry may fall anywhere after the clear's request.
func TestJournalOrderFollowsEffectOrderAcrossBackToBackSets(t *testing.T) {
	spec := minimalSpec()
	spec.Cameras[0].Faults = []FaultSpec{
		{Kind: "frame_drop", Rate: 0.5},
		{Kind: "rtsp_error", On: "describe", Status: 503, Rate: 1},
	}
	f := StartT(t, spec)
	cam, _ := f.Camera("front-door")
	if err := cam.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if err := cam.Inject(FaultSpec{Kind: "rtsp_error", On: "play", Status: 454, Rate: 1}); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	waitUntil(t, "journal", func() bool { return len(f.Journal()) >= 3 })

	j := f.Journal()
	if len(j) != 3 {
		t.Fatalf("journal has %d entries, want 3: %+v", len(j), j)
	}
	for i := 1; i < len(j); i++ {
		if j[i].Seq <= j[i-1].Seq {
			t.Fatalf("journal seq not increasing: %+v", j)
		}
	}
	if j[0].Action != "clear" || j[0].Clock != "request" {
		t.Errorf("entry 0 = %+v, want clear/request first", j[0])
	}
	var sawFrameClear, sawInject bool
	for _, e := range j[1:] {
		switch {
		case e.Action == "clear" && e.Clock == "frame":
			sawFrameClear = true
		case e.Action == "inject" && e.Clock == "request" && e.Fault.Kind == "rtsp_error":
			sawInject = true
		}
	}
	if !sawFrameClear || !sawInject {
		t.Errorf("journal = %+v, want a frame clear and a request inject", j)
	}
	// The request-class inject is effective immediately, so it precedes nothing
	// before the clear's own request entry and must not precede it.
	for i, e := range j {
		if e.Action == "inject" && i == 0 {
			t.Errorf("inject journaled before the clear it followed: %+v", j)
		}
	}
}
