package onvif

import (
	"regexp"
	"testing"

	"github.com/0x524a/camfarm/internal/seed"
)

var uuidV4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestEndpointUUIDIsAVersion4UUID(t *testing.T) {
	for _, s := range []seed.Seed{0, 1, 0xdeadbeef, ^seed.Seed(0)} {
		if u := EndpointUUID(s); !uuidV4.MatchString(u) {
			t.Errorf("EndpointUUID(%#x) = %q, not a v4 UUID", uint64(s), u)
		}
	}
}

func TestEndpointUUIDIsDeterministicAndPerSeed(t *testing.T) {
	first, second := EndpointUUID(42), EndpointUUID(42)
	if first != second {
		t.Fatal("same seed gave different UUIDs")
	}
	seen := map[string]seed.Seed{}
	for i := 0; i < 1000; i++ {
		u := EndpointUUID(seed.Seed(1).Camera(i))
		if prev, dup := seen[u]; dup {
			t.Fatalf("cameras %#x and %#x share UUID %s", uint64(prev), uint64(seed.Seed(1).Camera(i)), u)
		}
		seen[u] = seed.Seed(1).Camera(i)
	}
}
