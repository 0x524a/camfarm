package media

import (
	"errors"
	"fmt"
	"io"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	mp4codecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/pmp4"
)

// clockRate is the RTP timestamp clock every AccessUnit is expressed in.
const clockRate = 90000

// errNoSamples means a container parsed structurally but held no samples for its
// video track.
//
// It is the discriminator between plain and fragmented MP4: a fragmented file's
// moov describes its tracks, so a plain-MP4 read of one succeeds and simply
// finds nothing to play. It must not escape this package as a user-facing error.
var errNoSamples = errors.New("media: container holds no samples")

// rescale converts a value in a track's own timescale to 90 kHz.
//
// Multiplication comes first so a small timescale does not truncate to zero:
// with a 1000-tick timescale, dividing first would turn every value below a
// second into 0.
func rescale(v, timescale int64) int64 { return v * clockRate / timescale }

// adapterAndParamsFromMP4Codec maps an MP4 codec box to this package's adapter
// and parameter sets.
//
// Nothing here parses a box: mediacommon has already extracted the parameter
// sets from avcC or hvcC by the time a track carries this value, and it refuses
// multi-SPS or multi-PPS boxes rather than silently picking one.
func adapterAndParamsFromMP4Codec(c mp4codecs.Codec) (codecAdapter, paramSets, bool) {
	switch v := c.(type) {
	case *mp4codecs.H264:
		return h264Adapter{}, paramSets{SPS: v.SPS, PPS: v.PPS}, true
	case *mp4codecs.H265:
		return h265Adapter{}, paramSets{VPS: v.VPS, SPS: v.SPS, PPS: v.PPS}, true
	default:
		return nil, paramSets{}, false
	}
}

// parseISOBMFF reads an MP4-family file into immutable media.
func parseISOBMFF(r io.ReadSeeker) (*Media, error) {
	// Plain MP4 first: it is the common case, and telling plain from fragmented
	// up front would mean inspecting moov for an mvex box. Trying and falling
	// back is shorter and more robust than that heuristic.
	var pres pmp4.Presentation
	if err := pres.Unmarshal(r); err == nil {
		m, err := mediaFromPMP4(&pres)
		switch {
		case err == nil:
			return m, nil
		case !errors.Is(err, errNoSamples):
			// Parsed as plain MP4 and found real trouble. Falling back to the
			// fragmented reader here would replace a precise error with a
			// confusing one.
			return nil, err
		}
	}
	return nil, fmt.Errorf("media: reading MP4: %w", errNoSamples)
}

// mediaFromPMP4 turns a parsed plain-MP4 presentation into immutable media.
func mediaFromPMP4(pres *pmp4.Presentation) (*Media, error) {
	for _, tr := range pres.Tracks {
		ad, ps, ok := adapterAndParamsFromMP4Codec(tr.Codec)
		if !ok {
			continue
		}
		if tr.TimeScale == 0 {
			return nil, fmt.Errorf("media: MP4 track %d declares a zero timescale", tr.ID)
		}

		samples := make([]rawSample, 0, len(tr.Samples))
		var dts int64
		for i, s := range tr.Samples {
			payload, err := s.GetPayload()
			if err != nil {
				return nil, fmt.Errorf("media: reading MP4 sample %d: %w", i, err)
			}

			// AVCC framing is length-prefixed NALUs and is codec-agnostic, so
			// the same unmarshal serves H.265: mediacommon's own fmp4 helper
			// GetH265 likewise delegates straight to GetH264.
			var avcc h264.AVCC
			if err := avcc.Unmarshal(payload); err != nil {
				return nil, fmt.Errorf("media: splitting MP4 sample %d: %w", i, err)
			}
			nalus := copyNALUs(avcc)
			if len(nalus) == 0 {
				continue
			}
			ad.Classify(nalus, &ps)

			samples = append(samples, rawSample{
				NALUs: nalus,
				PTS:   rescale(dts+int64(s.PTSOffset), int64(tr.TimeScale)),
				DTS:   rescale(dts, int64(tr.TimeScale)),
				// MP4 declares random access itself, via stss. assemble
				// cross-checks it against the bitstream.
				Sync:      !s.IsNonSyncSample,
				SyncKnown: true,
			})
			dts += int64(s.Duration)
		}

		if len(samples) == 0 {
			return nil, errNoSamples
		}
		return assemble(ad, ps, samples)
	}
	return nil, ErrNoVideoTrack
}
