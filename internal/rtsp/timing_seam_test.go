package rtsp

import (
	"testing"

	"github.com/0x524a/camfarm/internal/fault"
	"github.com/0x524a/camfarm/internal/obs"
)

// Timestamp faults key on the monotonic frame counter and the loop seam carries
// the timestamp forward by one frame, so a shift must continue unbroken across
// the seam, however many loops the source has made.
func TestTimestampShiftContinuesAcrossTheLoopSeam(t *testing.T) {
	const dur = 3000
	cases := []struct {
		name  string
		spec  fault.Spec
		shift func(k int) int64
	}{
		{"reorderDepth 1", fault.Spec{Kind: fault.KindPTSDTSMismatch, ReorderDepth: 1}, func(k int) int64 {
			if k%2 == 0 {
				return dur
			}
			return -dur
		}},
		{"reorderDepth 2", fault.Spec{Kind: fault.KindPTSDTSMismatch, ReorderDepth: 2}, func(k int) int64 {
			if k%3 == 0 {
				return 2 * dur
			}
			return -dur
		}},
		// pts_delayed, a constant 10 ms lag: 900 ticks behind, wrapping below zero.
		{"negative constant shift", fault.Spec{Kind: fault.KindPTSDelayed, DelayMs: 10}, func(int) int64 { return -900 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := regularMedia(t, dur, 30)
			n := 2*len(m.AUs) + 3 // past two loop seams
			p, w := pumpWithMedia(t, m, []fault.Spec{tc.spec}, obs.New(10))
			got := frameTimestamps(t, p, w, n)
			if len(got) != n {
				t.Fatalf("%d frames written, want %d", len(got), n)
			}
			if p.Loops() < 2 {
				t.Fatalf("only %d loops; the test must cross the seam", p.Loops())
			}
			for k, ts := range got {
				want := uint32(int64(k)*dur + tc.shift(k))
				if ts != want {
					t.Fatalf("frame %d (loop %d): timestamp %d, want %d", k, k/len(m.AUs), ts, want)
				}
			}
		})
	}
}

// A fault-set swap that changes the pts_delayed spec installs a new entry, so its
// drift clock counts from the swap, not from frame zero: the frame index the
// drift is measured against restarts at zero while the pump's frame counter and
// the nominal timeline carry on. (An unchanged spec keeps its clock: see
// TestUnrelatedSwapLeavesTheDriftContinuous.)
func TestMidStreamSwapRestartsTheDriftClock(t *testing.T) {
	const dur = 3000
	drift := []fault.Spec{{Kind: fault.KindPTSDelayed, DriftPpm: 100000}} // 300 ticks per frame
	m := regularMedia(t, dur, 30)
	p, w := pumpWithMedia(t, m, drift, obs.New(10))

	first := frameTimestamps(t, p, w, 5)
	for k, ts := range first {
		if want := uint32(int64(k)*dur - int64(k)*300); ts != want {
			t.Fatalf("frame %d before the swap: timestamp %d, want %d", k, ts, want)
		}
	}

	var appliedAt = -1
	faster := []fault.Spec{{Kind: fault.KindPTSDelayed, DriftPpm: 50000}} // a changed spec: 150 ticks per frame
	if err := p.SetFaults(faster, func(i int) { appliedAt = i }); err != nil {
		t.Fatalf("SetFaults: %v", err)
	}
	second := frameTimestamps(t, p, w, 3)
	if appliedAt != 5 {
		t.Fatalf("swap applied at frame %d, want 5", appliedAt)
	}
	for j, ts := range second {
		k := 5 + j
		if want := uint32(int64(k)*dur - int64(j)*150); ts != want {
			t.Fatalf("frame %d after the swap: timestamp %d, want %d (drift restarts at the swap)", k, ts, want)
		}
	}
}

// Injecting or clearing an unrelated frame fault rebuilds the pump's whole
// engine, but an unchanged pts_delayed keeps its drift clock, so the timestamp
// sequence is identical to an uninterrupted run. Without the carry-over the
// drift would snap back to zero at each swap and the timestamps jump forward.
func TestUnrelatedSwapLeavesTheDriftContinuous(t *testing.T) {
	const (
		dur    = 3000
		frames = 40
	)
	drift := fault.Spec{Kind: fault.KindPTSDelayed, DelayMs: 20, DriftPpm: 100000}
	m := regularMedia(t, dur, 30)

	base, bw := pumpWithMedia(t, m, []fault.Spec{drift}, obs.New(10))
	want := frameTimestamps(t, base, bw, frames)

	p, w := pumpWithMedia(t, m, []fault.Spec{drift}, obs.New(10))
	churn := map[int][][]fault.Spec{
		10: {{drift, {Kind: fault.KindJitter, MaxMs: 50}}},
		20: {{{Kind: fault.KindFrameDrop, Rate: 0}, drift}},
		// Two swaps queued between one pair of frames chain the carry-over.
		30: {{drift, {Kind: fault.KindJitter, MaxMs: 10}}, {drift}},
	}
	var got []uint32
	for k := 0; k < frames; k++ {
		for _, specs := range churn[k] {
			if err := p.SetFaults(specs, nil); err != nil {
				t.Fatalf("SetFaults at frame %d: %v", k, err)
			}
		}
		got = append(got, frameTimestamps(t, p, w, 1)...)
	}
	if len(got) != frames {
		t.Fatalf("%d frames written, want %d", len(got), frames)
	}
	for k := range want {
		if got[k] != want[k] {
			t.Fatalf("frame %d: timestamp %d under churn, %d uninterrupted (drift not continuous)", k, got[k], want[k])
		}
	}
}
