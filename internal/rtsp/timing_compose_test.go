package rtsp

import (
	"testing"

	"github.com/0x524a/camfarm/internal/fault"
	"github.com/0x524a/camfarm/internal/obs"
	"github.com/0x524a/camfarm/internal/seed"
)

// TestTimingFaultsComposeUnderChurn runs all four timing faults plus
// packet_loss on one pump for 2,000 steps while a second goroutine injects and
// clears the whole set every 50 steps. The hand-off is lock-stepped (the stepper
// waits for each swap to be queued) so the run is deterministic, while the swap
// still crosses goroutines for the race detector.
//
// The pump is stepped directly: Step plans and emits at once, so no clock is
// needed (jitter's delay is a pacing concern of run, covered in jitter_test.go).
//
// Invariant asserted: no frame is dropped in this set, so every Step's frame is
// either emitted or sitting in the hold buffer, i.e. FramesServed + len(held) ==
// steps. A swap that finds frames held releases them (Task 4 design), so
// nothing is discarded mid-run; only frames still held when the pump stops are
// discarded, and they are accounted for as "currently held".
func TestTimingFaultsComposeUnderChurn(t *testing.T) {
	const steps = 2000
	full := []fault.Spec{
		{Kind: fault.KindJitter, MaxMs: 50},
		{Kind: fault.KindBurstDelivery, Rate: 0.2, HoldFrames: 5},
		{Kind: fault.KindPTSDTSMismatch, ReorderDepth: 2},
		{Kind: fault.KindPTSDelayed, DelayMs: 20, DriftPpm: 100},
		{Kind: fault.KindPacketLoss, Rate: 0.2},
	}

	rec := obs.New(100)
	p, w := burstPump(t, seed.Seed(7), full, rec, nil)

	req := make(chan []fault.Spec)
	ack := make(chan error)
	go func() {
		for specs := range req {
			ack <- p.SetFaults(specs, nil)
		}
	}()
	defer close(req)

	for i := 1; i <= steps; i++ {
		if i%50 == 0 {
			next := full
			if (i/50)%2 == 1 {
				next = nil
			}
			req <- next
			if err := <-ack; err != nil {
				t.Fatalf("SetFaults at step %d: %v", i, err)
			}
		}
		if err := p.Step(); err != nil {
			t.Fatalf("Step %d: %v", i, err)
		}
	}

	served := int(rec.Counters("cam").FramesServed)
	if served+len(p.held) != steps {
		t.Errorf("served %d + held %d != %d steps", served, len(p.held), steps)
	}
	if served == 0 || len(w.pkts) == 0 {
		t.Fatal("nothing was written")
	}

	// Sequence numbers are strictly increasing (wraparound-aware); the only
	// gaps are packet_loss's withheld packets, and a gap is never a reversal.
	gaps := 0
	for i := 1; i < len(w.pkts); i++ {
		d := w.pkts[i].SequenceNumber - w.pkts[i-1].SequenceNumber
		if d == 0 || d >= 0x8000 {
			t.Fatalf("packet %d: sequence %d after %d (not increasing)", i, w.pkts[i].SequenceNumber, w.pkts[i-1].SequenceNumber)
		}
		if d > 1 {
			gaps++
		}
	}
	if gaps == 0 {
		t.Error("packet_loss produced no sequence gap in 2,000 frames")
	}
}
