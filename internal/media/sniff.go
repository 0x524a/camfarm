package media

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
)

// container identifies a media container format.
type container string

// Containers this package can read.
const (
	containerMPEGTS  container = "MPEG-TS"
	containerISOBMFF container = "ISOBMFF (MP4/MOV)"
)

// tsPacketLen is the MPEG-TS packet size. Sync bytes recur at this stride.
const tsPacketLen = 188

// sniffLen is how many leading bytes sniff wants: enough to see sync bytes at
// offsets 0, 188 and 376.
const sniffLen = 2*tsPacketLen + 1

// sniff identifies a container from its leading bytes.
//
// Detection is by content only; a filename extension is never consulted. An
// extension can lie, and a file named .mp4 that actually holds MPEG-TS should
// load rather than fail.
func sniff(header []byte) (container, error) {
	// ISOBMFF: an 'ftyp' box type at offset 4, following the box length. This
	// covers plain and fragmented MP4 alike -- telling those two apart needs the
	// box structure rather than the header, which is why parseISOBMFF discovers
	// it by trying both instead of sniffing for mvex here.
	if len(header) >= 8 && bytes.Equal(header[4:8], []byte("ftyp")) {
		return containerISOBMFF, nil
	}

	// MPEG-TS: sync byte 0x47 at every packet boundary present in the buffer.
	// Requiring the stride rather than just offset 0 avoids claiming any file
	// that happens to begin with 0x47. A buffer too short to hold a second
	// packet is accepted on offset 0 alone; such a source carries no complete
	// access unit anyway and fails later with a precise message.
	if len(header) > 0 && header[0] == 0x47 {
		stridesOK := true
		for off := tsPacketLen; off < len(header); off += tsPacketLen {
			if header[off] != 0x47 {
				stridesOK = false
				break
			}
		}
		if stridesOK {
			return containerMPEGTS, nil
		}
	}

	n := min(len(header), 8)
	return "", fmt.Errorf(
		"media: unrecognised container (first %d bytes: %s); supported: %s, %s",
		n, hex.EncodeToString(header[:n]), containerMPEGTS, containerISOBMFF)
}

// parseAny sniffs the container in r and parses it.
//
// r must be seekable: plain MP4 needs random access to walk its box structure,
// so a stream-only source cannot be supported. Every source in this package
// reads a file or an in-memory fixture, so that costs nothing today.
func parseAny(r io.ReadSeeker) (*Media, error) {
	header := make([]byte, sniffLen)
	n, err := io.ReadFull(r, header)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, fmt.Errorf("media: reading source header: %w", err)
	}
	// A source shorter than the sniff window is still worth trying: sniff reports
	// on the bytes that exist rather than requiring a full window.
	header = header[:n]

	kind, err := sniff(header)
	if err != nil {
		return nil, err
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("media: rewinding source: %w", err)
	}

	switch kind {
	case containerMPEGTS:
		return parseMPEGTS(r)
	case containerISOBMFF:
		return parseISOBMFF(r)
	default:
		return nil, fmt.Errorf("media: unhandled container %q", kind)
	}
}
