package seed

import "testing"

func TestCameraIsDeterministicAndDistinct(t *testing.T) {
	root := Seed(0x3f2a9c81)

	a := root.Camera(0)
	b := root.Camera(0)
	if a != b {
		t.Fatalf("Camera(0) not deterministic: %#x vs %#x", a, b)
	}

	seen := map[Seed]int{}
	for i := 0; i < 1000; i++ {
		s := root.Camera(i)
		if prev, dup := seen[s]; dup {
			t.Fatalf("camera %d collides with %d at %#x", i, prev, s)
		}
		seen[s] = i
	}

	if root.Camera(0) == Seed(0) {
		t.Fatal("derived seed is zero")
	}
	if root.Camera(0) == root {
		t.Fatal("derived seed equals root")
	}
}

// A different root must move every camera, or one fleet's faults leak into another.
func TestDifferentRootsDiverge(t *testing.T) {
	for i := 0; i < 100; i++ {
		if Seed(1).Camera(i) == Seed(2).Camera(i) {
			t.Fatalf("roots 1 and 2 agree at camera %d", i)
		}
	}
}

// Spec section 8: streams split per concern so that adding a fault type does
// not shift another concern's sequence.
func TestStreamLabelsAreIndependent(t *testing.T) {
	cam := Seed(0x3f2a9c81).Camera(7)

	media1 := cam.Stream("media")
	media2 := cam.Stream("media")
	if media1 != media2 {
		t.Fatalf("Stream(media) not deterministic: %#x vs %#x", media1, media2)
	}
	if cam.Stream("media") == cam.Stream("fault") {
		t.Fatal("media and fault streams are identical")
	}

	// The point of the property: introducing a new label must leave the old
	// ones byte-identical.
	before := cam.Stream("fault")
	_ = cam.Stream("a-brand-new-concern")
	if cam.Stream("fault") != before {
		t.Fatal("deriving a new label perturbed an existing one")
	}
}

// A zero root is what an uninitialised Spec gives a caller, so it is the most
// likely root in a bug report -- it must derive seeds with the same properties
// as any other root, not some degenerate all-zero sequence.
func TestZeroRootDerivesDistinctNonZeroSeeds(t *testing.T) {
	root := Seed(0)

	a := root.Camera(0)
	b := root.Camera(0)
	if a != b {
		t.Fatalf("Camera(0) not deterministic from zero root: %#x vs %#x", a, b)
	}

	seen := map[Seed]int{}
	for i := 0; i < 1000; i++ {
		s := root.Camera(i)
		if s == Seed(0) {
			t.Fatalf("camera %d derives a zero seed from a zero root", i)
		}
		if prev, dup := seen[s]; dup {
			t.Fatalf("camera %d collides with %d at %#x", i, prev, s)
		}
		seen[s] = i
	}
}

func TestRandIsReproducible(t *testing.T) {
	s := Seed(0x3f2a9c81).Camera(3).Stream("fault")

	first := make([]uint64, 8)
	r := s.Rand()
	for i := range first {
		first[i] = r.Uint64()
	}

	r2 := s.Rand()
	for i := range first {
		if got := r2.Uint64(); got != first[i] {
			t.Fatalf("draw %d differs: %#x vs %#x", i, got, first[i])
		}
	}
}
