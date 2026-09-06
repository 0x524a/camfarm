package media

import (
	"errors"
	"fmt"
	"io"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts"
)

// ErrNoH264Track is returned when a source carries no H.264 video.
var ErrNoH264Track = errors.New("media: no H264 track in source")

// parseMPEGTS reads an MPEG-TS stream into immutable media.
func parseMPEGTS(r io.Reader) (*Media, error) {
	mr := &mpegts.Reader{R: r}
	if err := mr.Initialize(); err != nil {
		return nil, fmt.Errorf("media: reading MPEG-TS: %w", err)
	}

	var track *mpegts.Track
	for _, t := range mr.Tracks() {
		if _, ok := t.Codec.(*mpegts.CodecH264); ok {
			track = t
			break
		}
	}
	if track == nil {
		return nil, ErrNoH264Track
	}

	ad := h264Adapter{}
	var ps paramSets
	var samples []rawSample

	// Separate decoders for PTS and DTS. One decoder fed both series would
	// interleave two sequences through a single 33-bit wraparound detector; that
	// happens to work when there are no B-frames but is wrong in general.
	var ptsDec, dtsDec mpegts.TimeDecoder
	ptsDec.Initialize()
	dtsDec.Initialize()

	var decodeErr error
	mr.OnDecodeError(func(err error) {
		if decodeErr == nil {
			decodeErr = err
		}
	})

	mr.OnDataH264(track, func(pts, dts int64, au [][]byte) error {
		nalus := copyNALUs(au)
		if len(nalus) == 0 {
			return nil
		}
		ad.Classify(nalus, &ps)
		samples = append(samples, rawSample{
			NALUs: nalus,
			PTS:   ptsDec.Decode(pts),
			DTS:   dtsDec.Decode(dts),
			// SyncKnown stays false: MPEG-TS carries no sync-sample table, so
			// the bitstream is the only authority available here.
		})
		return nil
	})

	for {
		if err := mr.Read(); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("media: reading MPEG-TS: %w", err)
		}
	}
	if decodeErr != nil {
		return nil, fmt.Errorf("media: decoding MPEG-TS: %w", decodeErr)
	}

	return assemble(ad, ps, samples)
}

// copyNALUs returns a copy of every non-empty NALU in au, dropping empty ones.
//
// At the pinned mediacommon/go-astits versions this hazard is not live: each PES
// payload reaches us through astikit's BytesIterator.NextBytes, which allocates a
// fresh buffer per call, so nothing here currently aliases a reused buffer. The
// copy is deliberate insurance, not a fix for an active bug: mediacommon still
// ships NextBytesNoCopy, and its own mpegts/buffered_reader.go (now marked
// deprecated) shows it once had a buffer-reusing reader on this same path. Since
// the parsed *Media is shared read-only across every camera in the fleet, a
// dependency upgrade that starts reusing buffers here would otherwise corrupt
// media silently instead of failing loudly, which is worse than one extra
// allocation per NALU at load time.
func copyNALUs(au [][]byte) [][]byte {
	nalus := make([][]byte, 0, len(au))
	for _, n := range au {
		if len(n) == 0 {
			continue
		}
		c := make([]byte, len(n))
		copy(c, n)
		nalus = append(nalus, c)
	}
	return nalus
}
