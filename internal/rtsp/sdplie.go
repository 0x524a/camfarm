package rtsp

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h265"

	"github.com/0x524a/camfarm/internal/media"
)

// The lie parameter sets are real encoder output, embedded because
// mediacommon's SPS types can parse but not serialise, so a geometry cannot be
// edited into the honest SPS. Generated once with ffmpeg 4:2:0 testsrc, two
// frames, the parameter-set NAL units base64-encoded:
//
//	ffmpeg -f lavfi -i testsrc=size=640x360:rate=10 -frames:v 2 -pix_fmt yuv420p -c:v libx264 -profile:v high -f h264 a.h264
//	ffmpeg -f lavfi -i testsrc=size=320x240:rate=10 -frames:v 2 -pix_fmt yuv420p -c:v libx264 -profile:v baseline -f h264 b.h264
//	ffmpeg -f lavfi -i testsrc=size=640x360:rate=10 -frames:v 2 -pix_fmt yuv420p -c:v libx265 -f hevc a.h265
//	ffmpeg -f lavfi -i testsrc=size=320x240:rate=10 -frames:v 2 -pix_fmt yuv420p -c:v libx265 -f hevc b.h265
//
// "Small" is 320x240 and "large" 640x360. A resolution lie uses whichever of
// the two differs from the camera's real geometry; a sprop lie uses the large
// set whole, which also differs from the honest profile and level.
const (
	lieSPSSmall264 = "Z0LAC9kBQfsBEAAAAwAQAAADAUDxQqSA"
	lieSPSLarge264 = "Z2QAFqzZQKAv+XARAAADAAEAAAMAFA8WLZY="
	lieSPropPPS264 = "aOvjyyLA"

	lieVPSLarge265 = "QAEMAf//AWAAAAMAkAAAAwAAAwA/lZgJ"
	lieSPSLarge265 = "QgEBAWAAAAMAkAAAAwAAAwA/oAUCAWlllZpJMrwFoCAAAAMAIAAAAwFB"
	liePPSLarge265 = "RAHBcrRiQA=="
	lieSPSSmall265 = "QgEBAWAAAAMAkAAAAwAAAwA8oAoIDxZZWaSTK8BaAgAAAwACAAADABQQ"
)

var (
	reRtpmapCodec = regexp.MustCompile(`(a=rtpmap:\d+ )(H26[45])(/)`)
	reSpropSets   = regexp.MustCompile(`sprop-parameter-sets=([^;\r\n]*)`)
	reSpropVPS    = regexp.MustCompile(`sprop-vps=[^;\r\n]*`)
	reSpropSPS    = regexp.MustCompile(`sprop-sps=([^;\r\n]*)`)
	reSpropPPS    = regexp.MustCompile(`sprop-pps=[^;\r\n]*`)
)

// rewriteSDP makes an honest DESCRIBE body lie as mode says. codec is the
// camera's real codec. It is a pure function of its arguments. Only the
// a=rtpmap and a=fmtp attributes are touched: a=control, the line endings and
// everything else are preserved, so SETUP against the advertised control URL
// keeps working. contentBase is a header lie and is not handled here.
func rewriteSDP(body []byte, mode string, codec media.Codec) ([]byte, error) {
	s := string(body)
	switch mode {
	case "codec":
		if !reRtpmapCodec.MatchString(s) {
			return nil, fmt.Errorf("rtsp: sdp_lie codec: no H.264 or H.265 rtpmap in the SDP")
		}
		to := "H265"
		if codec == media.CodecH265 {
			to = "H264"
		}
		return []byte(reRtpmapCodec.ReplaceAllString(s, "${1}"+to+"${3}")), nil
	case "sprop":
		switch codec {
		case media.CodecH264:
			if !reSpropSets.MatchString(s) {
				return nil, fmt.Errorf("rtsp: sdp_lie sprop: no sprop-parameter-sets in the SDP")
			}
			return []byte(reSpropSets.ReplaceAllString(s, "sprop-parameter-sets="+lieSPSLarge264+","+lieSPropPPS264)), nil
		case media.CodecH265:
			if !reSpropVPS.MatchString(s) || !reSpropSPS.MatchString(s) || !reSpropPPS.MatchString(s) {
				return nil, fmt.Errorf("rtsp: sdp_lie sprop: sprop-vps, sprop-sps or sprop-pps missing from the SDP")
			}
			s = reSpropVPS.ReplaceAllString(s, "sprop-vps="+lieVPSLarge265)
			s = reSpropSPS.ReplaceAllString(s, "sprop-sps="+lieSPSLarge265)
			return []byte(reSpropPPS.ReplaceAllString(s, "sprop-pps="+liePPSLarge265)), nil
		}
	case "resolution":
		switch codec {
		case media.CodecH264:
			m := reSpropSets.FindStringSubmatch(s)
			if m == nil {
				return nil, fmt.Errorf("rtsp: sdp_lie resolution: no sprop-parameter-sets in the SDP")
			}
			sps, pps, ok := strings.Cut(m[1], ",")
			if !ok {
				return nil, fmt.Errorf("rtsp: sdp_lie resolution: sprop-parameter-sets %q is not an SPS,PPS pair", m[1])
			}
			lie, err := differentH264SPS(sps)
			if err != nil {
				return nil, err
			}
			return []byte(reSpropSets.ReplaceAllString(s, "sprop-parameter-sets="+lie+","+pps)), nil
		case media.CodecH265:
			m := reSpropSPS.FindStringSubmatch(s)
			if m == nil {
				return nil, fmt.Errorf("rtsp: sdp_lie resolution: no sprop-sps in the SDP")
			}
			lie, err := differentH265SPS(m[1])
			if err != nil {
				return nil, err
			}
			return []byte(reSpropSPS.ReplaceAllString(s, "sprop-sps="+lie)), nil
		}
	default:
		return nil, fmt.Errorf("rtsp: sdp_lie: unknown mode %q", mode)
	}
	return nil, fmt.Errorf("rtsp: sdp_lie %s: unsupported codec %q", mode, codec)
}

func h264Size(b64 string) (w, h int, err error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return 0, 0, err
	}
	var sps h264.SPS
	if err := sps.Unmarshal(raw); err != nil {
		return 0, 0, err
	}
	return sps.Width(), sps.Height(), nil
}

func h265Size(b64 string) (w, h int, err error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return 0, 0, err
	}
	var sps h265.SPS
	if err := sps.Unmarshal(raw); err != nil {
		return 0, 0, err
	}
	return sps.Width(), sps.Height(), nil
}

// differentH264SPS returns the embedded SPS whose geometry differs from the
// honest one given as base64.
func differentH264SPS(honest string) (string, error) {
	hw, hh, err := h264Size(honest)
	if err != nil {
		return "", fmt.Errorf("rtsp: sdp_lie resolution: honest SPS: %w", err)
	}
	if w, h, err := h264Size(lieSPSSmall264); err == nil && (w != hw || h != hh) {
		return lieSPSSmall264, nil
	}
	return lieSPSLarge264, nil
}

func differentH265SPS(honest string) (string, error) {
	hw, hh, err := h265Size(honest)
	if err != nil {
		return "", fmt.Errorf("rtsp: sdp_lie resolution: honest SPS: %w", err)
	}
	if w, h, err := h265Size(lieSPSSmall265); err == nil && (w != hw || h != hh) {
		return lieSPSSmall265, nil
	}
	return lieSPSLarge265, nil
}

// lieContentBase swaps the host of a Content-Base for one that cannot resolve
// and keeps the path, so a client that resolves relative control URLs against
// it ends up somewhere other than the camera. An unparseable base is returned
// unchanged.
func lieContentBase(orig string) string {
	u, err := url.Parse(orig)
	if err != nil || u.Host == "" {
		return orig
	}
	u.Host = "wrong.invalid"
	return u.String()
}
