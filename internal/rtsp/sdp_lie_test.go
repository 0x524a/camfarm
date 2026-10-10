package rtsp

import (
	"encoding/base64"
	"flag"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h265"

	"github.com/0x524a/camfarm/internal/media"
)

// updateSDP regenerates testdata/describe_*.sdp from a live DESCRIBE:
//
//	go test ./internal/rtsp -run TestHonestSDPMatchesTheGoldenFiles -update-sdp
var updateSDP = flag.Bool("update-sdp", false, "rewrite testdata/describe_*.sdp from a live DESCRIBE")

func readSDP(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The golden files are what the unit tests rewrite, so they must be what the
// server really sends.
func TestHonestSDPMatchesTheGoldenFiles(t *testing.T) {
	h265m, err := (media.FixtureH265Source{}).Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		file string
		m    *media.Media
	}{{"describe_h264.sdp", testMedia(t)}, {"describe_h265.sdp", h265m}} {
		s, _ := startSDPServer(t, c.m, nil)
		got := describeRaw(t, s.URL("cam-00")).Body
		if *updateSDP {
			if err := os.WriteFile("testdata/"+c.file, []byte(got), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if want := readSDP(t, c.file); got != want {
			t.Fatalf("%s is stale (rerun with -update-sdp):\n got: %q\nwant: %q", c.file, got, want)
		}
	}
}

var (
	reRtpmap = regexp.MustCompile(`a=rtpmap:\d+ (\S+)`)
	reSprop  = regexp.MustCompile(`sprop-parameter-sets=([^;\r\n]*)`)
	reSps265 = regexp.MustCompile(`sprop-sps=([^;\r\n]*)`)
	reCtl    = regexp.MustCompile(`a=control:[^\r\n]*`)
)

func lines(s string, re *regexp.Regexp) []string { return re.FindAllString(s, -1) }

func h264Parts(t *testing.T, sdp string) (sps, pps []byte) {
	t.Helper()
	m := reSprop.FindStringSubmatch(sdp)
	if m == nil {
		t.Fatalf("no sprop-parameter-sets in:\n%s", sdp)
	}
	parts := strings.Split(m[1], ",")
	if len(parts) != 2 {
		t.Fatalf("sprop-parameter-sets %q is not a pair", m[1])
	}
	dec := func(s string) []byte {
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			t.Fatalf("sprop element %q is not base64: %v", s, err)
		}
		return b
	}
	return dec(parts[0]), dec(parts[1])
}

func h264Dims(t *testing.T, sps []byte) (w, h int) {
	t.Helper()
	var s h264.SPS
	if err := s.Unmarshal(sps); err != nil {
		t.Fatalf("SPS does not parse: %v", err)
	}
	return s.Width(), s.Height()
}

func h265Dims(t *testing.T, sdp string) (w, h int) {
	t.Helper()
	m := reSps265.FindStringSubmatch(sdp)
	if m == nil {
		t.Fatalf("no sprop-sps in:\n%s", sdp)
	}
	raw, err := base64.StdEncoding.DecodeString(m[1])
	if err != nil {
		t.Fatal(err)
	}
	var s h265.SPS
	if err := s.Unmarshal(raw); err != nil {
		t.Fatalf("H265 SPS does not parse: %v", err)
	}
	return s.Width(), s.Height()
}

func TestRewriteSDPCodecSwapsTheRtpmapBothWays(t *testing.T) {
	for _, c := range []struct {
		file    string
		codec   media.Codec
		from    string
		to      string
		wantMap string
	}{
		{"describe_h264.sdp", media.CodecH264, "H264/90000", "H265/90000", "H265"},
		{"describe_h265.sdp", media.CodecH265, "H265/90000", "H264/90000", "H264"},
	} {
		honest := readSDP(t, c.file)
		if !strings.Contains(honest, c.from) {
			t.Fatalf("%s lacks %s", c.file, c.from)
		}
		out, err := rewriteSDP([]byte(honest), "codec", c.codec)
		if err != nil {
			t.Fatal(err)
		}
		got := string(out)
		if strings.Contains(got, c.from) || !strings.Contains(got, c.to) {
			t.Fatalf("%s codec lie did not swap %s for %s:\n%s", c.file, c.from, c.to, got)
		}
		if m := reRtpmap.FindStringSubmatch(got); m == nil || !strings.HasPrefix(m[1], c.wantMap) {
			t.Fatalf("rtpmap = %v", m)
		}
	}
}

func TestRewriteSDPSpropReplacesTheParameterSetsWithAnotherValidPair(t *testing.T) {
	honest := readSDP(t, "describe_h264.sdp")
	out, err := rewriteSDP([]byte(honest), "sprop", media.CodecH264)
	if err != nil {
		t.Fatal(err)
	}
	hSPS, hPPS := h264Parts(t, honest)
	sps, pps := h264Parts(t, string(out))
	if string(sps) == string(hSPS) || string(pps) == string(hPPS) {
		t.Fatal("sprop lie left the honest parameter sets in place")
	}
	if len(sps) == 0 || h264.NALUType(sps[0]&0x1F) != h264.NALUTypeSPS {
		t.Fatalf("lied SPS is not an SPS NAL unit: %x", sps)
	}
	if len(pps) == 0 || h264.NALUType(pps[0]&0x1F) != h264.NALUTypePPS {
		t.Fatalf("lied PPS is not a PPS NAL unit: %x", pps)
	}
	h264Dims(t, sps) // must still parse

	h265Out, err := rewriteSDP([]byte(readSDP(t, "describe_h265.sdp")), "sprop", media.CodecH265)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"sprop-vps=", "sprop-sps=", "sprop-pps="} {
		a := regexp.MustCompile(key + `([^;\r\n]*)`)
		h, l := a.FindStringSubmatch(readSDP(t, "describe_h265.sdp")), a.FindStringSubmatch(string(h265Out))
		if h == nil || l == nil || h[1] == l[1] {
			t.Fatalf("%s not replaced: honest %v lied %v", key, h, l)
		}
		if _, err := base64.StdEncoding.DecodeString(l[1]); err != nil {
			t.Fatalf("%s is not base64: %v", key, err)
		}
	}
	h265Dims(t, string(h265Out))
}

// The honest parameter sets may already be the constant lie; the SDP must still
// change.
func TestRewriteSDPSpropAlwaysChangesTheSDP(t *testing.T) {
	h264sdp := "a=rtpmap:96 H264/90000\r\na=fmtp:96 packetization-mode=1;sprop-parameter-sets=" + lieSPSLarge264 + "," + lieSPropPPS264 + "\r\n"
	h265sdp := "a=rtpmap:96 H265/90000\r\na=fmtp:96 sprop-vps=" + lieVPSLarge265 + ";sprop-sps=" + lieSPSLarge265 + ";sprop-pps=" + liePPSLarge265 + "\r\n"
	for _, c := range []struct {
		sdp   string
		codec media.Codec
	}{{h264sdp, media.CodecH264}, {h265sdp, media.CodecH265}} {
		out, err := rewriteSDP([]byte(c.sdp), "sprop", c.codec)
		if err != nil {
			t.Fatal(err)
		}
		if string(out) == c.sdp {
			t.Fatalf("%s sprop lie left an already-constant SDP unchanged", c.codec)
		}
	}
}

func TestRewriteSDPFailsWithoutSprop(t *testing.T) {
	if _, err := rewriteSDP([]byte("a=rtpmap:96 H264/90000\r\n"), "sprop", media.CodecH264); err == nil {
		t.Fatal("sprop lie on an SDP without sprop-parameter-sets succeeded")
	}
}

func TestRewriteSDPResolutionChangesTheParsedGeometry(t *testing.T) {
	honest := readSDP(t, "describe_h264.sdp")
	hSPS, hPPS := h264Parts(t, honest)
	hw, hh := h264Dims(t, hSPS)
	out, err := rewriteSDP([]byte(honest), "resolution", media.CodecH264)
	if err != nil {
		t.Fatal(err)
	}
	sps, pps := h264Parts(t, string(out))
	w, h := h264Dims(t, sps)
	if w == hw && h == hh {
		t.Fatalf("resolution lie kept %dx%d", w, h)
	}
	if string(pps) != string(hPPS) {
		t.Fatal("resolution lie must leave the PPS alone")
	}

	h265Honest := readSDP(t, "describe_h265.sdp")
	hw, hh = h265Dims(t, h265Honest)
	h265Out, err := rewriteSDP([]byte(h265Honest), "resolution", media.CodecH265)
	if err != nil {
		t.Fatal(err)
	}
	if w, h := h265Dims(t, string(h265Out)); w == hw && h == hh {
		t.Fatalf("H265 resolution lie kept %dx%d", w, h)
	}
}

// Whatever the camera's real geometry, the lie must differ from it.
func TestLieGeometryDiffersFromAnyHonestGeometry(t *testing.T) {
	for _, s := range []string{lieSPSSmall264, lieSPSLarge264} {
		raw, _ := base64.StdEncoding.DecodeString(s)
		w, h := h264Dims(t, raw)
		if w <= 0 || h <= 0 {
			t.Fatalf("bad lie geometry %dx%d", w, h)
		}
	}
	a, _ := base64.StdEncoding.DecodeString(lieSPSSmall264)
	b, _ := base64.StdEncoding.DecodeString(lieSPSLarge264)
	aw, ah := h264Dims(t, a)
	bw, bh := h264Dims(t, b)
	if aw == bw && ah == bh {
		t.Fatal("the two lie geometries must differ so one is always different from the honest one")
	}
}

func TestRewriteSDPLeavesControlLinesAndLineEndingsAlone(t *testing.T) {
	for _, file := range []string{"describe_h264.sdp", "describe_h265.sdp"} {
		codec := media.CodecH264
		if strings.Contains(file, "h265") {
			codec = media.CodecH265
		}
		honest := readSDP(t, file)
		// gortsplib writes CRLF for H.264 and bare LF for H.265; both are
		// preserved as found.
		if !strings.Contains(honest, "\n") {
			t.Fatalf("%s has no line endings; the test would prove nothing", file)
		}
		for _, mode := range []string{"codec", "sprop", "resolution"} {
			out, err := rewriteSDP([]byte(honest), mode, codec)
			if err != nil {
				t.Fatalf("%s %s: %v", file, mode, err)
			}
			got := string(out)
			if a, b := lines(honest, reCtl), lines(got, reCtl); strings.Join(a, "|") != strings.Join(b, "|") {
				t.Fatalf("%s %s changed a=control: %v -> %v", file, mode, a, b)
			}
			if strings.Count(got, "\r\n") != strings.Count(honest, "\r\n") || strings.Count(got, "\n") != strings.Count(honest, "\n") {
				t.Fatalf("%s %s changed the line endings", file, mode)
			}
			if got == honest {
				t.Fatalf("%s %s changed nothing", file, mode)
			}
		}
	}
}

func TestRewriteSDPRejectsWhatItCannotLieAbout(t *testing.T) {
	if _, err := rewriteSDP([]byte("v=0\r\n"), "nonsense", media.CodecH264); err == nil {
		t.Fatal("unknown mode must be an error")
	}
	if _, err := rewriteSDP([]byte("v=0\r\n"), "sprop", media.CodecH264); err == nil {
		t.Fatal("an SDP with no parameter sets cannot be lied about")
	}
}

func TestLieContentBaseKeepsThePathAndChangesTheHost(t *testing.T) {
	for in, want := range map[string]string{
		"rtsp://127.0.0.1:8554/cam-00/": "rtsp://wrong.invalid/cam-00/",
		"rtsp://cam.local/a/b":          "rtsp://wrong.invalid/a/b",
	} {
		if got := lieContentBase(in); got != want {
			t.Errorf("lieContentBase(%q) = %q, want %q", in, got, want)
		}
	}
	if got := lieContentBase("::not a url"); got != "::not a url" {
		t.Errorf("an unparseable base must be returned unchanged, got %q", got)
	}
}

// The embedded lie parameter sets were produced with ffmpeg and are pinned
// here, so a typo in a constant fails a test rather than a client.
func TestLieParameterSetsAreWellFormed(t *testing.T) {
	for name, c := range map[string]struct {
		b64  string
		kind h264.NALUType
	}{
		"small SPS": {lieSPSSmall264, h264.NALUTypeSPS},
		"large SPS": {lieSPSLarge264, h264.NALUTypeSPS},
		"large PPS": {lieSPropPPS264, h264.NALUTypePPS},
	} {
		raw, err := base64.StdEncoding.DecodeString(c.b64)
		if err != nil || len(raw) == 0 || h264.NALUType(raw[0]&0x1F) != c.kind {
			t.Errorf("%s is not a %v NAL unit: %v", name, c.kind, err)
		}
	}
}
