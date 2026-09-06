package camfarm

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
)

func minimalSpec() Spec {
	return Spec{
		Seed: 0x3f2a9c81,
		Cameras: []CameraSpec{
			{ID: "front-door"},
			{ID: "lobby"},
		},
	}
}

func TestStartServesTheFleetAndClosesCleanly(t *testing.T) {
	f, err := Start(context.Background(), minimalSpec())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	if got := f.Addr().RTSP; got == nil || got.Port == 0 {
		t.Fatalf("RTSP addr = %v, want a resolved ephemeral port", got)
	}

	list := f.List()
	if len(list) != 2 {
		t.Fatalf("List = %d cameras, want 2", len(list))
	}
	for _, st := range list {
		if !strings.HasPrefix(st.RTSPURL, "rtsp://") {
			t.Errorf("%s: RTSPURL = %q", st.ID, st.RTSPURL)
		}
		if st.Width != 320 || st.Height != 240 {
			t.Errorf("%s: geometry = %dx%d, want 320x240 from the bundled fixture", st.ID, st.Width, st.Height)
		}
		if st.Codec != "H264" {
			t.Errorf("%s: codec = %q", st.ID, st.Codec)
		}
	}

	// Close must be idempotent: a t.Cleanup and an explicit Close both fire.
	if err := f.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestCameraLookup(t *testing.T) {
	f, err := Start(context.Background(), minimalSpec())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })

	cam, err := f.Camera("front-door")
	if err != nil {
		t.Fatalf("Camera: %v", err)
	}
	if cam.ID() != "front-door" {
		t.Errorf("ID = %q", cam.ID())
	}
	if !strings.HasSuffix(cam.RTSPURL(), "/front-door") {
		t.Errorf("RTSPURL = %q", cam.RTSPURL())
	}

	if _, err := f.Camera("nope"); !errors.Is(err, ErrUnknownCamera) {
		t.Errorf("unknown camera: err = %v, want ErrUnknownCamera", err)
	}
}

func TestStatsReportSeedAndProgress(t *testing.T) {
	f, err := Start(context.Background(), minimalSpec())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })

	cam, err := f.Camera("front-door")
	if err != nil {
		t.Fatalf("Camera: %v", err)
	}
	st := cam.Stats()
	if st.Seed == 0 {
		t.Error("Stats.Seed is zero")
	}
	if st.Seed == f.Seed() {
		t.Error("camera seed equals the root seed; it must be derived")
	}
	// No faults are configured, so none can have fired.
	if st.FaultsFired != 0 {
		t.Errorf("FaultsFired = %d, want 0", st.FaultsFired)
	}
}

// Spec section 7.7: faults are catalogued and validated in this version, and
// refused rather than silently ignored.
func TestFaultsAreRefusedNotIgnored(t *testing.T) {
	s := minimalSpec()
	s.Cameras[0].Faults = []FaultSpec{{Kind: "frame_drop", Rate: 0.5}}

	_, err := Start(context.Background(), s)
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
	// The message must name the kind, so a caller knows what was refused.
	if !strings.Contains(err.Error(), "frame_drop") {
		t.Errorf("err = %v, want it to name the fault kind", err)
	}

	// An unknown kind is a different failure from an unimplemented one.
	s.Cameras[0].Faults = []FaultSpec{{Kind: "not-a-fault", Rate: 0.5}}
	if _, err := Start(context.Background(), s); err == nil {
		t.Fatal("unknown fault kind was accepted")
	}
}

func TestInjectAndClear(t *testing.T) {
	f, err := Start(context.Background(), minimalSpec())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })

	cam, err := f.Camera("front-door")
	if err != nil {
		t.Fatalf("Camera: %v", err)
	}
	if err := cam.Inject(FaultSpec{Kind: "frame_drop", Rate: 1}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Inject: err = %v, want ErrUnsupported", err)
	}
	// Clearing nothing succeeds: it is not an error to ask for the state that
	// already holds.
	if err := cam.Clear(); err != nil {
		t.Errorf("Clear: %v", err)
	}
}

func TestSpecValidation(t *testing.T) {
	cases := map[string]Spec{
		"no cameras":   {Seed: 1},
		"empty id":     {Seed: 1, Cameras: []CameraSpec{{ID: ""}}},
		"duplicate id": {Seed: 1, Cameras: []CameraSpec{{ID: "a"}, {ID: "a"}}},
		"slash in id":  {Seed: 1, Cameras: []CameraSpec{{ID: "a/b"}}},
		"bad source":   {Seed: 1, Cameras: []CameraSpec{{ID: "a", Source: SourceSpec{Kind: "nonsense"}}}},
		"file without path": {Seed: 1, Cameras: []CameraSpec{
			{ID: "a", Source: SourceSpec{Kind: SourceFile}},
		}},
		"advertised geometry differs from source": {Seed: 1, Cameras: []CameraSpec{
			{ID: "a", Video: VideoSpec{Width: 1920, Height: 1080}},
		}},
	}
	for name, s := range cases {
		if _, err := Start(context.Background(), s); err == nil {
			t.Errorf("%s: Start succeeded, want an error", name)
		}
	}
}

// Spec section 8.2: a failing test must be able to print one line that
// reproduces the run.
func TestReplayLineIsStableAndSpecific(t *testing.T) {
	f1, err := Start(context.Background(), minimalSpec())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = f1.Close() })

	line := f1.ReplayLine()
	if !strings.Contains(line, "seed=") || !strings.Contains(line, "spec=sha256:") {
		t.Fatalf("ReplayLine = %q", line)
	}

	// The same spec must hash the same, and a changed spec must not.
	h1 := minimalSpec().Hash()
	if h1 != minimalSpec().Hash() {
		t.Fatal("Hash is not stable for an identical spec")
	}
	changed := minimalSpec()
	changed.Cameras[0].ID = "side-door"
	if changed.Hash() == h1 {
		t.Fatal("Hash did not change when a camera ID did")
	}
	// A logger is not part of the fleet's identity.
	withLog := minimalSpec()
	withLog.Log = discardLoggerForTest()
	if withLog.Hash() != h1 {
		t.Fatal("Hash changed when only the logger was set")
	}
}

func TestStartTCleansUp(t *testing.T) {
	var cleanups []func()
	fake := &fakeTB{onCleanup: func(fn func()) { cleanups = append(cleanups, fn) }}

	f := StartT(fake, minimalSpec())
	if f.Addr().RTSP == nil {
		t.Fatal("StartT did not bind")
	}
	if len(cleanups) != 1 {
		t.Fatalf("registered %d cleanups, want 1", len(cleanups))
	}
	cleanups[0]()

	// After cleanup the fleet is closed; a second Close must still be safe.
	if err := f.Close(); err != nil {
		t.Errorf("Close after cleanup: %v", err)
	}
}

type fakeTB struct {
	onCleanup func(func())
	failed    string
}

func (f *fakeTB) Cleanup(fn func())              { f.onCleanup(fn) }
func (f *fakeTB) Helper()                        {}
func (f *fakeTB) Fatalf(format string, a ...any) { f.failed = format }

func discardLoggerForTest() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
