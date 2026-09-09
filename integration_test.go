package camfarm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ffprobeCodecName maps a fleet-advertised codec to the name ffprobe reports for
// it. ffprobe calls H.265 "hevc", so comparing against the advertised string
// directly would fail for a perfectly good stream.
func ffprobeCodecName(advertised string) string {
	switch advertised {
	case "H264":
		return "h264"
	case "H265":
		return "hevc"
	default:
		return strings.ToLower(advertised)
	}
}

// TestFFprobeDecodesEveryCamera is the external oracle. A stream that an
// independent decoder cannot play is broken however well it parses.
func TestFFprobeDecodesEveryCamera(t *testing.T) {
	h265Path := filepath.Join(t.TempDir(), "h265.ts")
	if err := os.WriteFile(h265Path, h265FixtureBytes(t), 0o600); err != nil {
		t.Fatalf("write H265 fixture: %v", err)
	}

	f := StartT(t, Spec{
		Seed: 0x3f2a9c81,
		Cameras: []CameraSpec{
			{ID: "front-door"},
			{ID: "lobby"},
			{ID: "hevc-cam", Source: SourceSpec{Kind: SourceFile, Path: h265Path}},
		},
	})

	for _, st := range f.List() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		out, err := exec.CommandContext(ctx, "ffprobe",
			"-v", "error",
			"-rtsp_transport", "tcp",
			"-i", st.RTSPURL,
			"-select_streams", "v:0",
			"-show_entries", "stream=codec_name,width,height,pix_fmt",
			"-read_intervals", "%+#20",
			"-of", "json",
		).CombinedOutput()
		cancel()
		if err != nil {
			t.Fatalf("%s: ffprobe failed: %v\n%s\n%s", st.ID, err, out, f.ReplayLine())
		}

		var got struct {
			Streams []struct {
				CodecName string `json:"codec_name"`
				Width     int    `json:"width"`
				Height    int    `json:"height"`
				PixFmt    string `json:"pix_fmt"`
			} `json:"streams"`
		}
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("%s: parsing ffprobe output: %v\n%s", st.ID, err, out)
		}
		if len(got.Streams) != 1 {
			t.Fatalf("%s: ffprobe saw %d video streams, want 1\n%s", st.ID, len(got.Streams), out)
		}
		s := got.Streams[0]
		if want := ffprobeCodecName(st.Codec); s.CodecName != want {
			t.Errorf("%s: ffprobe decoded codec %q, want %q", st.ID, s.CodecName, want)
		}
		// The decoded geometry must match what the fleet advertised. A mismatch
		// here would be the sps_resolution_lie fault firing by accident, which
		// is exactly the failure mode this project exists to make deliberate.
		if s.Width != st.Width || s.Height != st.Height {
			t.Errorf("%s: ffprobe decoded %dx%d but the fleet advertised %dx%d",
				st.ID, s.Width, s.Height, st.Width, st.Height)
		}
	}
}

// A fleet must survive more cameras than a developer would open by hand. This
// is not the measured ceiling spec section 10.2 calls for -- that needs a
// benchmark -- but it proves the model holds well past one.
func TestTwentyFiveCamerasOnOneListener(t *testing.T) {
	const n = 25

	spec := Spec{Seed: 0x3f2a9c81}
	for i := 0; i < n; i++ {
		spec.Cameras = append(spec.Cameras, CameraSpec{ID: fmt.Sprintf("cam-%02d", i)})
	}
	f := StartT(t, spec)

	list := f.List()
	if len(list) != n {
		t.Fatalf("List = %d, want %d", len(list), n)
	}

	// One listener, so every camera shares a port and differs only by path.
	port := f.Addr().RTSP.Port
	seen := make(map[string]bool, n)
	for _, st := range list {
		if seen[st.RTSPURL] {
			t.Fatalf("duplicate URL %q", st.RTSPURL)
		}
		seen[st.RTSPURL] = true
	}
	if got := f.Addr().RTSP.Port; got != port {
		t.Fatal("the fleet moved ports mid-test")
	}

	// Every camera must be independently and deterministically seeded.
	seeds := make(map[uint64]string, n)
	for _, st := range list {
		cam, err := f.Camera(st.ID)
		if err != nil {
			t.Fatalf("Camera(%q): %v", st.ID, err)
		}
		s := cam.Stats().Seed
		if prev, dup := seeds[s]; dup {
			t.Fatalf("%s and %s share seed %#x", st.ID, prev, s)
		}
		seeds[s] = st.ID
	}
}

// The whole point: the same seed and spec give the same camera seeds, so a
// recorded failure can be reconstructed.
func TestSameSeedReproducesCameraSeeds(t *testing.T) {
	h265Path := filepath.Join(t.TempDir(), "h265.ts")
	if err := os.WriteFile(h265Path, h265FixtureBytes(t), 0o600); err != nil {
		t.Fatalf("write H265 fixture: %v", err)
	}

	spec := Spec{
		Seed: 0xdeadbeefcafe,
		Cameras: []CameraSpec{
			{ID: "a"},
			{ID: "b"},
			{ID: "c"},
			{ID: "d", Source: SourceSpec{Kind: SourceFile, Path: h265Path}},
		},
	}

	collect := func() (map[string]uint64, string) {
		f := StartT(t, spec)
		out := map[string]uint64{}
		for _, st := range f.List() {
			cam, err := f.Camera(st.ID)
			if err != nil {
				t.Fatalf("Camera: %v", err)
			}
			out[st.ID] = cam.Stats().Seed
		}
		return out, f.SpecHash()
	}

	first, hash1 := collect()
	second, hash2 := collect()

	if hash1 != hash2 {
		t.Errorf("spec hash differs between runs: %s vs %s", hash1, hash2)
	}
	for id, want := range first {
		if got := second[id]; got != want {
			t.Errorf("%s: seed %#x then %#x", id, want, got)
		}
	}

	// A different root seed must move every camera.
	other := spec
	other.Seed = spec.Seed + 1
	f := StartT(t, other)
	for _, st := range f.List() {
		cam, err := f.Camera(st.ID)
		if err != nil {
			t.Fatalf("Camera: %v", err)
		}
		if cam.Stats().Seed == first[st.ID] {
			t.Errorf("%s kept seed %#x across a root-seed change", st.ID, first[st.ID])
		}
	}
}

// Frames must actually be served over time, not just on the first tick.
func TestFramesAccumulate(t *testing.T) {
	f := StartT(t, Spec{Seed: 1, Cameras: []CameraSpec{{ID: "a"}}})

	cam, err := f.Camera("a")
	if err != nil {
		t.Fatalf("Camera: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	var first uint64
	for {
		first = cam.Stats().FramesServed
		if first > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if first == 0 {
		t.Fatalf("no frames served within 10s\n%s", f.ReplayLine())
	}

	for {
		if cam.Stats().FramesServed > first || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := cam.Stats().FramesServed; got <= first {
		t.Fatalf("frames stalled at %d\n%s", got, f.ReplayLine())
	}

	// No faults were configured, so none may have fired.
	if got := cam.Stats().FaultsFired; got != 0 {
		t.Errorf("FaultsFired = %d, want 0", got)
	}
}
