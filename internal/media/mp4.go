package media

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
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

	// Fragmented fallback. Both readers tolerate being handed a whole
	// self-contained file: Init.Unmarshal's box switch does not recognise moof,
	// so it skips over the fragments, and Parts.Unmarshal's does not recognise
	// ftyp or moov, so it skips over the header. Neither needs the file
	// pre-split into init and media segments.
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("media: rewinding MP4: %w", err)
	}
	whole, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("media: reading MP4: %w", err)
	}

	var init fmp4.Init
	if err := init.Unmarshal(bytes.NewReader(whole)); err != nil {
		return nil, fmt.Errorf("media: reading MP4 init: %w", err)
	}
	var parts fmp4.Parts
	if err := parts.Unmarshal(whole); err != nil {
		// A producer that omits trunFlagDataOffsetPreset is refused here rather
		// than mis-parsed. That is the intended failure mode, but the message is
		// opaque on its own, so it is wrapped with what was being attempted.
		return nil, fmt.Errorf("media: reading MP4 fragments: %w", err)
	}
	return mediaFromFMP4(&init, parts)
}

// mediaFromFMP4 turns a parsed fragmented MP4 into immutable media.
func mediaFromFMP4(init *fmp4.Init, parts fmp4.Parts) (*Media, error) {
	for _, it := range init.Tracks {
		ad, ps, ok := adapterAndParamsFromMP4Codec(it.Codec)
		if !ok {
			continue
		}
		if it.TimeScale == 0 {
			return nil, fmt.Errorf("media: MP4 track %d declares a zero timescale", it.ID)
		}

		var samples []rawSample
		for _, part := range parts {
			for _, pt := range part.Tracks {
				if pt.ID != it.ID {
					continue
				}
				// BaseTime is the fragment's tfdt: the decode time of its first
				// sample, in track timescale. Accumulating from it rather than
				// from zero is what keeps timestamps continuous across the
				// fragment boundary.
				dts := int64(pt.BaseTime)
				for i, s := range pt.Samples {
					// Samples arrive pre-split here, unlike the plain-MP4 path:
					// fmp4 exposes a helper that unwraps the length prefixes,
					// and it is codec-agnostic (GetH265 delegates to GetH264).
					nalus, err := s.GetH264()
					if err != nil {
						return nil, fmt.Errorf("media: splitting MP4 fragment sample %d: %w", i, err)
					}
					copied := copyNALUs(nalus)
					if len(copied) == 0 {
						continue
					}
					ad.Classify(copied, &ps)

					// SyncKnown is false here, unlike the plain-MP4 path, and this
					// is a controller ruling, not an implementer shortcut: it is
					// unsafe to trust s.IsNonSyncSample on this path.
					//
					// mediacommon/v2 pkg/formats/fmp4/parts.go:161-167 sets a
					// sample's flags from the trun entry's own SampleFlags when
					// present, or otherwise from tfhd.DefaultSampleFlags — it never
					// reads trun.FirstSampleFlags, the ISO 14496-12 field (tf_flags
					// bit 0x000004) that overrides the flags of a fragment's first
					// sample. ffmpeg's +frag_keyframe sets DefaultSampleFlags to
					// non-sync and relies on exactly that override to mark sample 0
					// of each fragment sync instead. With the override silently
					// dropped, IsNonSyncSample reads true for every fragment's
					// first sample regardless of what the bitstream says, so
					// SyncKnown: true here would fail assemble's cross-check on
					// every fragment boundary — not a one-off, since the drop is
					// unconditional, not a property of this one file.
					//
					// The file itself is not contradicting its own bitstream; the
					// container's declaration is unavailable to us, not wrong, so
					// SyncKnown: false is the accurate encoding, not a workaround.
					// Contrast the plain-MP4 path just below, which reads stss
					// correctly and keeps SyncKnown: true.
					samples = append(samples, rawSample{
						NALUs:     copied,
						PTS:       rescale(dts+int64(s.PTSOffset), int64(it.TimeScale)),
						DTS:       rescale(dts, int64(it.TimeScale)),
						SyncKnown: false,
					})
					dts += int64(s.Duration)
				}
			}
		}

		if len(samples) == 0 {
			return nil, errNoSamples
		}
		return assemble(ad, ps, samples)
	}
	return nil, ErrNoVideoTrack
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
