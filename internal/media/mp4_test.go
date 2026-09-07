package media

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	mp4codecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/pmp4"
)

// remuxFixture writes MPEG-TS fixture bytes to a temp file and remuxes them into
// another container with ffmpeg, copying the video bitstream.
//
// `-c:v copy` is what makes these tests meaningful: no re-encode happens, so the
// NALU payloads are byte-identical on both sides and any difference in parsed
// output is a bug in this package rather than in an encoder. `-an` drops audio,
// which this package does not model.
//
// ffmpeg is a required test dependency, installed in CI, so a missing binary
// fails rather than skips: a test that quietly skips when its oracle is absent
// is worse than no test.
func remuxFixture(t *testing.T, src []byte, name string, extraArgs ...string) string {
	t.Helper()
	dir := t.TempDir()
	in := filepath.Join(dir, "in.ts")
	if err := os.WriteFile(in, src, 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}
	out := filepath.Join(dir, name)

	args := []string{"-hide_banner", "-loglevel", "error", "-y", "-i", in, "-c:v", "copy", "-an"}
	args = append(args, extraArgs...)
	args = append(args, out)

	if b, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg %v: %v\n%s", args, err, b)
	}
	return out
}

// parseISOBMFFFile is the path most tests take: open a remuxed file and parse
// it as ISOBMFF directly, without going through container sniffing, which
// arrives in a later task.
func parseISOBMFFFile(t *testing.T, path string) *Media {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	m, err := parseISOBMFF(f)
	if err != nil {
		t.Fatalf("parseISOBMFF(%s): %v", filepath.Base(path), err)
	}
	return m
}

func TestRescaleMultipliesBeforeDividing(t *testing.T) {
	// A timescale of 1000 and a value of 1 is 90 ticks at 90 kHz. Dividing first
	// would yield 0, which is the precision loss this ordering exists to avoid.
	if got := rescale(1, 1000); got != 90 {
		t.Errorf("rescale(1, 1000) = %d, want 90", got)
	}
	// 90 kHz in, 90 kHz out.
	if got := rescale(3000, 90000); got != 3000 {
		t.Errorf("rescale(3000, 90000) = %d, want 3000", got)
	}
	// A 600-tick timescale, common in MP4: one frame at 25 fps is 24 ticks.
	if got := rescale(24, 600); got != 3600 {
		t.Errorf("rescale(24, 600) = %d, want 3600", got)
	}
}

func TestParsePlainMP4H264(t *testing.T) {
	m := parseISOBMFFFile(t, remuxFixture(t, fixtureBytesForTest(t), "out.mp4"))

	if m.Codec != CodecH264 {
		t.Errorf("codec = %q, want %q", m.Codec, CodecH264)
	}
	if m.Width != fixtureWidth || m.Height != fixtureHeight {
		t.Errorf("geometry = %dx%d, want %dx%d", m.Width, m.Height, fixtureWidth, fixtureHeight)
	}
	if len(m.SPS) == 0 || len(m.PPS) == 0 {
		t.Error("parameter sets missing; avcC extraction failed")
	}
	if m.VPS != nil {
		t.Errorf("VPS = % x, want nil for H.264", m.VPS)
	}
	if len(m.AUs) != fixtureAUs {
		t.Errorf("access units = %d, want %d", len(m.AUs), fixtureAUs)
	}
	if m.AUs[0].DTS != 0 {
		t.Errorf("first DTS = %d, want 0", m.AUs[0].DTS)
	}
	if !m.AUs[0].RandomAccess {
		t.Error("first access unit is not a random-access point")
	}
}

func TestParsePlainMP4H265(t *testing.T) {
	m := parseISOBMFFFile(t, remuxFixture(t, fixtureH265BytesForTest(t), "out.mp4"))

	if m.Codec != CodecH265 {
		t.Errorf("codec = %q, want %q", m.Codec, CodecH265)
	}
	// All three parameter sets, proving hvcC extraction reached VPS as well.
	if len(m.VPS) == 0 || len(m.SPS) == 0 || len(m.PPS) == 0 {
		t.Errorf("parameter sets: vps=%d sps=%d pps=%d, want all non-empty",
			len(m.VPS), len(m.SPS), len(m.PPS))
	}
	if m.Width != fixtureH265Width || m.Height != fixtureH265Height {
		t.Errorf("geometry = %dx%d, want %dx%d", m.Width, m.Height, fixtureH265Width, fixtureH265Height)
	}
}

// TestParseMP4NoVideoTrack proves an audio-only MP4 is refused with the same
// error a video-less MPEG-TS gets, rather than producing empty media.
func TestParseMP4NoVideoTrack(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "audio.mp4")
	args := []string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=1",
		"-c:a", "aac", out,
	}
	if b, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v\n%s", err, b)
	}

	f, err := os.Open(out)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	if _, err := parseISOBMFF(f); err == nil {
		t.Fatal("expected an error for an MP4 with no video track")
	}
}

// TestMediaFromPMP4NoSamplesIsRecognisable pins the errNoSamples contract that
// Task 5 depends on: a video track that parses structurally but carries zero
// samples must produce an error errors.Is recognises as errNoSamples, since
// that is exactly the signal the fragmented-MP4 reader uses to decide whether
// to try harder rather than surface a confusing error to a caller.
//
// This is built by hand rather than through ffmpeg: mediacommon's own reader
// has no way to produce a structurally-valid track with no samples (a real
// plain MP4 always has at least one), so the only way to exercise this branch
// at all is to construct the pmp4.Presentation directly.
func TestMediaFromPMP4NoSamplesIsRecognisable(t *testing.T) {
	pres := &pmp4.Presentation{
		Tracks: []*pmp4.Track{
			{
				ID:        1,
				TimeScale: 90000,
				Codec:     &mp4codecs.H264{SPS: []byte{0x01}, PPS: []byte{0x02}},
				Samples:   nil,
			},
		},
	}

	_, err := mediaFromPMP4(pres)
	if !errors.Is(err, errNoSamples) {
		t.Fatalf("mediaFromPMP4() error = %v, want errors.Is(err, errNoSamples)", err)
	}
}

// fragFlags makes ffmpeg emit a fragmented MP4: an empty moov followed by one
// moof/mdat pair per keyframe-led fragment.
var fragFlags = []string{"-movflags", "+frag_keyframe+empty_moov"}

func TestParseFragmentedMP4H264(t *testing.T) {
	m := parseISOBMFFFile(t, remuxFixture(t, fixtureBytesForTest(t), "out.mp4", fragFlags...))

	if m.Codec != CodecH264 {
		t.Errorf("codec = %q, want %q", m.Codec, CodecH264)
	}
	if m.Width != fixtureWidth || m.Height != fixtureHeight {
		t.Errorf("geometry = %dx%d, want %dx%d", m.Width, m.Height, fixtureWidth, fixtureHeight)
	}
	if len(m.AUs) != fixtureAUs {
		t.Errorf("access units = %d, want %d", len(m.AUs), fixtureAUs)
	}
	if m.AUs[0].DTS != 0 {
		t.Errorf("first DTS = %d, want 0", m.AUs[0].DTS)
	}
}

// TestParseFragmentedMP4RandomAccess pins the outcome that assemble's
// container/bitstream cross-check would normally guard, since this path
// cannot use that check: see the SyncKnown: false comment in mediaFromFMP4.
// The codec adapter is the only source of the random-access decision here,
// so this asserts its answer directly rather than trusting a container
// declaration mediacommon cannot reliably surface on this path.
func TestParseFragmentedMP4RandomAccess(t *testing.T) {
	m := parseISOBMFFFile(t, remuxFixture(t, fixtureBytesForTest(t), "out.mp4", fragFlags...))

	want := map[int]bool{}
	for _, k := range fixtureKeyframes {
		want[k] = true
	}
	for i, au := range m.AUs {
		if au.RandomAccess != want[i] {
			t.Errorf("AU %d: RandomAccess = %v, want %v", i, au.RandomAccess, want[i])
		}
	}
}

func TestParseFragmentedMP4H265(t *testing.T) {
	m := parseISOBMFFFile(t, remuxFixture(t, fixtureH265BytesForTest(t), "out.mp4", fragFlags...))

	if m.Codec != CodecH265 {
		t.Errorf("codec = %q, want %q", m.Codec, CodecH265)
	}
	if len(m.VPS) == 0 || len(m.SPS) == 0 || len(m.PPS) == 0 {
		t.Errorf("parameter sets: vps=%d sps=%d pps=%d, want all non-empty",
			len(m.VPS), len(m.SPS), len(m.PPS))
	}
}

// TestFragmentedTimestampsAreMonotonic covers the join between fragments, which
// is where a per-fragment DTS reset would show up: each moof carries a tfdt
// baseTime, and ignoring it in favour of restarting at zero would make the
// second fragment's timestamps go backwards.
func TestParseFragmentedMP4TimestampsAreMonotonic(t *testing.T) {
	m := parseISOBMFFFile(t, remuxFixture(t, fixtureBytesForTest(t), "out.mp4", fragFlags...))
	for i := 1; i < len(m.AUs); i++ {
		if m.AUs[i].DTS <= m.AUs[i-1].DTS {
			t.Fatalf("DTS not increasing at %d: %d then %d (fragment boundary handled wrong?)",
				i, m.AUs[i-1].DTS, m.AUs[i].DTS)
		}
	}
}
