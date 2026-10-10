package rtsp

import (
	"runtime"
	"strings"
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
		{Kind: fault.KindBurstDelivery, Rate: 0.5, HoldFrames: 12},
		{Kind: fault.KindPTSDTSMismatch, ReorderDepth: 2},
		{Kind: fault.KindPTSDelayed, DelayMs: 20, DriftPpm: 100},
		{Kind: fault.KindPacketLoss, Rate: 0.2},
	}

	rec := obs.New(100000)
	p, w := burstPump(t, seed.Seed(7), full, rec, nil)

	req := make(chan []fault.Spec)
	ack := make(chan error)
	go func() {
		for specs := range req {
			ack <- p.SetFaults(specs, nil)
		}
	}()
	defer close(req)

	swapsWhileHeld := 0
	for i := 1; i <= steps; i++ {
		if i%50 == 0 {
			if len(p.held) > 0 { // stepper-owned, so race-free to read here
				swapsWhileHeld++
			}
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
	if swapsWhileHeld == 0 {
		t.Fatal("no swap ever found frames held: the flush-on-swap path was not exercised")
	}
	released := 0
	for _, ev := range rec.Events() {
		if ev.Kind == string(fault.KindBurstDelivery) && strings.HasPrefix(ev.Detail, "released") {
			released++
		}
	}
	if released == 0 {
		t.Fatal("burst_delivery never released a burst")
	}
	if rec.Dropped() != 0 {
		t.Fatalf("event log overflowed (%d dropped); released count is unreliable", rec.Dropped())
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

// TestTimingComposeFreeRunningChurn is the unsynchronised variant: a goroutine
// swaps fault sets in a free-running loop, with no hand-off, while the stepper
// runs 2,000 frames. Where a swap lands relative to a Step is up to the
// scheduler, so only interleaving-independent invariants are asserted: no
// panic or error, sequence numbers increasing (wrap-aware), and every step's
// frame accounted for as served, held or dropped.
func TestTimingComposeFreeRunningChurn(t *testing.T) {
	const steps = 2000
	full := []fault.Spec{
		{Kind: fault.KindJitter, MaxMs: 50},
		{Kind: fault.KindBurstDelivery, Rate: 0.2, HoldFrames: 5},
		{Kind: fault.KindPTSDTSMismatch, ReorderDepth: 2},
		{Kind: fault.KindPTSDelayed, DelayMs: 20, DriftPpm: 100},
		{Kind: fault.KindPacketLoss, Rate: 0.2},
		{Kind: fault.KindFrameDrop, Rate: 0.1},
	}
	rec := obs.New(10)
	p, w := burstPump(t, seed.Seed(11), full, rec, nil)

	done := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		for i := 0; ; i++ {
			select {
			case <-done:
				return
			default:
			}
			specs := full
			if i%2 == 1 {
				specs = nil
			}
			if err := p.SetFaults(specs, nil); err != nil {
				t.Errorf("SetFaults: %v", err)
				return
			}
			runtime.Gosched()
		}
	}()

	for i := 1; i <= steps; i++ {
		if err := p.Step(); err != nil {
			close(done)
			<-exited
			t.Fatalf("Step %d: %v", i, err)
		}
	}
	close(done)
	<-exited

	c := rec.Counters("cam")
	dropped := int(c.ByKind[string(fault.KindFrameDrop)].Applied)
	served := int(c.FramesServed)
	if served+len(p.held)+dropped != steps {
		t.Errorf("served %d + held %d + dropped %d != %d steps", served, len(p.held), dropped, steps)
	}
	for i := 1; i < len(w.pkts); i++ {
		d := w.pkts[i].SequenceNumber - w.pkts[i-1].SequenceNumber
		if d == 0 || d >= 0x8000 {
			t.Fatalf("packet %d: sequence %d after %d (not increasing)", i, w.pkts[i].SequenceNumber, w.pkts[i-1].SequenceNumber)
		}
	}
}
