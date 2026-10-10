package fault

import (
	"errors"
	"reflect"
	"testing"
)

func TestTimingKindsAreCatalogued(t *testing.T) {
	want := map[Kind][]string{
		KindJitter:         {"maxMs"},
		KindBurstDelivery:  {"rate", "holdFrames"},
		KindPTSDTSMismatch: {"reorderDepth"},
		KindPTSDelayed:     {"delayMs", "driftPpm"},
	}
	for k, names := range want {
		info, ok := Info(k)
		if !ok || info.Models == "" || info.Clock == "" {
			t.Fatalf("%s: bad catalogue entry %+v", k, info)
		}
		var got []string
		for _, p := range info.Params {
			got = append(got, p.Name)
		}
		if !reflect.DeepEqual(got, names) {
			t.Errorf("%s params = %v, want %v", k, got, names)
		}
	}
}

func TestPTSDelayedParamsAreNotIndividuallyRequired(t *testing.T) {
	info, _ := Info(KindPTSDelayed)
	for _, p := range info.Params {
		if p.Required {
			t.Errorf("%s.%s must not be individually required (at least one of two)", info.Kind, p.Name)
		}
	}
}

// validateTimingTable exercises the timing kinds' parameter rules. A kind the
// catalogue marks Implemented goes through the public Validate; one that is not
// yet still has its rules checked, directly through checkParams.
func validateTimingTable(t *testing.T) {
	t.Helper()
	check := func(s Spec) error {
		info, ok := Info(s.Kind)
		if !ok {
			t.Fatalf("kind %q not catalogued", s.Kind)
		}
		if info.Implemented {
			return Validate(s)
		}
		return checkParams(info, s)
	}
	cases := []struct {
		name string
		spec Spec
		ok   bool
	}{
		{"jitter ok", Spec{Kind: KindJitter, MaxMs: 100}, true},
		{"jitter missing maxMs", Spec{Kind: KindJitter}, false},
		{"jitter maxMs 5000", Spec{Kind: KindJitter, MaxMs: 5000}, true},
		{"jitter maxMs 5001", Spec{Kind: KindJitter, MaxMs: 5001}, false},
		{"burst ok", Spec{Kind: KindBurstDelivery, Rate: 0.1, HoldFrames: 5}, true},
		{"burst holdFrames 1", Spec{Kind: KindBurstDelivery, HoldFrames: 1}, false},
		{"burst holdFrames 301", Spec{Kind: KindBurstDelivery, HoldFrames: 301}, false},
		{"burst missing holdFrames", Spec{Kind: KindBurstDelivery, Rate: 0.5}, false},
		{"burst rate 2", Spec{Kind: KindBurstDelivery, Rate: 2, HoldFrames: 5}, false},
		{"mismatch ok", Spec{Kind: KindPTSDTSMismatch, ReorderDepth: 4}, true},
		{"mismatch depth 5", Spec{Kind: KindPTSDTSMismatch, ReorderDepth: 5}, false},
		{"mismatch missing depth", Spec{Kind: KindPTSDTSMismatch}, false},
		{"delayed empty", Spec{Kind: KindPTSDelayed}, false},
		{"delayed delay only", Spec{Kind: KindPTSDelayed, DelayMs: 60000}, true},
		{"delayed delay 60001", Spec{Kind: KindPTSDelayed, DelayMs: 60001}, false},
		{"delayed drift 100000", Spec{Kind: KindPTSDelayed, DriftPpm: 100000}, true},
		{"delayed drift 100001", Spec{Kind: KindPTSDelayed, DriftPpm: 100001}, false},
		{"delayed both", Spec{Kind: KindPTSDelayed, DelayMs: 10, DriftPpm: 10}, true},
		{"jitter foreign param", Spec{Kind: KindJitter, MaxMs: 10, HoldFrames: 5}, false},
	}
	for _, c := range cases {
		err := check(c.spec)
		if c.ok && err != nil {
			t.Errorf("%s: unexpected error %v", c.name, err)
		}
		if !c.ok && !errors.Is(err, ErrBadParam) && !errors.Is(err, ErrBadRate) {
			t.Errorf("%s: err = %v, want ErrBadParam or ErrBadRate", c.name, err)
		}
	}
	// A rate above 1 is a rate error specifically, not a generic parameter one.
	if err := check(Spec{Kind: KindBurstDelivery, Rate: 2, HoldFrames: 5}); !errors.Is(err, ErrBadRate) {
		t.Errorf("burst rate 2: err = %v, want ErrBadRate", err)
	}
}

func TestTimingValidation(t *testing.T) { validateTimingTable(t) }
