package fault_test

import (
	"reflect"
	"sync"
	"testing"

	"github.com/0x524a/camfarm/internal/fault"
	"github.com/0x524a/camfarm/internal/seed"
)

func TestRTSPErrorOnlyRequestFiresExactlyThere(t *testing.T) {
	e := fault.NewControlEngine(seed.Seed(1))
	if err := e.Set([]fault.Spec{{Kind: fault.KindRTSPError, On: "describe", Status: 503, OnlyRequest: 3}}); err != nil {
		t.Fatal(err)
	}
	var fired []int
	for i := 1; i <= 6; i++ {
		if d := e.Decide(fault.MethodDescribe); d.Error != nil {
			fired = append(fired, i)
		}
	}
	if !reflect.DeepEqual(fired, []int{3}) {
		t.Fatalf("fired on %v, want [3]", fired)
	}
}

func TestRTSPErrorOtherMethodsDoNotAdvanceTheCounter(t *testing.T) {
	e := fault.NewControlEngine(seed.Seed(1))
	_ = e.Set([]fault.Spec{{Kind: fault.KindRTSPError, On: "describe", Status: 454, OnlyRequest: 2}})
	e.Decide(fault.MethodSetup)
	e.Decide(fault.MethodPlay)
	if e.Decide(fault.MethodDescribe).Error != nil {
		t.Fatal("first DESCRIBE fired")
	}
	if d := e.Decide(fault.MethodDescribe); d.Error == nil || d.Error.Status != 454 {
		t.Fatalf("second DESCRIBE = %+v, want status 454", d.Error)
	}
}

func TestRTSPErrorRateZeroNeverFiresAndSeedIsReproducible(t *testing.T) {
	run := func() []bool {
		e := fault.NewControlEngine(seed.Seed(7))
		_ = e.Set([]fault.Spec{{Kind: fault.KindRTSPError, On: "all", Status: 503, Rate: 0.5}})
		var out []bool
		for i := 0; i < 64; i++ {
			out = append(out, e.Decide(fault.MethodDescribe).Error != nil)
		}
		return out
	}
	if !reflect.DeepEqual(run(), run()) {
		t.Fatal("same seed produced different sequences")
	}
	e := fault.NewControlEngine(seed.Seed(7))
	_ = e.Set([]fault.Spec{{Kind: fault.KindRTSPError, On: "all", Status: 503}})
	for i := 0; i < 100; i++ {
		if e.Decide(fault.MethodPlay).Error != nil {
			t.Fatal("rate 0 fired")
		}
	}
}

func TestDrawsAreUnconditionalPerMatchingRequest(t *testing.T) {
	// A non-matching method must not consume draws: the sequence at the
	// matching method is identical with and without interleaved SETUPs.
	a := fault.NewControlEngine(seed.Seed(3))
	b := fault.NewControlEngine(seed.Seed(3))
	spec := []fault.Spec{{Kind: fault.KindRTSPError, On: "describe", Status: 503, Rate: 0.5}}
	_ = a.Set(spec)
	_ = b.Set(spec)
	for i := 0; i < 50; i++ {
		b.Decide(fault.MethodSetup)
		if (a.Decide(fault.MethodDescribe).Error != nil) != (b.Decide(fault.MethodDescribe).Error != nil) {
			t.Fatalf("diverged at %d", i)
		}
	}
}

func TestSetResetsStreamsAndEngineIsRaceFree(t *testing.T) {
	e := fault.NewControlEngine(seed.Seed(1))
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				e.Decide(fault.MethodDescribe)
				_ = e.Set([]fault.Spec{{Kind: fault.KindSDPLie, Mode: "codec"}})
				e.ArmPlay()
			}
		}()
	}
	wg.Wait()
}

func TestSetRestartsSequenceAndCountsRequests(t *testing.T) {
	e := fault.NewControlEngine(seed.Seed(5))
	spec := []fault.Spec{{Kind: fault.KindRTSPError, On: "all", Status: 503, Rate: 0.5}}
	_ = e.Set(spec)
	var first []bool
	for i := 0; i < 20; i++ {
		first = append(first, e.Decide(fault.MethodDescribe).Error != nil)
	}
	if e.Requests() != 20 {
		t.Fatalf("Requests = %d, want 20", e.Requests())
	}
	_ = e.Set(spec)
	for i, want := range first {
		if got := e.Decide(fault.MethodDescribe).Error != nil; got != want {
			t.Fatalf("sequence did not restart at %d", i)
		}
	}
}

func TestSetRefusesInvalidAndIgnoresNonControlKinds(t *testing.T) {
	e := fault.NewControlEngine(seed.Seed(1))
	if err := e.Set([]fault.Spec{{Kind: fault.KindRTSPError, Rate: 2}}); err == nil {
		t.Fatal("rate 2 accepted")
	}
	if err := e.Set([]fault.Spec{{Kind: fault.KindFrameDrop, Rate: 1}}); err != nil {
		t.Fatal(err)
	}
	if d := e.Decide(fault.MethodDescribe); d.Error != nil || d.Lie != nil {
		t.Fatalf("decision = %+v", d)
	}
}

func TestSDPLieFiresAndErrorWins(t *testing.T) {
	e := fault.NewControlEngine(seed.Seed(1))
	_ = e.Set([]fault.Spec{{Kind: fault.KindSDPLie, Mode: "sprop"}})
	if d := e.Decide(fault.MethodSetup); d.Lie != nil {
		t.Fatal("lie on SETUP")
	}
	if d := e.Decide(fault.MethodDescribe); d.Lie == nil || d.Lie.Mode != "sprop" {
		t.Fatalf("lie = %+v, want sprop (unset rate means 1)", d.Lie)
	}
	_ = e.Set([]fault.Spec{
		{Kind: fault.KindSDPLie, Mode: "codec"},
		{Kind: fault.KindRTSPError, On: "describe", Status: 503, OnlyRequest: 1},
	})
	if d := e.Decide(fault.MethodDescribe); d.Error == nil || d.Lie != nil {
		t.Fatalf("decision = %+v, want error to win", d)
	}
}

func TestArmPlay(t *testing.T) {
	e := fault.NewControlEngine(seed.Seed(1))
	if e.ArmPlay() != nil {
		t.Fatal("armed with no teardown spec")
	}
	_ = e.Set([]fault.Spec{{Kind: fault.KindTeardown, AfterFrames: 10, Mode: "reset"}})
	if a := e.ArmPlay(); a == nil || a.AfterFrames != 10 || a.Mode != "reset" {
		t.Fatalf("arming = %+v", a)
	}
	_ = e.Set([]fault.Spec{{Kind: fault.KindTeardown, AfterFrames: 10, Mode: "close", Rate: 0.5}})
	armed := 0
	for i := 0; i < 200; i++ {
		if e.ArmPlay() != nil {
			armed++
		}
	}
	if armed < 60 || armed > 140 {
		t.Fatalf("armed %d of 200 at rate 0.5", armed)
	}
}
