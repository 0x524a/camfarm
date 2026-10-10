package fault_test

import (
	"reflect"
	"testing"

	"github.com/0x524a/camfarm/internal/fault"
	"github.com/0x524a/camfarm/internal/seed"
)

func TestHoldBoundariesAreSeededAndExactlyHoldFramesLong(t *testing.T) {
	run := func() []fault.HoldDecision {
		e, err := fault.New(seed.Seed(11), []fault.Spec{{Kind: fault.KindBurstDelivery, Rate: 0.1, HoldFrames: 4}})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		var out []fault.HoldDecision
		for k := 0; k < 400; k++ {
			out = append(out, e.DecideHold(k))
		}
		return out
	}
	a, b := run(), run()
	if !reflect.DeepEqual(a, b) {
		t.Fatal("not reproducible")
	}
	held, flushes := 0, 0
	for _, d := range a {
		switch d {
		case fault.HoldHeld:
			held++
		case fault.HoldFlush:
			if held != 3 {
				t.Fatalf("flush after %d held frames, want 3 (HoldFrames-1)", held)
			}
			held = 0
			flushes++
		case fault.HoldNone:
			if held != 0 {
				t.Fatalf("a hold was interrupted after %d held frames", held)
			}
		}
	}
	if held > 3 {
		t.Fatalf("unterminated hold of %d", held)
	}
	if flushes == 0 {
		t.Fatal("no hold in 400 frames at rate 0.1; the test proves nothing")
	}
}

func TestDrawsAreConsumedDuringAHold(t *testing.T) {
	const n = 400
	drops := func(h int) []bool {
		e, err := fault.New(seed.Seed(11), []fault.Spec{
			{Kind: fault.KindBurstDelivery, Rate: 0.2, HoldFrames: h},
			{Kind: fault.KindFrameDrop, Rate: 0.5},
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		var out []bool
		for k := 0; k < n; k++ {
			e.DecideHold(k)
			out = append(out, len(e.DecideFrame(k, false)) > 0)
		}
		return out
	}
	if !reflect.DeepEqual(drops(2), drops(9)) {
		t.Fatal("frame_drop decisions depend on the hold length")
	}

	// The burst stream itself: exactly one draw per frame, hold or not. A hold
	// starts exactly where the draw is below Rate and no hold is running.
	const rate, h = 0.2, 5
	e, err := fault.New(seed.Seed(11), []fault.Spec{{Kind: fault.KindBurstDelivery, Rate: rate, HoldFrames: h}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rnd := seed.Seed(11).Stream("burst_delivery").Rand()
	remaining := 0
	for k := 0; k < n; k++ {
		u := rnd.Float64()
		want := fault.HoldNone
		if remaining == 0 && u < rate {
			remaining = h
		}
		if remaining > 0 {
			if remaining > 1 {
				want = fault.HoldHeld
			} else {
				want = fault.HoldFlush
			}
			remaining--
		}
		if got := e.DecideHold(k); got != want {
			t.Fatalf("frame %d: %v, want %v (one unconditional draw per frame)", k, got, want)
		}
	}
}

func TestHoldFramesAccessor(t *testing.T) {
	e, _ := fault.New(seed.Seed(1), []fault.Spec{{Kind: fault.KindBurstDelivery, Rate: 1, HoldFrames: 7}})
	if got := e.HoldFrames(); got != 7 {
		t.Fatalf("HoldFrames = %d, want 7", got)
	}
	e, _ = fault.New(seed.Seed(1), nil)
	if got := e.HoldFrames(); got != 0 {
		t.Fatalf("HoldFrames without the fault = %d, want 0", got)
	}
	if got := e.DecideHold(0); got != fault.HoldNone {
		t.Fatalf("DecideHold without the fault = %v", got)
	}
}
