// Package media turns a source of encoded video into the immutable slice of
// access units the RTSP pumps serve.
//
// Media is parsed once and shared by every camera that uses the same source:
// that sharing is the scale lever, so a *Media must be treated as read-only
// once Load returns.
package media

// Codec identifies the video codec of a source.
type Codec string

// CodecH264 is H.264.
const CodecH264 Codec = "H264"

// CodecH265 is H.265 / HEVC.
const CodecH265 Codec = "H265"

// AccessUnit is one coded picture, as a list of NAL units without start codes.
type AccessUnit struct {
	// NALUs are owned by this AccessUnit and must not be mutated.
	NALUs [][]byte
	// PTS and DTS are in 90 kHz units, rebased so the first access unit is 0.
	PTS int64
	DTS int64
	// RandomAccess reports whether a decoder can start here.
	RandomAccess bool
}

// Media is parsed, immutable source video.
type Media struct {
	Codec Codec
	// VPS is the H.265 video parameter set. It is nil for H.264, which has no
	// equivalent. A nil check is sufficient to tell the two apart, so this stays
	// a plain field rather than forcing a type switch on every consumer: each
	// one already branches on Codec where codec-specific behaviour is needed.
	VPS    []byte
	SPS    []byte
	PPS    []byte
	Width  int
	Height int
	// FPS is as declared by the bitstream. Zero when the stream carries no
	// timing information.
	FPS float64
	AUs []AccessUnit
}

// FrameDuration returns the nominal spacing between access units in 90 kHz
// units. It is derived from FPS when the bitstream declares timing, and from
// the mean DTS delta otherwise.
func (m *Media) FrameDuration() int64 {
	const clockRate = 90000
	if m.FPS > 0 {
		return int64(clockRate / m.FPS)
	}
	if len(m.AUs) < 2 {
		return clockRate / 30
	}
	span := m.AUs[len(m.AUs)-1].DTS - m.AUs[0].DTS
	return span / int64(len(m.AUs)-1)
}

// Source is somewhere media comes from.
type Source interface {
	// Load parses the source into immutable media.
	Load() (*Media, error)
	// Deterministic reports whether replaying this source yields identical
	// bytes. A live upstream feed does not, which places it outside camfarm's
	// reproducibility guarantee.
	Deterministic() bool
	// Describe returns a short human-readable identification, for logs and
	// inspection output.
	Describe() string
}
