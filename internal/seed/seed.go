// Package seed derives the deterministic random streams camfarm runs on.
//
// Every fault decision must be a pure function of the root seed, the camera's
// stable index, and an event counter -- never of wall-clock time, goroutine
// scheduling order, or map iteration order. This package supplies the first two
// of those three inputs.
package seed

import (
	"hash/fnv"
	"math/rand/v2"
)

// Seed is a 64-bit seed for one concern of one camera.
type Seed uint64

// splitmix64 is the mixing function from Vigna's SplittableRandom. It is used
// rather than hashing because it is a bijection on uint64: distinct inputs give
// distinct outputs, so per-camera seeds cannot silently collide.
func splitmix64(x uint64) uint64 {
	x += 0x9E3779B97F4A7C15
	z := x
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return z ^ (z >> 31)
}

// Camera derives the seed for the camera at the given stable index.
//
// The index must come from the camera's position in the parsed spec, never from
// iteration over a map: Go randomises map order, which would make a fleet's
// seeds differ between runs of the same spec.
func (s Seed) Camera(index int) Seed {
	return Seed(splitmix64(uint64(s) + splitmix64(uint64(index)+1)))
}

// Stream derives a substream for one concern, such as "media" or "fault".
//
// Labels rather than ordinals: a new concern added later hashes to its own
// value and leaves every existing substream byte-identical, so introducing a
// fault type cannot shift an unrelated sequence.
func (s Seed) Stream(label string) Seed {
	h := fnv.New64a()
	_, _ = h.Write([]byte(label))
	return Seed(splitmix64(uint64(s) + splitmix64(h.Sum64())))
}

// Rand returns a generator seeded from s. Two calls on equal Seeds yield
// identical sequences.
func (s Seed) Rand() *rand.Rand {
	return rand.New(rand.NewPCG(uint64(s), splitmix64(uint64(s))))
}
