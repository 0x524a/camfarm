package fault

import (
	"errors"
	"testing"

	"github.com/0x524a/camfarm/internal/seed"
)

func TestNoSpecsDecidesNothing(t *testing.T) {
	e, err := New(seed.Seed(1), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if e.Enabled() {
		t.Error("Enabled() true with no specs")
	}
	for i := 0; i < 1000; i++ {
		if d := e.DecideFrame(i); len(d) != 0 {
			t.Fatalf("frame %d decided %v with no specs configured", i, d)
		}
	}
}

// The core guarantee: same seed, same spec, same sequence of decisions.
func TestSameSeedSameDecisionSequence(t *testing.T) {
	specs := []Spec{{Kind: KindFrameDrop, Rate: 0.3}}

	run := func() []Decision {
		e, err := New(seed.Seed(0x3f2a9c81).Camera(4), specs)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		var out []Decision
		for i := 0; i < 500; i++ {
			out = append(out, e.DecideFrame(i)...)
		}
		return out
	}

	a, b := run(), run()
	if len(a) == 0 {
		t.Fatal("rate 0.3 over 500 frames fired nothing; the generator is not being drawn from")
	}
	if len(a) != len(b) {
		t.Fatalf("run lengths differ: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("decision %d differs: %+v vs %+v", i, a[i], b[i])
		}
	}
}

func TestDifferentCamerasDecideDifferently(t *testing.T) {
	specs := []Spec{{Kind: KindFrameDrop, Rate: 0.5}}
	root := seed.Seed(0x3f2a9c81)

	collect := func(idx int) []Decision {
		e, err := New(root.Camera(idx), specs)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		var out []Decision
		for i := 0; i < 200; i++ {
			out = append(out, e.DecideFrame(i)...)
		}
		return out
	}

	a, b := collect(0), collect(1)
	if len(a) == len(b) {
		same := true
		for i := range a {
			if a[i] != b[i] {
				same = false
				break
			}
		}
		if same {
			t.Fatal("two cameras produced an identical decision sequence")
		}
	}
}

// Spec section 8: adding a fault kind must not perturb an existing kind's
// sequence. This is why each kind draws from its own generator.
func TestAddingAKindDoesNotShiftAnother(t *testing.T) {
	base := []Spec{{Kind: KindFrameDrop, Rate: 0.4}}
	extended := []Spec{
		{Kind: KindFrameDrop, Rate: 0.4},
		{Kind: KindKeyframeStarvation, Rate: 0.4},
	}

	dropsFrom := func(specs []Spec) []int {
		e, err := New(seed.Seed(99), specs)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		var frames []int
		for i := 0; i < 300; i++ {
			for _, d := range e.DecideFrame(i) {
				if d.Kind == KindFrameDrop {
					frames = append(frames, d.FrameIndex)
				}
			}
		}
		return frames
	}

	before, after := dropsFrom(base), dropsFrom(extended)
	if len(before) != len(after) {
		t.Fatalf("frame-drop count changed when another kind was added: %d vs %d", len(before), len(after))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("frame-drop decision %d moved from frame %d to %d", i, before[i], after[i])
		}
	}
}

func TestRateBoundaries(t *testing.T) {
	e, err := New(seed.Seed(7), []Spec{{Kind: KindFrameDrop, Rate: 1}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for i := 0; i < 50; i++ {
		if len(e.DecideFrame(i)) != 1 {
			t.Fatalf("rate 1 did not fire at frame %d", i)
		}
	}

	e0, err := New(seed.Seed(7), []Spec{{Kind: KindFrameDrop, Rate: 0}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for i := 0; i < 50; i++ {
		if len(e0.DecideFrame(i)) != 0 {
			t.Fatalf("rate 0 fired at frame %d", i)
		}
	}
}

func TestDecisionCarriesFrameIndex(t *testing.T) {
	e, err := New(seed.Seed(7), []Spec{{Kind: KindFrameDrop, Rate: 1}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d := e.DecideFrame(123)
	if len(d) != 1 {
		t.Fatalf("decisions = %d, want 1", len(d))
	}
	if d[0].FrameIndex != 123 {
		t.Errorf("FrameIndex = %d, want 123", d[0].FrameIndex)
	}
	if d[0].Kind != KindFrameDrop {
		t.Errorf("Kind = %q", d[0].Kind)
	}
}

func TestRejectsUnknownKindAndBadRate(t *testing.T) {
	if _, err := New(seed.Seed(1), []Spec{{Kind: "not-a-fault", Rate: 0.5}}); !errors.Is(err, ErrUnknownKind) {
		t.Errorf("unknown kind: err = %v, want ErrUnknownKind", err)
	}
	for _, rate := range []float64{-0.1, 1.1} {
		if _, err := New(seed.Seed(1), []Spec{{Kind: KindFrameDrop, Rate: rate}}); !errors.Is(err, ErrBadRate) {
			t.Errorf("rate %v: err = %v, want ErrBadRate", rate, err)
		}
	}
}
