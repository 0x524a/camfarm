package fault

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/0x524a/camfarm/internal/seed"
)

func TestCatalogueMatchesKnownKinds(t *testing.T) {
	seen := map[Kind]bool{}
	for _, ki := range Catalogue() {
		if seen[ki.Kind] {
			t.Fatalf("duplicate catalogue entry %q", ki.Kind)
		}
		seen[ki.Kind] = true
		if !Known(ki.Kind) {
			t.Errorf("%q is in the catalogue but not Known", ki.Kind)
		}
		if strings.TrimSpace(ki.Models) == "" {
			t.Errorf("%q has no Models text: every kind must name the real bug it models", ki.Kind)
		}
	}
	for k := range knownKinds {
		if !seen[k] {
			t.Errorf("%q is Known but missing from the catalogue", k)
		}
	}
}

func TestCatalogueOrderAndImplementedFlags(t *testing.T) {
	var kinds []Kind
	impl := map[Kind]bool{}
	for _, ki := range Catalogue() {
		kinds = append(kinds, ki.Kind)
		impl[ki.Kind] = ki.Implemented
		if Implemented(ki.Kind) != ki.Implemented {
			t.Errorf("Implemented(%q) disagrees with the catalogue", ki.Kind)
		}
	}
	want := []Kind{KindFrameDrop, KindPacketLoss, KindKeyframeStarvation,
		KindRTSPError, KindTeardown, KindSDPLie, KindJitter, KindBurstDelivery,
		KindPTSDTSMismatch, KindPTSDelayed, KindAcceptThenSilence}
	if len(kinds) != len(want) {
		t.Fatalf("catalogue kinds = %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("catalogue order = %v, want %v", kinds, want)
		}
	}
	for _, k := range []Kind{KindFrameDrop, KindPacketLoss, KindKeyframeStarvation, KindRTSPError, KindTeardown, KindSDPLie} {
		if !impl[k] {
			t.Errorf("%q should be implemented", k)
		}
	}
	for _, k := range []Kind{KindJitter, KindBurstDelivery, KindPTSDTSMismatch, KindPTSDelayed, KindAcceptThenSilence} {
		if impl[k] {
			t.Errorf("%q should not be implemented yet", k)
		}
	}
}

func TestCatalogueReturnsACopy(t *testing.T) {
	c := Catalogue()
	c[0].Models = "mutated"
	c[0].Params[0].Name = "mutated"
	*c[0].Params[0].Min = 99
	*c[0].Params[0].Max = 99
	c[0].Params[0].Default = "mutated"
	again := Catalogue()
	if again[0].Models == "mutated" || again[0].Params[0].Name == "mutated" {
		t.Fatal("mutating a returned catalogue changed the next one")
	}
	if *again[0].Params[0].Min != 0 || *again[0].Params[0].Max != 1 || again[0].Params[0].Default == "mutated" {
		t.Fatal("mutating through a Min/Max pointer or Default changed the next catalogue")
	}
}

func TestCatalogueJSONShape(t *testing.T) {
	b, err := json.Marshal(Catalogue())
	if err != nil {
		t.Fatal(err)
	}
	var got []map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	fd := got[0]
	if fd["kind"] != "frame_drop" || fd["implemented"] != true {
		t.Fatalf("first entry = %v", fd)
	}
	params := fd["params"].([]any)
	names := []string{}
	for _, p := range params {
		names = append(names, p.(map[string]any)["name"].(string))
	}
	if strings.Join(names, ",") != "rate,protectKeyframes" {
		t.Fatalf("frame_drop params = %v, want rate,protectKeyframes", names)
	}
	// Unimplemented kinds are listed with an empty (never null) params array.
	last := got[len(got)-1]
	if last["implemented"] != false {
		t.Fatalf("last entry should be unimplemented: %v", last)
	}
	if p, ok := last["params"].([]any); !ok || len(p) != 0 {
		t.Fatalf("unimplemented kind params = %v, want []", last["params"])
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name string
		spec Spec
		want error // nil means valid
	}{
		{"frame_drop ok", Spec{Kind: KindFrameDrop, Rate: 0.5}, nil},
		{"frame_drop rate zero is a no-op", Spec{Kind: KindFrameDrop}, nil},
		{"frame_drop rate high", Spec{Kind: KindFrameDrop, Rate: 1.5}, ErrBadRate},
		{"frame_drop rate negative", Spec{Kind: KindFrameDrop, Rate: -0.1}, ErrBadRate},
		{"frame_drop rate NaN", Spec{Kind: KindFrameDrop, Rate: math.NaN()}, ErrBadRate},
		{"frame_drop keepEvery does not apply", Spec{Kind: KindFrameDrop, Rate: 0.1, KeepEvery: 3}, ErrBadParam},
		{"frame_drop meanBurst does not apply", Spec{Kind: KindFrameDrop, Rate: 0.1, MeanBurst: 2}, ErrBadParam},
		{"packet_loss independent", Spec{Kind: KindPacketLoss, Rate: 0.1}, nil},
		{"packet_loss rate one independent", Spec{Kind: KindPacketLoss, Rate: 1}, nil},
		{"packet_loss burst ok", Spec{Kind: KindPacketLoss, Rate: 0.2, MeanBurst: 4}, nil},
		{"packet_loss burst at the limit", Spec{Kind: KindPacketLoss, Rate: 0.8, MeanBurst: 4}, nil},
		{"packet_loss burst above the limit", Spec{Kind: KindPacketLoss, Rate: 0.9, MeanBurst: 4}, ErrBadParam},
		{"packet_loss burst needs rate below one", Spec{Kind: KindPacketLoss, Rate: 1, MeanBurst: 4}, ErrBadParam},
		{"packet_loss meanBurst must exceed one", Spec{Kind: KindPacketLoss, Rate: 0.1, MeanBurst: 1}, ErrBadParam},
		{"packet_loss meanBurst below one", Spec{Kind: KindPacketLoss, Rate: 0.1, MeanBurst: 0.5}, ErrBadParam},
		{"packet_loss keepEvery does not apply", Spec{Kind: KindPacketLoss, Rate: 0.1, KeepEvery: 2}, ErrBadParam},
		{"keyframe_starvation ok", Spec{Kind: KindKeyframeStarvation, KeepEvery: 3}, nil},
		{"keyframe_starvation keepEvery missing", Spec{Kind: KindKeyframeStarvation}, ErrBadParam},
		{"keyframe_starvation keepEvery one", Spec{Kind: KindKeyframeStarvation, KeepEvery: 1}, ErrBadParam},
		{"keyframe_starvation keepEvery too large", Spec{Kind: KindKeyframeStarvation, KeepEvery: 1001}, ErrBadParam},
		{"keyframe_starvation rate does not apply", Spec{Kind: KindKeyframeStarvation, KeepEvery: 3, Rate: 0.5}, ErrBadParam},
		{"keyframe_starvation protectKeyframes does not apply", Spec{Kind: KindKeyframeStarvation, KeepEvery: 3, ProtectKeyframes: true}, ErrBadParam},
		{"unknown kind", Spec{Kind: "nope"}, ErrUnknownKind},
		{"unimplemented kinds skip param checks", Spec{Kind: KindAcceptThenSilence, Rate: 0.5, KeepEvery: 7}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(tc.spec)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("Validate = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("Validate = %v, want errors.Is %v", err, tc.want)
			}
		})
	}
}

func TestBoundsTextOnlyFormatsDeclaredBounds(t *testing.T) {
	one := 1.0
	if got := boundsText(Param{Min: &one}); got != "[1, no upper bound)" {
		t.Errorf("min only: %q", got)
	}
	if got := boundsText(Param{Max: &one}); got != "(no lower bound, 1]" {
		t.Errorf("max only: %q", got)
	}
	if got := boundsText(Param{Min: &one, Max: &one}); got != "[1, 1]" {
		t.Errorf("both: %q", got)
	}
	if got := boundsText(Param{}); got == "" {
		t.Error("neither: empty")
	}
}

func TestValidateOutOfRangeReportsBounds(t *testing.T) {
	err := Validate(Spec{Kind: KindPacketLoss, Rate: 0.1, MeanBurst: 2000})
	if !errors.Is(err, ErrBadParam) || !strings.Contains(err.Error(), "[1, 1000]") {
		t.Fatalf("err = %v", err)
	}
}

func TestNewRefusesRepeatedKind(t *testing.T) {
	_, err := New(seed.Seed(1), []Spec{{Kind: KindFrameDrop, Rate: 0.1}, {Kind: KindFrameDrop, Rate: 0.2}})
	if !errors.Is(err, ErrDuplicateKind) {
		t.Fatalf("err = %v, want ErrDuplicateKind", err)
	}
}
