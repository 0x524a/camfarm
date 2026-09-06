package media

import (
	"errors"
	"fmt"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h265"
)

// Parameter-set errors, one per codec so the message names what is actually
// missing. The H.264 wording is unchanged from the version that predated the
// adapter seam, because tests assert it verbatim.
var (
	ErrNoParameterSetsH264 = errors.New("media: source carries no in-band SPS/PPS")
	ErrNoParameterSetsH265 = errors.New("media: source carries no in-band VPS/SPS/PPS")
)

// paramSets holds a codec's out-of-band parameter sets. VPS is H.265-only and
// stays nil for H.264.
type paramSets struct{ VPS, SPS, PPS []byte }

// codecAdapter owns everything codec-specific about classifying NAL units.
//
// It takes plain NALU lists and never sees a container, so every adapter is
// testable against hand-built input with no fixture and no demuxer. That
// isolation is the point: each trap in this area (H.265's two-byte NAL header,
// open-GOP CRA keyframes) then has exactly one place to be got wrong instead of
// one per container.
type codecAdapter interface {
	// Codec identifies what this adapter handles.
	Codec() Codec
	// Classify records any parameter sets found in au into ps.
	Classify(au [][]byte, ps *paramSets)
	// IsRandomAccess reports whether a decoder can begin at au.
	IsRandomAccess(au [][]byte) bool
	// Validate reports whether ps holds every parameter set this codec needs.
	Validate(ps paramSets) error
	// Geometry derives the advertised dimensions and frame rate from ps.
	Geometry(ps paramSets) (width, height int, fps float64, err error)
}

type h264Adapter struct{}

func (h264Adapter) Codec() Codec { return CodecH264 }

func (h264Adapter) Classify(au [][]byte, ps *paramSets) {
	for _, n := range au {
		if len(n) < 1 {
			continue
		}
		switch h264.NALUType(n[0] & 0x1F) {
		case h264.NALUTypeSPS:
			ps.SPS = n
		case h264.NALUTypePPS:
			ps.PPS = n
		}
	}
}

// IsRandomAccess delegates rather than comparing NALU types locally. See the
// h265Adapter method for why that rule is absolute.
func (h264Adapter) IsRandomAccess(au [][]byte) bool { return h264.IsRandomAccess(au) }

func (h264Adapter) Validate(ps paramSets) error {
	if len(ps.SPS) == 0 || len(ps.PPS) == 0 {
		return ErrNoParameterSetsH264
	}
	return nil
}

func (h264Adapter) Geometry(ps paramSets) (int, int, float64, error) {
	var sps h264.SPS
	if err := sps.Unmarshal(ps.SPS); err != nil {
		return 0, 0, 0, fmt.Errorf("media: parsing H264 SPS: %w", err)
	}
	return sps.Width(), sps.Height(), sps.FPS(), nil
}

type h265Adapter struct{}

func (h265Adapter) Codec() Codec { return CodecH265 }

// Classify reads the H.265 NAL header, which is two bytes rather than H.264's
// one, with the type in six bits starting at bit 1 of byte 0. The constants are
// underscored (NALUType_VPS_NUT) where H.264's are not (NALUTypeSPS).
func (h265Adapter) Classify(au [][]byte, ps *paramSets) {
	for _, n := range au {
		if len(n) < 2 {
			continue
		}
		switch h265.NALUType((n[0] >> 1) & 0b111111) {
		case h265.NALUType_VPS_NUT:
			ps.VPS = n
		case h265.NALUType_SPS_NUT:
			ps.SPS = n
		case h265.NALUType_PPS_NUT:
			ps.PPS = n
		}
	}
}

// IsRandomAccess delegates to mediacommon and must never be replaced by a local
// NALU-type comparison. H.265 splits "a decoder can start here" across
// IDR_W_RADL, IDR_N_LP, and CRA_NUT, and libx265 defaults to open GOP, so every
// keyframe after the first is a CRA. A check written for IDR alone finds the IDR
// at the head of a file, passes casual testing, and then reports every later
// keyframe as non-random-access -- a failure that surfaces at the loop boundary,
// far from its cause.
func (h265Adapter) IsRandomAccess(au [][]byte) bool { return h265.IsRandomAccess(au) }

func (h265Adapter) Validate(ps paramSets) error {
	if len(ps.VPS) == 0 || len(ps.SPS) == 0 || len(ps.PPS) == 0 {
		return ErrNoParameterSetsH265
	}
	return nil
}

func (h265Adapter) Geometry(ps paramSets) (int, int, float64, error) {
	var sps h265.SPS
	if err := sps.Unmarshal(ps.SPS); err != nil {
		return 0, 0, 0, fmt.Errorf("media: parsing H265 SPS: %w", err)
	}
	return sps.Width(), sps.Height(), sps.FPS(), nil
}
