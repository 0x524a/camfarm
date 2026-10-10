package rtsp

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/description"

	"github.com/0x524a/camfarm/internal/clock"
	"github.com/0x524a/camfarm/internal/fault"
	"github.com/0x524a/camfarm/internal/obs"
	"github.com/0x524a/camfarm/internal/seed"
)

const burstDur = 3000

// holdStartingAt finds a root seed whose burst_delivery stream, as the pump
// derives it, starts a hold of h frames at exactly the given frame and none
// before it.
func holdStartingAt(t *testing.T, start, h int, rate float64) seed.Seed {
	t.Helper()
	for n := 1; n < 20000; n++ {
		s := seed.Seed(n)
		e, err := fault.New(s.Stream("fault"), []fault.Spec{{Kind: fault.KindBurstDelivery, Rate: rate, HoldFrames: h}})
		if err != nil {
			t.Fatal(err)
		}
		ok := true
		for k := 0; k <= start+h-1 && ok; k++ {
			d := e.DecideHold(k)
			switch {
			case k < start:
				ok = d == fault.HoldNone
			case k < start+h-1:
				ok = d == fault.HoldHeld
			default:
				ok = d == fault.HoldFlush
			}
		}
		if ok {
			return s
		}
	}
	t.Fatalf("no seed holds frames %d-%d at rate %v", start, start+3, rate)
	return 0
}

func burstPump(t *testing.T, s seed.Seed, faults []fault.Spec, rec *obs.Recorder, after func()) (*Pump, *recorder) {
	t.Helper()
	w := &recorder{}
	p, err := NewPump(PumpConfig{
		CameraID: "cam", Media: regularMedia(t, burstDur, 30), Medi: &description.Media{Type: description.MediaTypeVideo},
		Writer: w, Seed: s, Faults: faults, Obs: rec, AfterFrame: after,
	})
	if err != nil {
		t.Fatalf("NewPump: %v", err)
	}
	return p, w
}

func stepN(t *testing.T, p *Pump, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if err := p.Step(); err != nil {
			t.Fatalf("Step: %v", err)
		}
	}
}

// distinctTimestamps collapses packets to one timestamp per access unit.
func distinctTimestamps(w *recorder, from int) []uint32 {
	var ts []uint32
	for _, pkt := range w.pkts[from:] {
		if len(ts) == 0 || ts[len(ts)-1] != pkt.Timestamp {
			ts = append(ts, pkt.Timestamp)
		}
	}
	return ts
}

func requireContiguousSequence(t *testing.T, w *recorder) {
	t.Helper()
	for i := 1; i < len(w.pkts); i++ {
		if w.pkts[i].SequenceNumber != w.pkts[i-1].SequenceNumber+1 {
			t.Fatalf("sequence gap at packet %d: %d then %d", i, w.pkts[i-1].SequenceNumber, w.pkts[i].SequenceNumber)
		}
	}
}

func TestBurstHoldsThenFlushesBackToBack(t *testing.T) {
	spec := []fault.Spec{{Kind: fault.KindBurstDelivery, Rate: 0.3, HoldFrames: 4}}
	s := holdStartingAt(t, 5, 4, 0.3)
	rec := obs.New(100)
	after := 0
	p, w := burstPump(t, s, spec, rec, func() { after++ })

	stepN(t, p, 5)
	before := len(w.pkts)
	if before == 0 {
		t.Fatal("nothing written before the hold")
	}
	// obs.Frame is called when a frame is emitted, so held frames count at
	// flush, not when they are held.
	for i := 5; i < 8; i++ {
		stepN(t, p, 1)
		if len(w.pkts) != before {
			t.Fatalf("step %d wrote packets while holding", i)
		}
		if got := rec.Counters("cam").FramesServed; got != 5 {
			t.Fatalf("step %d: FramesServed = %d, want 5 (held frames are not yet emitted)", i, got)
		}
	}
	stepN(t, p, 1)
	if got := distinctTimestamps(w, before); len(got) != 4 || got[0] != 5*burstDur || got[3] != 8*burstDur {
		t.Fatalf("flushed timestamps = %v, want frames 5..8 stamped as if undelayed", got)
	}
	if got := rec.Counters("cam").FramesServed; got != 9 {
		t.Fatalf("FramesServed = %d after flush, want 9", got)
	}
	requireContiguousSequence(t, w)
	if after != 9 {
		t.Fatalf("AfterFrame called %d times over 9 steps, holding steps included", after)
	}

	var held, released []string
	for _, ev := range rec.Events() {
		if ev.Kind != "burst_delivery" {
			t.Fatalf("unexpected event %+v", ev)
		}
		if ev.Detail == "released 4 frames" {
			released = append(released, ev.Detail)
		} else {
			held = append(held, ev.Detail)
		}
	}
	wantHeld := []string{"held (1 of 4)", "held (2 of 4)", "held (3 of 4)"}
	if fmt.Sprint(held) != fmt.Sprint(wantHeld) || len(released) != 1 {
		t.Fatalf("events held=%v released=%v", held, released)
	}
	if k := rec.Counters("cam").ByKind["burst_delivery"]; k.Applied != 3 {
		t.Fatalf("tally = %+v, want 3 applied (frames held)", k)
	}
}

func TestClearWhileHoldingFlushesAtTheNextStep(t *testing.T) {
	s := holdStartingAt(t, 5, 4, 0.3)
	rec := obs.New(100)
	p, w := burstPump(t, s, []fault.Spec{{Kind: fault.KindBurstDelivery, Rate: 0.3, HoldFrames: 4}}, rec, nil)
	stepN(t, p, 7) // frames 5 and 6 are held
	before := len(w.pkts)
	if err := p.SetFaults(nil, nil); err != nil {
		t.Fatal(err)
	}
	stepN(t, p, 1)
	got := distinctTimestamps(w, before)
	if len(got) != 3 || got[0] != 5*burstDur || got[2] != 7*burstDur {
		t.Fatalf("timestamps after clear = %v, want held frames 5,6 then 7", got)
	}
	requireContiguousSequence(t, w)
	stepN(t, p, 3)
	requireContiguousSequence(t, w)
	if got := rec.Counters("cam").FramesServed; got != 11 {
		t.Fatalf("FramesServed = %d, want 11: no frame lost", got)
	}
	if ev := rec.Events(); ev[len(ev)-1].Detail != "released 2 frames" {
		t.Fatalf("last event %+v, want released 2 frames", ev[len(ev)-1])
	}
}

func TestReplacingBurstWithAFreshEngineFlushesTheOldHold(t *testing.T) {
	s := holdStartingAt(t, 5, 4, 0.3)
	spec := []fault.Spec{{Kind: fault.KindBurstDelivery, Rate: 0.3, HoldFrames: 4}}
	p, w := burstPump(t, s, spec, obs.New(100), nil)
	stepN(t, p, 7)
	before := len(w.pkts)
	// The new engine starts with no hold running, so the old one cannot
	// continue; its frames are released rather than left to a later burst.
	if err := p.SetFaults([]fault.Spec{{Kind: fault.KindBurstDelivery, Rate: 0, HoldFrames: 9}}, nil); err != nil {
		t.Fatal(err)
	}
	stepN(t, p, 1)
	if got := distinctTimestamps(w, before); len(got) != 3 {
		t.Fatalf("timestamps = %v, want held frames 5,6 then 7", got)
	}
	requireContiguousSequence(t, w)
}

func TestHoldSpanningTheLoopSeamKeepsTimestampsContinuing(t *testing.T) {
	rec := obs.New(100)
	p, w := burstPump(t, seed.Seed(1), nil, rec, nil)
	n := len(p.aus)
	stepN(t, p, n-2)
	// Rate 1 starts a hold at once: frames n-2, n-1 and n held, n+1 flushes.
	if err := p.SetFaults([]fault.Spec{{Kind: fault.KindBurstDelivery, Rate: 1, HoldFrames: 4}}, nil); err != nil {
		t.Fatal(err)
	}
	before := len(w.pkts)
	stepN(t, p, 3)
	if len(w.pkts) != before {
		t.Fatal("wrote while holding")
	}
	stepN(t, p, 1)
	if p.Loops() != 1 {
		t.Fatalf("Loops = %d, want the hold to span the seam", p.Loops())
	}
	got := distinctTimestamps(w, before)
	if len(got) != 4 {
		t.Fatalf("flushed %d frames, want 4", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i]-got[i-1] != burstDur {
			t.Fatalf("timestamps %v are not strictly continuing across the seam", got)
		}
	}
	requireContiguousSequence(t, w)
}

// Frames still held when a pump stops are discarded: they were never emitted,
// so they were never counted, and nothing writes them later.
func TestShutdownWithFramesHeldDiscardsThemQuietly(t *testing.T) {
	p, w := burstPump(t, seed.Seed(1), []fault.Spec{{Kind: fault.KindBurstDelivery, Rate: 1, HoldFrames: 5}}, obs.New(100), nil)
	stepN(t, p, 3)
	if len(p.held) != 3 || len(w.pkts) != 0 {
		t.Fatalf("held %d frames, wrote %d packets", len(p.held), len(w.pkts))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() { p.run(ctx, clock.Real{}, nil); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return on cancel with frames held")
	}
	if len(w.pkts) != 0 {
		t.Fatalf("shutdown wrote %d held packets", len(w.pkts))
	}
}

func TestBurstWithFrameDropStillAdvancesAndReleases(t *testing.T) {
	// Every frame is dropped, so nothing is ever buffered, but the hold clock
	// still runs and nothing is written or counted.
	specs := []fault.Spec{{Kind: fault.KindBurstDelivery, Rate: 1, HoldFrames: 3}, {Kind: fault.KindFrameDrop, Rate: 1}}
	rec := obs.New(100)
	p, w := burstPump(t, seed.Seed(1), specs, rec, nil)
	stepN(t, p, 6)
	if len(w.pkts) != 0 || rec.Counters("cam").FramesServed != 0 || len(p.held) != 0 {
		t.Fatalf("dropped frames were emitted or buffered: %d packets, %d held", len(w.pkts), len(p.held))
	}
}
