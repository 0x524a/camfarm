package fault_test

import (
	"reflect"
	"testing"

	"github.com/0x524a/camfarm/internal/fault"
	"github.com/0x524a/camfarm/internal/seed"
)

// I1: an omitted status takes the catalogue default.
func TestRTSPErrorOmittedStatusDefaultsTo503(t *testing.T) {
	e := fault.NewControlEngine(seed.Seed(1))
	if err := e.Set([]fault.Spec{{Kind: fault.KindRTSPError, On: "describe", Rate: 1}}); err != nil {
		t.Fatal(err)
	}
	if d := e.Decide(fault.MethodDescribe); d.Error == nil || d.Error.Status != 503 {
		t.Fatalf("decision = %+v, want status 503", d.Error)
	}
}

// M3: an omitted rate on rtsp_error is the catalogue default 0.2 unless
// onlyRequest is set.
func TestRTSPErrorOmittedRateIsTheCatalogueDefault(t *testing.T) {
	e := fault.NewControlEngine(seed.Seed(1))
	_ = e.Set([]fault.Spec{{Kind: fault.KindRTSPError, On: "all", Status: 503}})
	fired := 0
	for i := 0; i < 1000; i++ {
		if e.Decide(fault.MethodPlay).Error != nil {
			fired++
		}
	}
	if fired < 120 || fired > 280 {
		t.Fatalf("omitted rate fired %d of 1000, want about 200", fired)
	}
	_ = e.Set([]fault.Spec{{Kind: fault.KindRTSPError, On: "all", Status: 503, OnlyRequest: 2}})
	var got []int
	for i := 1; i <= 5; i++ {
		if e.Decide(fault.MethodPlay).Error != nil {
			got = append(got, i)
		}
	}
	if !reflect.DeepEqual(got, []int{2}) {
		t.Fatalf("onlyRequest fired on %v, want [2]", got)
	}
}

// M2: on "all" counts across methods with one counter.
func TestRTSPErrorOnAllOnlyRequestCountsAcrossMethods(t *testing.T) {
	e := fault.NewControlEngine(seed.Seed(1))
	_ = e.Set([]fault.Spec{{Kind: fault.KindRTSPError, On: "all", Status: 503, OnlyRequest: 3}})
	var got []int
	for i, m := range []fault.Method{fault.MethodDescribe, fault.MethodSetup, fault.MethodPlay, fault.MethodDescribe, fault.MethodSetup, fault.MethodPlay} {
		if e.Decide(m).Error != nil {
			got = append(got, i+1)
		}
	}
	if !reflect.DeepEqual(got, []int{3}) {
		t.Fatalf("fired on %v, want exactly the 3rd matching request [3]", got)
	}
}

// M1: SetAt reports the request total under the swap's lock.
func TestSetAtReturnsTheRequestTotal(t *testing.T) {
	e := fault.NewControlEngine(seed.Seed(1))
	e.Decide(fault.MethodDescribe)
	e.Decide(fault.MethodSetup)
	n, err := e.SetAt([]fault.Spec{{Kind: fault.KindSDPLie, Mode: "codec"}})
	if err != nil || n != 2 {
		t.Fatalf("SetAt = %d, %v; want 2, nil", n, err)
	}
}

// I3: the teardown draw is taken on every PLAY decision, so an errored PLAY
// does not shift the sequence for later PLAYs.
func TestArmPlayDrawIsIndependentOfRTSPErrors(t *testing.T) {
	td := fault.Spec{Kind: fault.KindTeardown, AfterFrames: 10, Mode: "close", Rate: 0.5}
	run := func(extra ...fault.Spec) []bool {
		e := fault.NewControlEngine(seed.Seed(3))
		_ = e.Set(append([]fault.Spec{td}, extra...))
		var out []bool
		for i := 0; i < 12; i++ {
			d := e.Decide(fault.MethodPlay)
			a := e.ArmPlay() // taken on every PLAY, as OnPlay does
			out = append(out, d.Error == nil && a != nil)
		}
		return out
	}
	alone := run()
	with := run(fault.Spec{Kind: fault.KindRTSPError, On: "play", Status: 454, OnlyRequest: 1})
	if with[0] {
		t.Fatal("errored PLAY armed a session")
	}
	if !reflect.DeepEqual(alone[1:], with[1:]) {
		t.Fatalf("arming for PLAYs 2.. shifted:\n alone %v\n with  %v", alone, with)
	}
}
