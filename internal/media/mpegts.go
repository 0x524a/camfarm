package media

import (
	"errors"
	"fmt"
	"io"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts"
)

// ErrNoVideoTrack is returned when a source carries no track in a codec this
// package can serve.
var ErrNoVideoTrack = errors.New("media: no supported video track in source")

// parseMPEGTS reads an MPEG-TS stream into immutable media.
func parseMPEGTS(r io.Reader) (*Media, error) {
	mr := &mpegts.Reader{R: r}
	if err := mr.Initialize(); err != nil {
		return nil, fmt.Errorf("media: reading MPEG-TS: %w", err)
	}

	// First H.264 or H.265 track wins. Track order is the container's, not a
	// map's, so this is deterministic for a given file.
	var track *mpegts.Track
	var ad codecAdapter
	for _, t := range mr.Tracks() {
		switch t.Codec.(type) {
		case *mpegts.CodecH264:
			track, ad = t, h264Adapter{}
		case *mpegts.CodecH265:
			track, ad = t, h265Adapter{}
		}
		if track != nil {
			break
		}
	}
	if track == nil {
		return nil, ErrNoVideoTrack
	}

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

	// The callback body is codec-independent: mediacommon hands back the same
	// (pts, dts, au) shape for both, and every codec-specific decision inside
	// belongs to the adapter.
	onData := func(pts, dts int64, au [][]byte) error {
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
	}

	switch ad.Codec() {
	case CodecH264:
		mr.OnDataH264(track, onData)
	case CodecH265:
		mr.OnDataH265(track, onData)
	}

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
