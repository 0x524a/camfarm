package fault_test

import (
	"testing"
	"time"

	"github.com/0x524a/camfarm/internal/fault"
	"github.com/0x524a/camfarm/internal/seed"
)

func TestJitterDelaysAreTheSeededSchedule(t *testing.T) {
	spec := []fault.Spec{{Kind: fault.KindJitter, MaxMs: 200}}
	a, err := fault.New(seed.Seed(21), spec)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := fault.New(seed.Seed(21), spec)
	rnd := seed.Seed(21).Stream("jitter").Rand()
	for k := 0; k < 200; k++ {
		da, db := a.FrameDelay(k), b.FrameDelay(k)
		want := time.Duration(rnd.Float64() * 200 * float64(time.Millisecond)).Truncate(time.Microsecond)
		if da != db || da != want {
			t.Fatalf("frame %d: %v, %v, want %v", k, da, db, want)
		}
		if da < 0 || da >= 200*time.Millisecond {
			t.Fatalf("frame %d delay %v out of range", k, da)
		}
	}
}

func TestFrameDelayIsZeroWithoutJitter(t *testing.T) {
	e, _ := fault.New(seed.Seed(1), []fault.Spec{{Kind: fault.KindFrameDrop, Rate: 0.5}})
	if d := e.FrameDelay(0); d != 0 {
		t.Fatalf("delay = %v, want 0", d)
	}
}
