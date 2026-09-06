package media

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
)

// These constants describe the committed fixture. They are measured, not
// chosen. If scripts/gen-fixture.sh is re-run on a different ffmpeg build and
// these change, update them deliberately.
const (
	fixtureWidth      = 320
	fixtureHeight     = 240
	fixtureFPS        = 15.0
	fixtureAUs        = 30
	fixtureProfileIdc = 66 // baseline
)

// fixtureKeyframes are the access-unit indices that can be randomly accessed.
// -g 15 at 15 fps over 2 seconds puts an IDR at 0 and 15.
var fixtureKeyframes = []int{0, 15}

func TestFixtureSourceProperties(t *testing.T) {
	m, err := (FixtureSource{}).Load()
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}

	if m.Codec != CodecH264 {
		t.Errorf("codec = %q, want %q", m.Codec, CodecH264)
	}
	if m.Width != fixtureWidth || m.Height != fixtureHeight {
		t.Errorf("geometry = %dx%d, want %dx%d", m.Width, m.Height, fixtureWidth, fixtureHeight)
	}
	if m.FPS != fixtureFPS {
		t.Errorf("fps = %v, want %v", m.FPS, fixtureFPS)
	}
	if len(m.AUs) != fixtureAUs {
		t.Errorf("access units = %d, want %d", len(m.AUs), fixtureAUs)
	}
	if len(m.SPS) == 0 {
		t.Error("SPS is empty")
	}
	if len(m.PPS) == 0 {
		t.Error("PPS is empty")
	}

	// Baseline profile is deliberate: it maximises decoder compatibility for a
	// fixture whose whole job is to be consumed by arbitrary client code.
	var sps h264.SPS
	if err := sps.Unmarshal(m.SPS); err != nil {
		t.Fatalf("unmarshal SPS: %v", err)
	}
	if sps.ProfileIdc != fixtureProfileIdc {
		t.Errorf("profile_idc = %d, want %d (baseline)", sps.ProfileIdc, fixtureProfileIdc)
	}

	var keyframes []int
	for i, au := range m.AUs {
		if au.RandomAccess {
			keyframes = append(keyframes, i)
		}
	}
	if len(keyframes) != len(fixtureKeyframes) {
		t.Fatalf("keyframes = %v, want %v", keyframes, fixtureKeyframes)
	}
	for i := range keyframes {
		if keyframes[i] != fixtureKeyframes[i] {
			t.Fatalf("keyframes = %v, want %v", keyframes, fixtureKeyframes)
		}
	}

	// A client that joins mid-stream needs an IDR to decode anything, so the
	// very first access unit must be one.
	if !m.AUs[0].RandomAccess {
		t.Error("first access unit is not a keyframe")
	}

	// Every AU must carry at least one non-empty NALU, or the RTP encoder panics.
	for i, au := range m.AUs {
		if len(au.NALUs) == 0 {
			t.Fatalf("access unit %d has no NALUs", i)
		}
		for j, n := range au.NALUs {
			if len(n) == 0 {
				t.Fatalf("access unit %d NALU %d is empty", i, j)
			}
		}
	}
}

// TestFixtureLoadsDoNotShareBackingMemory asserts only what it can actually
// detect: that two independent loads of the fixture own separate memory, so
// mutating one can never observably disturb the other. It does not, on its
// own, prove that copyNALUs's per-NALU copy is what causes this -- two fresh
// parses of the same bytes would look identical here even if copyNALUs
// aliased its input, because each parse still reads from its own io.Reader.
// The copy's own behaviour is proven directly by TestCopyNALUsDoesNotAliasInput
// in mpegts_test.go, which mutates the exact buffer copyNALUs was given.
func TestFixtureLoadsDoNotShareBackingMemory(t *testing.T) {
	m1, err := (FixtureSource{}).Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	m2, err := (FixtureSource{}).Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	// Mutating one load must not disturb another.
	m1.AUs[0].NALUs[0][0] ^= 0xFF
	if m2.AUs[0].NALUs[0][0] == m1.AUs[0].NALUs[0][0] {
		t.Fatal("two loads share backing memory")
	}
}

func TestFixtureTimestampsAreMonotonic(t *testing.T) {
	m, err := (FixtureSource{}).Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for i := 1; i < len(m.AUs); i++ {
		if m.AUs[i].DTS <= m.AUs[i-1].DTS {
			t.Fatalf("DTS not increasing at %d: %d then %d", i, m.AUs[i-1].DTS, m.AUs[i].DTS)
		}
	}
	if m.AUs[0].DTS != 0 {
		t.Errorf("first DTS = %d, want 0 (decoder rebases to the first timestamp)", m.AUs[0].DTS)
	}
}

func TestFrameDurationMatchesFPS(t *testing.T) {
	m, err := (FixtureSource{}).Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// 90 kHz / 15 fps = 6000.
	if got := m.FrameDuration(); got != 6000 {
		t.Errorf("FrameDuration() = %d, want 6000", got)
	}
}

// TestFrameDurationFallsBackToMeanDTSDelta exercises the branch the fixture
// never reaches: FPS == 0 (no timing declared by the bitstream), with at
// least two access units, so FrameDuration must derive spacing from the mean
// DTS delta instead.
func TestFrameDurationFallsBackToMeanDTSDelta(t *testing.T) {
	m := &Media{
		AUs: []AccessUnit{
			{DTS: 0},
			{DTS: 3000},
			{DTS: 6000},
			{DTS: 9000},
		},
	}
	// span = 9000 - 0 = 9000 over 3 gaps = 3000.
	if got := m.FrameDuration(); got != 3000 {
		t.Errorf("FrameDuration() = %d, want 3000", got)
	}
}

// TestFrameDurationFallsBackToDefaultBelowTwoAUs covers the len(AUs) < 2
// guard: with no timing declared and fewer than two access units, there is no
// delta to measure, so FrameDuration must return the hardcoded 90kHz/30fps
// default rather than divide by zero.
func TestFrameDurationFallsBackToDefaultBelowTwoAUs(t *testing.T) {
	const want = 90000 / 30

	for name, m := range map[string]*Media{
		"zero AUs": {},
		"one AU":   {AUs: []AccessUnit{{DTS: 1234}}},
	} {
		if got := m.FrameDuration(); got != want {
			t.Errorf("%s: FrameDuration() = %d, want %d", name, got, want)
		}
	}
}

func TestFileSourceMatchesFixtureSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "copy.ts")
	if err := os.WriteFile(path, fixtureBytesForTest(t), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	fromFile, err := (&FileSource{Path: path}).Load()
	if err != nil {
		t.Fatalf("file load: %v", err)
	}
	fromEmbed, err := (FixtureSource{}).Load()
	if err != nil {
		t.Fatalf("fixture load: %v", err)
	}
	if len(fromFile.AUs) != len(fromEmbed.AUs) {
		t.Fatalf("file gave %d AUs, embed gave %d", len(fromFile.AUs), len(fromEmbed.AUs))
	}
	if fromFile.Width != fromEmbed.Width || fromFile.Height != fromEmbed.Height {
		t.Fatal("geometry differs between file and embedded source")
	}
}

func TestFileSourceMissingFile(t *testing.T) {
	_, err := (&FileSource{Path: filepath.Join(t.TempDir(), "nope.ts")}).Load()
	if err == nil {
		t.Fatal("expected an error for a missing file")
	}
}

func TestSourcesReportDeterminism(t *testing.T) {
	if !(FixtureSource{}).Deterministic() {
		t.Error("fixture source must be deterministic")
	}
	if !(&FileSource{Path: "x.ts"}).Deterministic() {
		t.Error("file source must be deterministic")
	}
	if (FixtureSource{}).Describe() == "" {
		t.Error("Describe() must not be empty")
	}
}

func fixtureBytesForTest(t *testing.T) []byte {
	t.Helper()
	b := fixtureData()
	if len(b) == 0 {
		t.Fatal("embedded fixture is empty")
	}
	return b
}

// These constants describe the committed H.265 fixture. Like the H.264 set
// above they are measured, not chosen. Step 6 of this task is where they get
// their values; a guess here would be a test that asserts nothing.
const (
	fixtureH265Width  = 320
	fixtureH265Height = 240
	fixtureH265FPS    = 15.0
)

func TestFixtureH265SourceProperties(t *testing.T) {
	m, err := (FixtureH265Source{}).Load()
	if err != nil {
		t.Fatalf("load H265 fixture: %v", err)
	}

	if m.Codec != CodecH265 {
		t.Errorf("codec = %q, want %q", m.Codec, CodecH265)
	}
	if m.Width != fixtureH265Width || m.Height != fixtureH265Height {
		t.Errorf("geometry = %dx%d, want %dx%d", m.Width, m.Height, fixtureH265Width, fixtureH265Height)
	}
	if m.FPS != fixtureH265FPS {
		t.Errorf("fps = %v, want %v", m.FPS, fixtureH265FPS)
	}

	// All three parameter sets, which is what distinguishes H.265 from H.264
	// here. A source missing any of them must have been refused by Validate.
	if len(m.VPS) == 0 {
		t.Error("VPS is empty")
	}
	if len(m.SPS) == 0 {
		t.Error("SPS is empty")
	}
	if len(m.PPS) == 0 {
		t.Error("PPS is empty")
	}

	if len(m.AUs) == 0 {
		t.Fatal("no access units")
	}
	if !m.AUs[0].RandomAccess {
		t.Error("first access unit is not a random-access point")
	}
	if m.AUs[0].DTS != 0 {
		t.Errorf("first DTS = %d, want 0", m.AUs[0].DTS)
	}

	// More than one random-access point, which is the observable consequence of
	// handling open-GOP CRA keyframes. -g 15 over 2 seconds at 15 fps puts a
	// second keyframe mid-fixture; an implementation that recognised only IDR
	// would find exactly one and still pass every other assertion here.
	keyframes := 0
	for _, au := range m.AUs {
		if au.RandomAccess {
			keyframes++
		}
	}
	if keyframes < 2 {
		t.Errorf("random-access points = %d, want at least 2 (open-GOP CRA keyframes not recognised?)", keyframes)
	}

	for i, au := range m.AUs {
		if len(au.NALUs) == 0 {
			t.Fatalf("access unit %d has no NALUs", i)
		}
		for j, n := range au.NALUs {
			if len(n) == 0 {
				t.Fatalf("access unit %d NALU %d is empty", i, j)
			}
		}
	}
}

func TestFixtureH265ReportsDeterminism(t *testing.T) {
	if !(FixtureH265Source{}).Deterministic() {
		t.Error("H265 fixture source must be deterministic")
	}
	if (FixtureH265Source{}).Describe() == "" {
		t.Error("Describe() must not be empty")
	}
}
