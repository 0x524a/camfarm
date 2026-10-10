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
}
