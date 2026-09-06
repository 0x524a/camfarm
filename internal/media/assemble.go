package media

import (
	"errors"
	"fmt"
)

// rawSample is one sample as a container reports it, before any codec-specific
// validation or rebasing.
type rawSample struct {
	NALUs [][]byte
	// PTS and DTS are in 90 kHz units, not yet rebased so the first sample is 0.
	PTS, DTS int64
	// Sync is the container's own random-access declaration. SyncKnown reports
	// whether the container declared anything at all: MPEG-TS does not, so it
	// leaves SyncKnown false and Sync is then never read.
	Sync      bool
	SyncKnown bool
}

// assemble validates parsed samples and finalizes them into immutable Media.
//
// Every container converges here, so validation, timestamp rebasing, and the
// random-access decision are written exactly once. A container that built a
// *Media itself instead would be the place two ingestion paths silently
// diverged, which is the failure the cross-container equivalence test exists to
// catch.
func assemble(ad codecAdapter, ps paramSets, samples []rawSample) (*Media, error) {
	// Checked before the parameter sets, matching the order that predates the
	// adapter seam: a source with nothing in it should say so, rather than
	// blaming a missing SPS it never had a chance to carry.
	if len(samples) == 0 {
		return nil, errors.New("media: source contains no access units")
	}
	if err := ad.Validate(ps); err != nil {
		return nil, err
	}

	width, height, fps, err := ad.Geometry(ps)
	if err != nil {
		return nil, err
	}

	m := &Media{
		Codec:  ad.Codec(),
		VPS:    ps.VPS,
		SPS:    ps.SPS,
		PPS:    ps.PPS,
		Width:  width,
		Height: height,
		FPS:    fps,
		AUs:    make([]AccessUnit, 0, len(samples)),
	}

	// Rebase on the first sample's DTS so AccessUnit's documented invariant
	// holds identically for every container. Both series shift by the same
	// offset, which preserves the PTS-DTS skew that carries B-frame reordering;
	// shifting them independently would flatten it silently.
	base := samples[0].DTS

	for i, s := range samples {
		randomAccess := ad.IsRandomAccess(s.NALUs)

		// The codec is authoritative. Whether a client can begin decoding here
		// is a property of the bitstream, not of the muxer's bookkeeping, and
		// routing every container through one decision keeps MPEG-TS and MP4
		// from diverging. Where a container also declares a value, a
		// disagreement is refused rather than quietly resolved: a file whose
		// sync table contradicts its own bitstream is exactly the input that
		// would undermine a reproducibility claim without ever looking wrong.
		if s.SyncKnown && s.Sync != randomAccess {
			return nil, fmt.Errorf(
				"media: sample %d: container declares random access = %v but the bitstream says %v",
				i, s.Sync, randomAccess)
		}

		m.AUs = append(m.AUs, AccessUnit{
			NALUs:        s.NALUs,
			PTS:          s.PTS - base,
			DTS:          s.DTS - base,
			RandomAccess: randomAccess,
		})
	}
	return m, nil
}
