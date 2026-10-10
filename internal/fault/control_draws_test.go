package fault

import (
	"testing"

	"github.com/0x524a/camfarm/internal/seed"
)

// A matching request consumes exactly one draw even when OnlyRequest makes the
// draw irrelevant to the outcome, and a non-matching method consumes none. The
// engine's stream is compared with a reference stream advanced by hand.
func TestOnlyRequestStillConsumesOneDrawPerMatchingRequest(t *testing.T) {
	e := NewControlEngine(seed.Seed(3))
	if err := e.Set([]Spec{{Kind: KindRTSPError, On: "describe", Status: 503, Rate: 0.5, OnlyRequest: 2}}); err != nil {
		t.Fatal(err)
	}
	const matching = 5
	for i := 0; i < matching; i++ {
		e.Decide(MethodSetup) // never matches: no draw
		e.Decide(MethodDescribe)
	}
	ref := seed.Seed(3).Stream(string(KindRTSPError)).Rand()
	for i := 0; i < matching; i++ {
		ref.Float64()
	}
	got := e.specs[KindRTSPError].rnd.Float64()
	if want := ref.Float64(); got != want {
		t.Fatalf("next draw = %v, want %v: the stream did not advance once per matching request", got, want)
	}
}
