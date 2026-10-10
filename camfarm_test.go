package camfarm

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0x524a/camfarm/internal/media"
	"github.com/0x524a/camfarm/internal/media/fixture"
	"github.com/0x524a/camfarm/internal/onvif"
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
	s.Cameras[0].Faults = []FaultSpec{{Kind: "accept_then_silence", Rate: 0.5}}

	_, err := Start(context.Background(), s)
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
	// The message must name the kind, so a caller knows what was refused.
	if !strings.Contains(err.Error(), "accept_then_silence") {
		t.Errorf("err = %v, want it to name the fault kind", err)
	}

	// An unknown kind is a different failure from an unimplemented one.
	s.Cameras[0].Faults = []FaultSpec{{Kind: "not-a-fault", Rate: 0.5}}
	if _, err := Start(context.Background(), s); err == nil {
		t.Fatal("unknown fault kind was accepted")
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

// h265FixtureBytes writes the bundled H.265 fixture out through the media
// package's own source, so this test does not duplicate the embed.
func h265FixtureBytes(t *testing.T) []byte {
	t.Helper()
	b := fixture.BytesH265()
	if len(b) == 0 {
		t.Fatal("embedded H265 fixture is empty")
	}
	return b
}

// TestSpecAcceptsH265Source proves an H.265 camera reaches a running fleet and
// reports its codec, which is the whole feature from a caller's point of view.
func TestSpecAcceptsH265Camera(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "h265.ts")
	if err := os.WriteFile(path, h265FixtureBytes(t), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	f := StartT(t, Spec{
		Seed: 0x1234,
		Cameras: []CameraSpec{{
			ID:     "hevc",
			Source: SourceSpec{Kind: SourceFile, Path: path},
		}},
	})

	list := f.List()
	if len(list) != 1 {
		t.Fatalf("cameras = %d, want 1", len(list))
	}
	if list[0].Codec != "H265" {
		t.Errorf("codec = %q, want H265", list[0].Codec)
	}
}

// TestVideoSpecMatchingSourcePasses proves the redefined semantics: a value equal
// to the source's own is an honoured request for passthrough, not a rejection.
func TestVideoSpecMatchingSourcePasses(t *testing.T) {
	f := StartT(t, Spec{
		Seed: 1,
		Cameras: []CameraSpec{{
			ID:    "cam",
			Video: VideoSpec{Codec: "H264", Width: 320, Height: 240, FPS: 15},
		}},
	})
	if got := f.List()[0].Width; got != 320 {
		t.Errorf("width = %d, want 320", got)
	}
}

// TestVideoSpecRescaleRefusedNamingFutureBehaviour pins the mitigation for the
// accepted risk in the design: because a spec refused today will silently begin
// rescaling on a later version, the refusal must name that future behaviour so
// the upgrade is at least legible from the error a developer sees now.
func TestVideoSpecRescaleRefusedNamingFutureBehaviour(t *testing.T) {
	_, err := Start(context.Background(), Spec{
		Seed:    1,
		Cameras: []CameraSpec{{ID: "cam", Video: VideoSpec{Width: 1920, Height: 1080}}},
	})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
	if !strings.Contains(err.Error(), "rescaling is not implemented") {
		t.Errorf("err = %q, want it to name rescaling as the unimplemented behaviour", err.Error())
	}
}

func TestVideoSpecTranscodeRefusedNamingFutureBehaviour(t *testing.T) {
	_, err := Start(context.Background(), Spec{
		Seed:    1,
		Cameras: []CameraSpec{{ID: "cam", Video: VideoSpec{Codec: "H265"}}},
	})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
	if !strings.Contains(err.Error(), "transcoding") {
		t.Errorf("err = %q, want it to name transcoding as the unimplemented behaviour", err.Error())
	}
}

func TestSpecRejectsUnknownCodec(t *testing.T) {
	_, err := Start(context.Background(), Spec{
		Seed:    1,
		Cameras: []CameraSpec{{ID: "cam", Video: VideoSpec{Codec: "VP9"}}},
	})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
}

// TestMixedCodecFleet proves per-camera codec dispatch is genuinely per camera
// rather than per fleet.
func TestMixedCodecFleet(t *testing.T) {
	dir := t.TempDir()
	h265Path := filepath.Join(dir, "h265.ts")
	if err := os.WriteFile(h265Path, h265FixtureBytes(t), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	f := StartT(t, Spec{
		Seed: 0xABCD,
		Cameras: []CameraSpec{
			{ID: "avc"},
			{ID: "hevc", Source: SourceSpec{Kind: SourceFile, Path: h265Path}},
		},
	})

	byID := map[string]string{}
	for _, s := range f.List() {
		byID[s.ID] = s.Codec
	}
	if byID["avc"] != "H264" {
		t.Errorf("avc codec = %q, want H264", byID["avc"])
	}
	if byID["hevc"] != "H265" {
		t.Errorf("hevc codec = %q, want H265", byID["hevc"])
	}
}

func TestAddCameraAppearsInList(t *testing.T) {
	f := StartT(t, minimalSpec())

	cam, err := f.AddCamera(CameraSpec{ID: "new-cam"})
	if err != nil {
		t.Fatalf("AddCamera: %v", err)
	}
	if cam.ID() != "new-cam" {
		t.Errorf("ID = %q", cam.ID())
	}

	list := f.List()
	if len(list) != 3 {
		t.Fatalf("List = %d cameras, want 3", len(list))
	}
	found := false
	for _, st := range list {
		if st.ID == "new-cam" {
			found = true
			if !strings.HasSuffix(st.RTSPURL, "/new-cam") {
				t.Errorf("RTSPURL = %q", st.RTSPURL)
			}
		}
	}
	if !found {
		t.Fatal("new-cam not present in List()")
	}

	got, err := f.Camera("new-cam")
	if err != nil {
		t.Fatalf("Camera: %v", err)
	}
	if got.Stats().Seed == 0 || got.Stats().Seed == f.Seed() {
		t.Error("added camera's seed is zero or equals the root seed; it must be derived")
	}
}

func TestAddCameraDuplicateIDRefused(t *testing.T) {
	f := StartT(t, minimalSpec())
	if _, err := f.AddCamera(CameraSpec{ID: "front-door"}); err == nil {
		t.Fatal("AddCamera with a duplicate ID succeeded, want an error")
	}
}

// Spec section 7.7's fault refusal must hold through AddCamera too, not only
// through Start -- otherwise a dashboard could ask for a fault and silently
// get a camera that never misbehaves.
func TestAddCameraFaultRefused(t *testing.T) {
	f := StartT(t, minimalSpec())
	_, err := f.AddCamera(CameraSpec{ID: "new-cam", Faults: []FaultSpec{{Kind: "accept_then_silence", Rate: 0.5}}})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
}

// TestAddCameraH265Source proves AddCamera can add a camera whose source
// kind differs from every camera present since Start, reusing the fleet's
// media cache the same way Start shares a source across cameras.
func TestAddCameraH265Source(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "h265.ts")
	if err := os.WriteFile(path, h265FixtureBytes(t), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	f := StartT(t, minimalSpec())
	cam, err := f.AddCamera(CameraSpec{ID: "hevc", Source: SourceSpec{Kind: SourceFile, Path: path}})
	if err != nil {
		t.Fatalf("AddCamera: %v", err)
	}
	_ = cam

	for _, st := range f.List() {
		if st.ID == "hevc" && st.Codec != "H265" {
			t.Errorf("codec = %q, want H265", st.Codec)
		}
	}
}

// TestAddCameraRollsBackRTSPOnONVIFFailure exercises AddCamera's rollback path
// for real, with no mocks: it desyncs the RTSP and ONVIF subsystems so that
// f.srv.AddCamera succeeds but f.onvifSrv.AddCamera then fails, which is
// exactly the branch the rollback exists to handle.
func TestAddCameraRollsBackRTSPOnONVIFFailure(t *testing.T) {
	f := StartT(t, minimalSpec())

	// Register "collision" on the ONVIF side only, bypassing Fleet.AddCamera
	// entirely. The RTSP side knows nothing about it, so f.srv.AddCamera below
	// will succeed; the ONVIF side already has it, so f.onvifSrv.AddCamera will
	// refuse it as a duplicate.
	m := &media.Media{Codec: media.CodecH264, Width: 320, Height: 240, FPS: 30}
	if err := f.onvifSrv.AddCamera(onvif.CameraConfig{ID: "collision", Media: m, RTSPURL: "http://127.0.0.1:9/collision"}); err != nil {
		t.Fatalf("pre-registering collision on onvif: %v", err)
	}

	if _, err := f.AddCamera(CameraSpec{ID: "collision"}); err == nil {
		t.Fatal("AddCamera succeeded despite the onvif-side ID collision, want an error")
	}

	for _, st := range f.List() {
		if st.ID == "collision" {
			t.Fatal("collision present in List() after a failed AddCamera; fleet bookkeeping corrupted")
		}
	}

	// Clear the manually-registered onvif entry and retry for real: if the
	// rollback had left a leftover entry in the RTSP dispatch table, this
	// second, otherwise-clean attempt would spuriously fail as a duplicate.
	if err := f.onvifSrv.RemoveCamera("collision"); err != nil {
		t.Fatalf("removing manually-registered onvif entry: %v", err)
	}
	if _, err := f.AddCamera(CameraSpec{ID: "collision"}); err != nil {
		t.Fatalf("AddCamera after rollback: %v", err)
	}
}

func TestRemoveCameraRemovesFromList(t *testing.T) {
	f := StartT(t, minimalSpec())

	if err := f.RemoveCamera("front-door"); err != nil {
		t.Fatalf("RemoveCamera: %v", err)
	}

	list := f.List()
	if len(list) != 1 {
		t.Fatalf("List = %d cameras, want 1", len(list))
	}
	if list[0].ID != "lobby" {
		t.Errorf("surviving camera = %q, want lobby", list[0].ID)
	}
	if _, err := f.Camera("front-door"); !errors.Is(err, ErrUnknownCamera) {
		t.Errorf("Camera(removed): err = %v, want ErrUnknownCamera", err)
	}
}

func TestRemoveUnknownCameraErrorsFleet(t *testing.T) {
	f := StartT(t, minimalSpec())
	if err := f.RemoveCamera("nope"); !errors.Is(err, ErrUnknownCamera) {
		t.Errorf("err = %v, want ErrUnknownCamera", err)
	}
}

// TestAddThenRemoveCamera proves the full lifecycle: a camera added at
// runtime can also be removed at runtime.
func TestAddThenRemoveCamera(t *testing.T) {
	f := StartT(t, minimalSpec())

	if _, err := f.AddCamera(CameraSpec{ID: "temp"}); err != nil {
		t.Fatalf("AddCamera: %v", err)
	}
	if len(f.List()) != 3 {
		t.Fatalf("List = %d cameras after add, want 3", len(f.List()))
	}
	if err := f.RemoveCamera("temp"); err != nil {
		t.Fatalf("RemoveCamera: %v", err)
	}
	if len(f.List()) != 2 {
		t.Fatalf("List = %d cameras after remove, want 2", len(f.List()))
	}
}

func TestScreenshotH264(t *testing.T) {
	f := StartT(t, minimalSpec())
	cam, err := f.Camera("front-door")
	if err != nil {
		t.Fatalf("Camera: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	img, err := cam.Screenshot(ctx)
	if err != nil {
		t.Fatalf("Screenshot: %v", err)
	}
	if b := img.Bounds(); b.Dx() != 320 || b.Dy() != 240 {
		t.Errorf("bounds = %v, want 320x240", b)
	}
}

func TestScreenshotAfterCloseIsNotReady(t *testing.T) {
	f, err := Start(context.Background(), minimalSpec())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	cam, err := f.Camera("front-door")
	if err != nil {
		t.Fatalf("Camera: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := cam.Screenshot(context.Background()); !errors.Is(err, ErrNotReady) {
		t.Errorf("err = %v, want ErrNotReady", err)
	}
}

func TestScreenshotHonoursContext(t *testing.T) {
	f := StartT(t, minimalSpec())
	cam, err := f.Camera("front-door")
	if err != nil {
		t.Fatalf("Camera: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := cam.Screenshot(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

// TestScreenshotRacesRemoveCamera runs under -race: capturing while the camera
// is being removed must end in a frame or an error, never a panic or a hang.
func TestScreenshotRacesRemoveCamera(t *testing.T) {
	f := StartT(t, minimalSpec())
	cam, err := f.Camera("front-door")
	if err != nil {
		t.Fatalf("Camera: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = cam.Screenshot(ctx)
	}()
	if err := f.RemoveCamera("front-door"); err != nil {
		t.Fatalf("RemoveCamera: %v", err)
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("Screenshot did not return after RemoveCamera")
	}
}

func TestInjectUnimplementedKindIsUnsupported(t *testing.T) {
	f := StartT(t, minimalSpec())
	cam, _ := f.Camera("front-door")
	err := cam.Inject(FaultSpec{Kind: "accept_then_silence", Rate: 1})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
	if err := cam.Inject(FaultSpec{Kind: "not-a-fault", Rate: 1}); !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("unknown kind: err = %v, want ErrInvalidSpec", err)
	}
	if err := cam.Inject(FaultSpec{Kind: "frame_drop", Rate: 2}); !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("bad rate: err = %v, want ErrInvalidSpec", err)
	}
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSpecTimeFrameDropTakesEffect(t *testing.T) {
	s := minimalSpec()
	s.Cameras[0].Faults = []FaultSpec{{Kind: "frame_drop", Rate: 1}}
	f := StartT(t, s)
	cam, _ := f.Camera("front-door")
	other, _ := f.Camera("lobby")

	waitUntil(t, "drops to be recorded", func() bool { return cam.Stats().FaultsFired >= 3 })
	if got := cam.Stats().FramesServed; got != 0 {
		t.Errorf("FramesServed = %d, want 0 under frame_drop rate 1", got)
	}
	waitUntil(t, "the other camera to serve frames", func() bool { return other.Stats().FramesServed > 0 })
	if other.Stats().FaultsFired != 0 {
		t.Error("a fault on one camera fired on another")
	}
	if got := cam.Faults(); len(got) != 1 || got[0].Kind != "frame_drop" {
		t.Errorf("Faults() = %+v", got)
	}
}

func TestInjectThenClearOnARunningCamera(t *testing.T) {
	f := StartT(t, minimalSpec())
	cam, _ := f.Camera("front-door")
	waitUntil(t, "frames to flow", func() bool { return cam.Stats().FramesServed > 0 })

	if err := cam.Inject(FaultSpec{Kind: "frame_drop", Rate: 1}); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	waitUntil(t, "drops to be recorded", func() bool { return cam.Stats().FaultsFired >= 3 })
	if got := cam.Faults(); len(got) != 1 {
		t.Fatalf("Faults() = %+v, want the injected fault", got)
	}

	if err := cam.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if len(cam.Faults()) != 0 {
		t.Fatal("Faults() not empty after Clear")
	}
	before := cam.Stats().FramesServed
	waitUntil(t, "frames to resume", func() bool { return cam.Stats().FramesServed > before })

	// Clearing with nothing active succeeds.
	if err := cam.Clear(); err != nil {
		t.Errorf("second Clear: %v", err)
	}
}

func TestInjectReplacesTheSameKind(t *testing.T) {
	f := StartT(t, minimalSpec())
	cam, _ := f.Camera("front-door")
	_ = cam.Inject(FaultSpec{Kind: "frame_drop", Rate: 0.2})
	_ = cam.Inject(FaultSpec{Kind: "frame_drop", Rate: 0.9, ProtectKeyframes: true})
	got := cam.Faults()
	if len(got) != 1 || got[0].Rate != 0.9 || !got[0].ProtectKeyframes {
		t.Fatalf("Faults() = %+v, want one frame_drop at 0.9 with protectKeyframes", got)
	}
}

func TestSpecAndAuthAccessors(t *testing.T) {
	s := Spec{Seed: 1, Cameras: []CameraSpec{{ID: "locked", Auth: AuthSpec{Username: "admin", Password: "pw"}}, {ID: "open"}}}
	f := StartT(t, s)
	locked, _ := f.Camera("locked")
	open, _ := f.Camera("open")

	if u, p, ok := locked.Auth(); !ok || u != "admin" || p != "pw" {
		t.Errorf("Auth() = %q %q %v", u, p, ok)
	}
	if _, _, ok := open.Auth(); ok {
		t.Error("open camera reports credentials")
	}
	if got := locked.Spec(); got.ID != "locked" || got.Source.Kind != SourceFixture {
		t.Errorf("Spec() = %+v", got)
	}
}

func TestSentinelErrors(t *testing.T) {
	f := StartT(t, minimalSpec())
	if _, err := f.AddCamera(CameraSpec{ID: "front-door"}); !errors.Is(err, ErrDuplicateCamera) {
		t.Errorf("duplicate: err = %v, want ErrDuplicateCamera", err)
	}
	for name, cs := range map[string]CameraSpec{
		"empty id":   {ID: ""},
		"slash":      {ID: "a/b"},
		"half auth":  {ID: "x", Auth: AuthSpec{Username: "u"}},
		"bad source": {ID: "y", Source: SourceSpec{Kind: "nonsense"}},
		"bad fault":  {ID: "z", Faults: []FaultSpec{{Kind: "nope", Rate: 1}}},
	} {
		if _, err := f.AddCamera(cs); !errors.Is(err, ErrInvalidSpec) {
			t.Errorf("%s: err = %v, want ErrInvalidSpec", name, err)
		}
	}
}

// A handle kept past RemoveCamera must not act on a camera later re-added under
// the same id: it would mutate the new camera's pump and journal the change.
func TestStaleCameraHandleCannotInjectIntoReaddedCamera(t *testing.T) {
	f := StartT(t, minimalSpec())
	stale, _ := f.Camera("front-door")
	if err := f.RemoveCamera("front-door"); err != nil {
		t.Fatalf("RemoveCamera: %v", err)
	}
	if _, err := f.AddCamera(CameraSpec{ID: "front-door"}); err != nil {
		t.Fatalf("AddCamera: %v", err)
	}
	fresh, _ := f.Camera("front-door")
	journalBefore := len(f.Journal())

	if err := stale.Inject(FaultSpec{Kind: "frame_drop", Rate: 1}); !errors.Is(err, ErrUnknownCamera) {
		t.Fatalf("stale Inject = %v, want ErrUnknownCamera", err)
	}
	if err := stale.Clear(); !errors.Is(err, ErrUnknownCamera) {
		t.Fatalf("stale Clear = %v, want ErrUnknownCamera", err)
	}
	if len(fresh.Faults()) != 0 || len(f.Journal()) != journalBefore {
		t.Fatalf("stale handle changed the re-added camera: faults=%v journal grew=%d", fresh.Faults(), len(f.Journal())-journalBefore)
	}
}

func TestInjectPacketLossAndKeyframeStarvationOnARunningCamera(t *testing.T) {
	f := StartT(t, minimalSpec())
	cam, _ := f.Camera("front-door")

	if err := cam.Inject(FaultSpec{Kind: "packet_loss", Rate: 0.3, MeanBurst: 3}); err != nil {
		t.Fatalf("Inject packet_loss: %v", err)
	}
	if err := cam.Inject(FaultSpec{Kind: "keyframe_starvation", KeepEvery: 2}); err != nil {
		t.Fatalf("Inject keyframe_starvation: %v", err)
	}
	if got := cam.Faults(); len(got) != 2 {
		t.Fatalf("Faults() = %+v, want both kinds active", got)
	}
}

func TestInjectRejectsBadParametersAsInvalidSpec(t *testing.T) {
	f := StartT(t, minimalSpec())
	cam, _ := f.Camera("front-door")
	for name, spec := range map[string]FaultSpec{
		"starvation without keepEvery":   {Kind: "keyframe_starvation"},
		"starvation with rate":           {Kind: "keyframe_starvation", KeepEvery: 3, Rate: 0.5},
		"frame_drop with keepEvery":      {Kind: "frame_drop", Rate: 0.1, KeepEvery: 3},
		"burst above the reachable rate": {Kind: "packet_loss", Rate: 0.9, MeanBurst: 4},
		"rate above one":                 {Kind: "packet_loss", Rate: 1.5},
	} {
		if err := cam.Inject(spec); !errors.Is(err, ErrInvalidSpec) {
			t.Errorf("%s: Inject = %v, want ErrInvalidSpec", name, err)
		}
	}
	if len(cam.Faults()) != 0 {
		t.Fatal("a refused injection changed the active faults")
	}
}

func TestSpecAcceptsPacketLossAndRefusesBadParameters(t *testing.T) {
	s := minimalSpec()
	s.Cameras[0].Faults = []FaultSpec{{Kind: "packet_loss", Rate: 0.1}}
	f, err := Start(context.Background(), s)
	if err != nil {
		t.Fatalf("Start with packet_loss: %v", err)
	}
	f.Close()

	s.Cameras[0].Faults = []FaultSpec{{Kind: "keyframe_starvation"}}
	if _, err := Start(context.Background(), s); !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("Start with starvation lacking keepEvery = %v, want ErrInvalidSpec", err)
	}
}

func TestAddCameraRefusesBadFaultParametersAsInvalidSpec(t *testing.T) {
	f := StartT(t, minimalSpec())
	_, err := f.AddCamera(CameraSpec{ID: "new-cam", Faults: []FaultSpec{{Kind: "keyframe_starvation"}}})
	if !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("AddCamera with starvation lacking keepEvery = %v, want ErrInvalidSpec", err)
	}
}

func TestStatsFaultCountsReportPerKindEffects(t *testing.T) {
	f := StartT(t, minimalSpec())
	cam, _ := f.Camera("front-door")
	if cam.Stats().FaultCounts == nil {
		t.Fatal("FaultCounts is nil on a camera with no faults")
	}
	if err := cam.Inject(FaultSpec{Kind: "frame_drop", Rate: 1}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "drops to be tallied", func() bool {
		return cam.Stats().FaultCounts["frame_drop"].Applied >= 3
	})
}
