package onvif

import (
	"encoding/binary"
	"fmt"

	"github.com/0x524a/camfarm/internal/seed"
)

// EndpointUUID derives a camera's ONVIF endpoint-reference UUID from its seed.
//
// A real device has a stable UUID that identifies it on the network; here it
// must also be reproducible, so it comes from the seed rather than from a
// random source. The result is shaped like an RFC 4122 version 4 UUID.
func EndpointUUID(s seed.Seed) string {
	r := s.Stream("onvif-uuid").Rand()
	var b [16]byte
	binary.BigEndian.PutUint64(b[0:8], r.Uint64())
	binary.BigEndian.PutUint64(b[8:16], r.Uint64())
	b[6] = b[6]&0x0f | 0x40 // version 4
	b[8] = b[8]&0x3f | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
