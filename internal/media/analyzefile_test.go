package media

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/0x524a/camfarm/internal/media/fixture"
)

func writeTemp(t *testing.T, name string, b []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func firstInvalid(t *testing.T, r Report) Issue {
	t.Helper()
	if r.Status != StatusInvalid {
		t.Fatalf("status = %q, want invalid (issues %v)", r.Status, r.Issues)
	}
	for _, is := range r.Issues {
		if is.Severity == SeverityInvalid {
			return is
		}
	}
	t.Fatal("status invalid but no invalid issue")
	return Issue{}
}

func TestAnalyzeFileBundledFixtures(t *testing.T) {
	for name, b := range map[string][]byte{"h264": fixture.Bytes(), "h265": fixture.BytesH265()} {
		r := AnalyzeFile(writeTemp(t, name+".ts", b))
		if r.Status != StatusValid || r.Facts.Container != string(containerMPEGTS) || r.Facts.Frames != 30 {
			t.Errorf("%s: %+v", name, r)
		}
		if r.Facts.SizeBytes != int64(len(b)) {
			t.Errorf("%s: SizeBytes = %d, want %d", name, r.Facts.SizeBytes, len(b))
		}
	}
}

func TestAnalyzeFileFailures(t *testing.T) {
	trunc := fixture.Bytes()[:188*5+17] // cut mid-packet
	cases := []struct {
		name string
		path string
		code string
	}{
		{"missing file", filepath.Join(t.TempDir(), "nope.ts"), "unreadable"},
		{"empty file", writeTemp(t, "empty.ts", nil), "unsupported_container"},
		{"random bytes", writeTemp(t, "noise.mp4", []byte("this is not video, it is a sentence of text")), "unsupported_container"},
		{"directory", t.TempDir(), "unreadable"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := firstInvalid(t, AnalyzeFile(c.path)); got.Code != c.code {
				t.Errorf("code = %q, want %q (%+v)", got.Code, c.code, got)
			}
		})
	}
	t.Run("truncated TS is invalid or valid-with-fewer-frames, never a panic", func(t *testing.T) {
		r := AnalyzeFile(writeTemp(t, "cut.ts", trunc))
		if r.Status == "" {
			t.Fatal("no status")
		}
	})
}

// A file whose name lies about its container still loads; the report names the
// container it really is.
func TestAnalyzeFileIgnoresExtension(t *testing.T) {
	r := AnalyzeFile(writeTemp(t, "actually-ts.mp4", fixture.Bytes()))
	if r.Status != StatusValid || r.Facts.Container != string(containerMPEGTS) {
		t.Errorf("%+v", r)
	}
}

func TestAnalyzeFileAudioOnlyMP4(t *testing.T) {
	out := filepath.Join(t.TempDir(), "audio.mp4")
	args := []string{"-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "sine=frequency=440:duration=1", "-c:a", "aac", out}
	if b, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v\n%s", err, b)
	}
	got := firstInvalid(t, AnalyzeFile(out))
	if got.Code != "no_video" {
		t.Errorf("code = %q, want no_video (%+v)", got.Code, got)
	}
}

// Real B-frames, produced by an encoder, must be flagged. This is the check the
// synthetic tests cannot make: that isH264BSlice agrees with a real bitstream.
func TestAnalyzeFileRealBFrames(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "b.mp4")
	args := []string{"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size=320x240:rate=25:duration=2",
		"-c:v", "libx264", "-bf", "2", "-g", "25", "-pix_fmt", "yuv420p", out}
	if b, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v\n%s", err, b)
	}
	r := AnalyzeFile(out)
	codes := codesOf(r)
	if r.Status != StatusWarning || !contains(codes, "b_frames") || !contains(codes, "pts_dts_reordering") {
		t.Errorf("status %q codes %v facts %+v", r.Status, codes, r.Facts)
	}
	if r.Facts.Container != string(containerISOBMFF) {
		t.Errorf("container = %q", r.Facts.Container)
	}

	noB := filepath.Join(dir, "nob.mp4")
	args = []string{"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size=320x240:rate=25:duration=2",
		"-c:v", "libx264", "-bf", "0", "-g", "25", "-pix_fmt", "yuv420p", noB}
	if b, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v\n%s", err, b)
	}
	if r := AnalyzeFile(noB); contains(codesOf(r), "b_frames") {
		t.Errorf("no-B encode flagged: %v", codesOf(r))
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func TestClassifyLoadError(t *testing.T) {
	cases := map[string]error{
		"no_video":               ErrNoVideoTrack,
		"missing_parameter_sets": ErrNoParameterSetsH264,
		"no_frames":              ErrNoAccessUnits,
		"loader_error":           os.ErrInvalid,
	}
	for code, err := range cases {
		r := classifyLoadError(Facts{}, err)
		if got := firstInvalid(t, r); got.Code != code {
			t.Errorf("%v: code = %q, want %q", err, got.Code, code)
		}
	}
}

func TestRecoverFromParserPanic(t *testing.T) {
	r := reportFromPanic(Facts{Container: "MPEG-TS"}, "boom")
	got := firstInvalid(t, r)
	if got.Code != "loader_error" || got.Detail != "boom" || r.Facts.Container != "MPEG-TS" {
		t.Errorf("%+v", r)
	}
}
