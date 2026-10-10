package fault_test

import (
	"reflect"
	"testing"

	"github.com/0x524a/camfarm/internal/fault"
	"github.com/0x524a/camfarm/internal/seed"
)

func TestReorderSequenceForDepthOne(t *testing.T) {
	e, err := fault.New(seed.Seed(1), []fault.Spec{{Kind: fault.KindPTSDTSMismatch, ReorderDepth: 1}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	const dur = 3000
	var ts []int64
	for k := 0; k < 8; k++ {
		ts = append(ts, int64(k)*dur+e.TimestampShift(k, dur))
	}
	want := []int64{1, 0, 3, 2, 5, 4, 7, 6}
	for i := range want {
		want[i] *= dur
	}
	if !reflect.DeepEqual(ts, want) {
		t.Fatalf("ts = %v, want %v", ts, want)
	}
}

func TestReorderKeepsTheSetOfTimestampsForEveryDepth(t *testing.T) {
	for depth := 1; depth <= 4; depth++ {
		e, err := fault.New(seed.Seed(1), []fault.Spec{{Kind: fault.KindPTSDTSMismatch, ReorderDepth: depth}})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		const dur, n = 3600, 60 // a multiple of every window size 2..5
		got := map[int64]bool{}
		for k := 0; k < n; k++ {
			got[int64(k)*dur+e.TimestampShift(k, dur)] = true
		}
		for k := 0; k < n; k++ {
			if !got[int64(k)*dur] {
				t.Fatalf("depth %d: timestamp %d missing", depth, k)
			}
		}
	}
}

func TestDelayShiftsConstantlyAndDriftGrowsPerFrame(t *testing.T) {
	e, err := fault.New(seed.Seed(1), []fault.Spec{{Kind: fault.KindPTSDelayed, DelayMs: 100, DriftPpm: 1000}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	const dur = 3000
	if got := e.TimestampShift(0, dur); got != -9000 { // 100 ms * 90 ticks/ms, n=0 -> no drift yet
		t.Fatalf("frame 0 shift = %d, want -9000", got)
	}
	// n=1000 -> 1000*3000*1000/1e6 = 3000 ticks of extra lag.
	var last int64
	for k := 1; k <= 1000; k++ {
		last = e.TimestampShift(k, dur)
	}
	if last != -9000-3000 {
		t.Fatalf("frame 1000 shift = %d, want -12000", last)
	}
}

func TestTimestampFaultsUseNoRandomnessAndComposeByAdding(t *testing.T) {
	both, err := fault.New(seed.Seed(5), []fault.Spec{
		{Kind: fault.KindPTSDTSMismatch, ReorderDepth: 1},
		{Kind: fault.KindPTSDelayed, DelayMs: 10},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := both.TimestampShift(0, 3000); got != 3000-900 {
		t.Fatalf("composed shift = %d, want 2100", got)
	}
}

func TestTimestampShiftIsZeroWithoutTimestampFaults(t *testing.T) {
	e, err := fault.New(seed.Seed(1), []fault.Spec{{Kind: fault.KindFrameDrop, Rate: 0.5}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for k := 0; k < 5; k++ {
		if got := e.TimestampShift(k, 3000); got != 0 {
			t.Fatalf("frame %d shift = %d, want 0", k, got)
		}
	}
}

// Drift is counted in calls to TimestampShift since the entry was installed,
// not in the frame index it is handed, so a fault installed mid-run starts at
// zero lag (ruling R2).
func TestDriftCountsCallsNotFrameIndex(t *testing.T) {
	e, err := fault.New(seed.Seed(1), []fault.Spec{{Kind: fault.KindPTSDelayed, DriftPpm: 1000}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := e.TimestampShift(5000, 3000); got != 0 {
		t.Fatalf("first call shift = %d, want 0", got)
	}
}

// Inherit carries pts_delayed's drift clock only to an identical spec; a
// changed spec, or one re-added after a swap without it, starts at zero.
func TestInheritCarriesTheDriftClockOnlyForAnUnchangedSpec(t *testing.T) {
	drift := fault.Spec{Kind: fault.KindPTSDelayed, DriftPpm: 100000} // 300 ticks per 3000-tick frame
	newEngine := func(specs ...fault.Spec) *fault.Engine {
		t.Helper()
		e, err := fault.New(seed.Seed(1), specs)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		return e
	}
	old := newEngine(drift)
	for k := 0; k < 4; k++ {
		old.TimestampShift(k, 3000)
	}

	same := newEngine(fault.Spec{Kind: fault.KindJitter, MaxMs: 5}, drift)
	same.Inherit(old)
	if got := same.TimestampShift(4, 3000); got != -4*300 {
		t.Fatalf("unchanged spec after the swap: shift %d, want %d (drift continues)", got, -4*300)
	}

	changed := newEngine(fault.Spec{Kind: fault.KindPTSDelayed, DriftPpm: 50000})
	changed.Inherit(old)
	if got := changed.TimestampShift(4, 3000); got != 0 {
		t.Fatalf("changed spec after the swap: shift %d, want 0 (drift restarts)", got)
	}

	// Removed, then re-added: the engine in between has no entry to carry from.
	without := newEngine(fault.Spec{Kind: fault.KindJitter, MaxMs: 5})
	without.Inherit(old)
	readded := newEngine(drift)
	readded.Inherit(without)
	if got := readded.TimestampShift(4, 3000); got != 0 {
		t.Fatalf("re-added spec: shift %d, want 0 (drift restarts)", got)
	}

	newEngine(drift).Inherit(nil) // a nil prev is a no-op
}
