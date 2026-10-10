# Screenshot Capture Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a caller grab one decoded frame from a running camera, from Go (`Camera.Screenshot`) and over ONVIF HTTP (`GET /onvif/{id}/snapshot`, advertised by `GetSnapshotUri`).

**Architecture:** A leaf package `internal/screenshot` exposes `Capture(ctx, rtspURL) (image.Image, error)`. It dials the live RTSP wire like any client, waits for the next random-access access unit, decodes it with pure-Go decoders (`hi264`, `hi265`), and returns an `image.Image`. `Camera.Screenshot` feeds it the camera's own RTSP URL; the ONVIF server feeds it the camera's configured RTSP URL and JPEG-encodes the result behind the existing Digest auth.

**Tech Stack:** Go 1.26, `gortsplib/v5` client and `rtph264`/`rtph265` depacketisers (already required), `mediacommon/v2` `IsRandomAccess` (already required), new pure-Go MIT deps `github.com/Eyevinn/hi264 v0.10.0` and `github.com/Eyevinn/hi265 v0.5.0`.

**Spec:** `docs/superpowers/specs/2026-09-20-screenshot-capture-design.md`

## Global Constraints

- `CGO_ENABLED=0` must keep building: no cgo dependency of any kind. `hi264`, `hi265` and their `mp4ff` dependency are pure Go and MIT.
- Determinism is the product: nothing in this slice introduces randomness or wall-clock-dependent decisions. The only wait is "until the next random-access access unit", bounded by one loop of the fixture.
- Tests use real sockets and real decoding: a real `internal/rtsp.Server`, a real `gortsplib` dial, real `hi264`/`hi265` decode. No mocks of the RTSP wire.
- Both codecs (H.264 and H.265) are exercised end to end wherever the spec says "both codecs".
- Reuse the existing error taxonomy (`ErrNotReady`); context errors are returned unwrapped so `errors.Is(err, context.DeadlineExceeded)` / `context.Canceled` work. Do not add new exported sentinel errors to the root package.
- `go test -race ./...` must pass, including a race of `Camera.Screenshot` against `Fleet.RemoveCamera`.
- Prefer a small set of faithfully simulated behaviours: no keyframe priming or GOP cache, no resolution/format negotiation, no retry, no caching or rate limiting (all deferred by the spec).
- The README must keep its "not ONVIF conformant" language intact and must state the decoder limits honestly (see Task 5). No coverage number or badge is added by hand.
- No employer or client name anywhere in code, comments, commit messages or docs.
- Commits use the repository's configured identity (never an employer address).

## Rulings recorded against the spec

- **No Annex-B step.** Spec §2 step 5 says to assemble Annex-B before decoding. Both `hi264` (`DecodeNALUs(nalus [][]byte) (*frame.Frame, error)`) and `hi265` (`DecodeNALUs(nalus [][]byte) ([]*frame.Frame, error)`) accept NAL units without start codes, which is exactly what the RTP depacketisers return. So no start-code assembly is performed. The observable behaviour the spec asks for is unchanged.
- **Parameter sets come from the SDP.** The fixture's random-access units carry no in-band SPS/PPS/VPS, so the SDP's sets (`format.H264.SPS/PPS`, `format.H265.VPS/SPS/PPS`) are prepended to the access unit before decoding. This is why `Capture` needs no `internal/media` import.
- **`rewriteXAddr` signature is not changed.** A sibling `rewriteSnapshotURI` is added instead, so the three existing call sites (`server.go`, `dispatch_test.go` x2) are untouched.
- **Snapshot route lives inside the existing `/onvif/` handler.** `handle` gains a `snapshot` service branch before its POST-only check; no new mux pattern is registered.
- **"Not ready" detection:** `Camera.RTSPURL()` returns `""` when the RTSP server is not serving (before `Start`, after `Close`); `Screenshot` maps that to `ErrNotReady`. No new `Started()` method.

## File Structure

- Create `internal/screenshot/convert.go` — decoded-frame to `image.Image` adapters (one per codec).
- Create `internal/screenshot/screenshot.go` — `Capture`, `ErrNoSupportedVideo`, per-codec RTP-to-image grabbers.
- Create `internal/screenshot/screenshot_test.go` — real-wire tests for both codecs, cancellation, dead address.
- Modify `camfarm.go` — add `Camera.Screenshot`.
- Modify `camfarm_test.go` — Screenshot tests (both codecs, not ready, ctx, race with RemoveCamera).
- Modify `internal/onvif/server.go` — `rtspURL` on `camera`, `Snapshot.Enabled`, `snapshotURL`, `/snapshot` branch, `handleSnapshot`.
- Modify `internal/onvif/dispatch.go` — `rewriteSnapshotURI`.
- Modify `internal/onvif/dispatch_test.go` — `rewriteSnapshotURI` unit test.
- Create `internal/onvif/snapshot_test.go` — endpoint tests against a real RTSP server.
- Modify `onvif_test.go` — the `GetSnapshotUri` row now expects the advertised endpoint, not a fault.
- Modify `go.mod`, `go.sum` — new dependencies.
- Modify `README.md` — Works bullet and honest limits.

---

### Task 1: `internal/screenshot` with the H.264 path

**Files:**
- Create: `internal/screenshot/convert.go`
- Create: `internal/screenshot/screenshot.go`
- Create: `internal/screenshot/screenshot_test.go`
- Modify: `go.mod`, `go.sum`

**Interfaces:**
- Consumes: `internal/rtsp.New/Config/CameraConfig`, `rtsp.Server.Start/Close/URL`, `media.FixtureSource{}.Load()`, `obs.New(int)`, `seed.Seed` (all existing; used only by the test).
- Produces: `screenshot.Capture(ctx context.Context, rtspURL string) (image.Image, error)` and `screenshot.ErrNoSupportedVideo`. Task 2 extends `newGrabber`; Tasks 3 and 4 call `Capture`.

- [ ] **Step 1: Write the failing test**

Create `internal/screenshot/screenshot_test.go`:

```go
package screenshot

import (
	"context"
	"errors"
	"image"
	"testing"
	"time"

	"github.com/0x524a/camfarm/internal/media"
	"github.com/0x524a/camfarm/internal/obs"
	"github.com/0x524a/camfarm/internal/rtsp"
	"github.com/0x524a/camfarm/internal/seed"
)

// startRTSP serves src as camera "cam" over a real RTSP server and returns the
// camera's URL.
func startRTSP(t *testing.T, src media.Source) string {
	t.Helper()
	m, err := src.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	s, err := rtsp.New(rtsp.Config{
		Host: "127.0.0.1", Obs: obs.New(1000),
		Cameras: []rtsp.CameraConfig{{ID: "cam", Media: m, Seed: seed.Seed(7)}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(s.Close)
	return s.URL("cam")
}

func TestCaptureH264(t *testing.T) {
	url := startRTSP(t, media.FixtureSource{})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	img, err := Capture(ctx, url)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if b := img.Bounds(); b != image.Rect(0, 0, 320, 240) {
		t.Errorf("bounds = %v, want 320x240", b)
	}
}

func TestCaptureCancelled(t *testing.T) {
	url := startRTSP(t, media.FixtureSource{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Capture(ctx, url); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestCaptureDeadAddress(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := Capture(ctx, "rtsp://127.0.0.1:1/nope"); err == nil {
		t.Fatal("want an error dialing a closed port")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/screenshot/ -count=1`
Expected: FAIL to compile (`undefined: Capture`).

- [ ] **Step 3: Add the dependency and write the implementation**

Run: `go get github.com/Eyevinn/hi264@v0.10.0`

Create `internal/screenshot/convert.go`:

```go
package screenshot

import (
	"image"

	hi264frame "github.com/Eyevinn/hi264/pkg/frame"
	"github.com/Eyevinn/hi264/pkg/yuv"
)

// h264Image converts a decoded H.264 frame to RGB, honouring the colour
// metadata hi264 carries from the SPS.
func h264Image(f *hi264frame.Frame) image.Image {
	return yuv.FrameToImage(f)
}
```

Create `internal/screenshot/screenshot.go`:

```go
// Package screenshot grabs one decoded frame from a live RTSP stream.
//
// Capture dials the stream like any RTSP client, waits for the next random
// access point, decodes it, and returns it. It knows nothing about camfarm
// fleets: it takes a URL, so it works against any RTSP source and exercises
// the same wire path a real consumer would.
package screenshot

import (
	"context"
	"errors"
	"fmt"
	"image"
	"sync"

	"github.com/Eyevinn/hi264/pkg/decoder"
	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/pion/rtp"
)

// ErrNoSupportedVideo means the stream's SDP offers neither H.264 nor H.265.
var ErrNoSupportedVideo = errors.New("screenshot: stream has no H.264 or H.265 video")

// Capture returns the next decodable frame of the stream at rtspURL. It blocks
// until a random access point arrives, the stream ends, or ctx is done; ctx's
// own error is returned unwrapped so errors.Is(err, context.DeadlineExceeded)
// works for callers.
func Capture(ctx context.Context, rtspURL string) (image.Image, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	u, err := base.ParseURL(rtspURL)
	if err != nil {
		return nil, fmt.Errorf("screenshot: parsing %q: %w", rtspURL, err)
	}

	c := &gortsplib.Client{
		Scheme:            u.Scheme,
		Host:              u.Host,
		OnTransportSwitch: func(error) {},
	}
	if err := c.Start(); err != nil {
		return nil, fmt.Errorf("screenshot: connecting to %s: %w", u.Host, err)
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, c.Close)
	defer stop()

	desc, _, err := c.Describe(u)
	if err != nil {
		return nil, ctxOr(ctx, fmt.Errorf("screenshot: DESCRIBE: %w", err))
	}

	medi, forma, grab, err := newGrabber(desc)
	if err != nil {
		return nil, err
	}
	if err := c.SetupAll(desc.BaseURL, []*description.Media{medi}); err != nil {
		return nil, ctxOr(ctx, fmt.Errorf("screenshot: SETUP: %w", err))
	}

	type result struct {
		img image.Image
		err error
	}
	done := make(chan result, 1)
	var once sync.Once
	finish := func(img image.Image, err error) {
		once.Do(func() { done <- result{img, err} })
	}

	c.OnPacketRTP(medi, forma, func(pkt *rtp.Packet) {
		img, err := grab(pkt)
		if err != nil {
			finish(nil, err)
		} else if img != nil {
			finish(img, nil)
		}
	})
	if _, err := c.Play(nil); err != nil {
		return nil, ctxOr(ctx, fmt.Errorf("screenshot: PLAY: %w", err))
	}

	closed := make(chan error, 1)
	go func() { closed <- c.Wait() }()

	select {
	case r := <-done:
		return r.img, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case err := <-closed:
		return nil, ctxOr(ctx, fmt.Errorf("screenshot: stream ended before a frame arrived: %w", err))
	}
}

// ctxOr prefers the context's error when the context is what caused err.
func ctxOr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// grabber consumes RTP packets and returns a non-nil image once one decodes.
type grabber func(*rtp.Packet) (image.Image, error)

func newGrabber(desc *description.Session) (*description.Media, format.Format, grabber, error) {
	var f264 *format.H264
	if medi := desc.FindFormat(&f264); medi != nil {
		g, err := newH264Grabber(f264)
		return medi, f264, g, err
	}
	return nil, nil, nil, ErrNoSupportedVideo
}

// An RTP depacketising error is not fatal: mid-fragment joins and loss are
// normal on a live stream and simply mean "keep waiting". Only a failed decode
// of a random access point is reported, since that is the frame being asked for.
func newH264Grabber(f *format.H264) (grabber, error) {
	rd, err := f.CreateDecoder()
	if err != nil {
		return nil, fmt.Errorf("screenshot: H.264 RTP decoder: %w", err)
	}
	dec := decoder.New()
	return func(pkt *rtp.Packet) (image.Image, error) {
		nalus, err := rd.Decode(pkt)
		if err != nil {
			return nil, nil
		}
		if !h264.IsRandomAccess(nalus) {
			return nil, nil
		}
		au := withParamSets(nalus, f.SPS, f.PPS)
		fr, err := dec.DecodeNALUs(au)
		if err != nil {
			return nil, fmt.Errorf("screenshot: decoding H.264 random access point: %w", err)
		}
		return h264Image(fr), nil
	}, nil
}

// withParamSets prepends the SDP's parameter sets. The RTP stream need not
// repeat them in-band, and the decoders cannot start without them. Empty sets
// (H.264 has no VPS) are skipped.
func withParamSets(nalus [][]byte, sets ...[]byte) [][]byte {
	out := make([][]byte, 0, len(nalus)+len(sets))
	for _, s := range sets {
		if len(s) > 0 {
			out = append(out, s)
		}
	}
	return append(out, nalus...)
}
```

Run: `go mod tidy`

- [ ] **Step 4: Run tests to verify they pass**

Run: `go vet ./internal/screenshot/ && go test -race -count=1 ./internal/screenshot/ -v`
Expected: `TestCaptureH264`, `TestCaptureCancelled`, `TestCaptureDeadAddress` PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l . && git add go.mod go.sum internal/screenshot
git commit -m "Add internal/screenshot: capture one decoded H.264 frame from a live RTSP stream"
```

---

### Task 2: H.265 path

**Files:**
- Modify: `internal/screenshot/convert.go`
- Modify: `internal/screenshot/screenshot.go`
- Modify: `internal/screenshot/screenshot_test.go`
- Modify: `go.mod`, `go.sum`

**Interfaces:**
- Consumes: Task 1's `grabber`, `newGrabber`, `withParamSets`, `h264Image`; `media.FixtureH265Source{}`.
- Produces: `Capture` now also decodes H.265 streams. `h265Image(*hi265frame.Frame) image.Image`.

- [ ] **Step 1: Write the failing test**

Append to `internal/screenshot/screenshot_test.go`:

```go
func TestCaptureH265(t *testing.T) {
	url := startRTSP(t, media.FixtureH265Source{})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	img, err := Capture(ctx, url)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if img.Bounds().Empty() {
		t.Errorf("empty image: %v", img.Bounds())
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/screenshot/ -run TestCaptureH265 -count=1`
Expected: FAIL with `screenshot: stream has no H.264 or H.265 video`.

- [ ] **Step 3: Implement**

Run: `go get github.com/Eyevinn/hi265@v0.5.0`

In `internal/screenshot/convert.go`, replace the import block and append `h265Image`:

```go
import (
	"image"

	hi264frame "github.com/Eyevinn/hi264/pkg/frame"
	"github.com/Eyevinn/hi264/pkg/yuv"
	hi265frame "github.com/Eyevinn/hi265/pkg/frame"
)
```

```go
// h265Image wraps a decoded H.265 frame's planes as an image.YCbCr. hi265
// ships no colour conversion of its own and its frame carries no colour
// metadata, so the standard library's full-range conversion is used as is.
func h265Image(f *hi265frame.Frame) image.Image {
	return &image.YCbCr{
		Y:              f.Y,
		Cb:             f.Cb,
		Cr:             f.Cr,
		YStride:        f.StrideY,
		CStride:        f.StrideC,
		SubsampleRatio: image.YCbCrSubsampleRatio420,
		Rect:           image.Rect(0, 0, f.Width, f.Height),
	}
}
```

In `internal/screenshot/screenshot.go`: add imports `hi265decoder "github.com/Eyevinn/hi265/pkg/decoder"` and `"github.com/bluenviron/mediacommon/v2/pkg/codecs/h265"`; in `newGrabber`, insert before the final `return nil, nil, nil, ErrNoSupportedVideo`:

```go
	var f265 *format.H265
	if medi := desc.FindFormat(&f265); medi != nil {
		g, err := newH265Grabber(f265)
		return medi, f265, g, err
	}
```

and add after `newH264Grabber`:

```go
func newH265Grabber(f *format.H265) (grabber, error) {
	rd, err := f.CreateDecoder()
	if err != nil {
		return nil, fmt.Errorf("screenshot: H.265 RTP decoder: %w", err)
	}
	dec := hi265decoder.New()
	return func(pkt *rtp.Packet) (image.Image, error) {
		nalus, err := rd.Decode(pkt)
		if err != nil {
			return nil, nil
		}
		if !h265.IsRandomAccess(nalus) {
			return nil, nil
		}
		au := withParamSets(nalus, f.VPS, f.SPS, f.PPS)
		frames, err := dec.DecodeNALUs(au)
		if err != nil {
			return nil, fmt.Errorf("screenshot: decoding H.265 random access point: %w", err)
		}
		if len(frames) == 0 {
			return nil, nil
		}
		return h265Image(frames[0]), nil
	}, nil
}
```

Run: `go mod tidy` (this resolves `mp4ff` to the higher of hi264's and hi265's requirements; expected).

- [ ] **Step 4: Run tests to verify they pass**

Run: `go vet ./... && go test -race -count=1 ./internal/screenshot/ -v && CGO_ENABLED=0 go build ./...`
Expected: all four tests PASS; build succeeds.

- [ ] **Step 5: Commit**

```bash
gofmt -l . && git add go.mod go.sum internal/screenshot
git commit -m "Add H.265 support to screenshot capture"
```

---

### Task 3: `Camera.Screenshot`

**Files:**
- Modify: `camfarm.go` (imports; method added after `Inject`, before `Clear`)
- Modify: `camfarm_test.go` (add `"time"` to imports; append tests)

**Interfaces:**
- Consumes: `screenshot.Capture` (Task 1/2); existing `Camera.RTSPURL()`, `ErrNotReady`, `StartT`, `h265FixtureBytes`, `minimalSpec`.
- Produces: `func (c *Camera) Screenshot(ctx context.Context) (image.Image, error)`.

- [ ] **Step 1: Write the failing tests**

In `camfarm_test.go` add `"time"` to the import block (after `"testing"`), then append:

```go
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

func TestScreenshotH265(t *testing.T) {
	path := filepath.Join(t.TempDir(), "h265.ts")
	if err := os.WriteFile(path, h265FixtureBytes(t), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	f := StartT(t, Spec{Seed: 9, Cameras: []CameraSpec{{ID: "hevc", Source: SourceSpec{Kind: SourceFile, Path: path}}}})
	cam, err := f.Camera("hevc")
	if err != nil {
		t.Fatalf("Camera: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	img, err := cam.Screenshot(ctx)
	if err != nil {
		t.Fatalf("Screenshot: %v", err)
	}
	if img.Bounds().Empty() {
		t.Errorf("empty image: %v", img.Bounds())
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test . -run Screenshot -count=1`
Expected: FAIL to compile (`cam.Screenshot undefined`).

- [ ] **Step 3: Implement**

In `camfarm.go` add `"image"` to the standard imports (after `"fmt"`... keep gofmt order: `context`, `errors`, `fmt`, `image`, `io`, ...) and `"github.com/0x524a/camfarm/internal/screenshot"` between the `rtsp` and `seed` imports. Add after `Inject` and before `Clear`'s doc comment:

```go
// Screenshot returns one decoded frame from this camera's live RTSP stream.
//
// It dials the camera's own RTSP URL, so it exercises the real wire path, and
// blocks until the stream's next random access point (bounded by one loop of
// the source). It returns ErrNotReady if the fleet's RTSP server is not
// serving, and ctx's own error if ctx ends first.
func (c *Camera) Screenshot(ctx context.Context) (image.Image, error) {
	url := c.RTSPURL()
	if url == "" {
		return nil, ErrNotReady
	}
	return screenshot.Capture(ctx, url)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `gofmt -l . ; go vet ./... && go test -race -count=1 . -run Screenshot -v`
Expected: all five PASS.

- [ ] **Step 5: Commit**

```bash
git add camfarm.go camfarm_test.go
git commit -m "Add Camera.Screenshot"
```

---

### Task 4: ONVIF snapshot endpoint

**Files:**
- Modify: `internal/onvif/server.go`
- Modify: `internal/onvif/dispatch.go`
- Modify: `internal/onvif/dispatch_test.go`
- Create: `internal/onvif/snapshot_test.go`
- Modify: `onvif_test.go` (the `GetSnapshotUri` case, ~lines 95-99)

**Interfaces:**
- Consumes: `screenshot.Capture`; existing `camera`, `digestAuth.verify/challenge`, `Server.Endpoint`, `Server.log`, `parsePath`, `handle`, `testMedia`, `startTestServer`, `soapContentType`.
- Produces: `GET {endpoint}/snapshot` → `image/jpeg`; 404 unknown camera, 401 when credentialed and unauthenticated, 405 non-GET, 500 on capture failure. `rewriteSnapshotURI(resp any, snapshotURL string)`. `GetSnapshotUri` now answers with `{Endpoint(id)}/snapshot` instead of a fault.

- [ ] **Step 1: Write the failing tests**

Create `internal/onvif/snapshot_test.go`:

```go
package onvif

import (
	"bytes"
	"image/jpeg"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/0x524a/camfarm/internal/media"
	"github.com/0x524a/camfarm/internal/obs"
	"github.com/0x524a/camfarm/internal/rtsp"
	"github.com/0x524a/camfarm/internal/seed"
)

// startRealRTSP serves the bundled fixture over a real RTSP server and returns
// its media and the camera's URL, so snapshot tests exercise the real wire.
func startRealRTSP(t *testing.T) (*media.Media, string) {
	t.Helper()
	m, err := (media.FixtureSource{}).Load()
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	rs, err := rtsp.New(rtsp.Config{
		Host: "127.0.0.1", Obs: obs.New(1000),
		Cameras: []rtsp.CameraConfig{{ID: "cam", Media: m, Seed: seed.Seed(3)}},
	})
	if err != nil {
		t.Fatalf("rtsp.New: %v", err)
	}
	if err := rs.Start(); err != nil {
		t.Fatalf("rtsp.Start: %v", err)
	}
	t.Cleanup(rs.Close)
	return m, rs.URL("cam")
}

func TestSnapshotEndpointServesJPEG(t *testing.T) {
	m, url := startRealRTSP(t)
	srv := startTestServer(t, CameraConfig{ID: "cam", Media: m, RTSPURL: url, Seed: seed.Seed(1)})

	resp, err := http.Get(srv.Endpoint("cam") + "/snapshot")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("Content-Type = %q, want image/jpeg", ct)
	}
	img, err := jpeg.Decode(resp.Body)
	if err != nil {
		t.Fatalf("body is not a JPEG: %v", err)
	}
	if b := img.Bounds(); b.Dx() != 320 || b.Dy() != 240 {
		t.Errorf("bounds = %v, want 320x240", b)
	}
}

func TestSnapshotUnknownCameraIs404(t *testing.T) {
	srv := startTestServer(t, CameraConfig{ID: "cam", Media: testMedia(), RTSPURL: "rtsp://127.0.0.1:9/cam", Seed: seed.Seed(1)})
	resp, err := http.Get("http://" + srv.Addr().String() + "/onvif/nope/snapshot")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestSnapshotCaptureFailureIs500(t *testing.T) {
	// Port 9 (discard) has no RTSP server: the dial fails for real.
	srv := startTestServer(t, CameraConfig{ID: "cam", Media: testMedia(), RTSPURL: "rtsp://127.0.0.1:9/cam", Seed: seed.Seed(1)})
	resp, err := http.Get(srv.Endpoint("cam") + "/snapshot")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 500 {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
}

func TestSnapshotRequiresDigestWhenCredentialed(t *testing.T) {
	srv := startTestServer(t, CameraConfig{
		ID: "cam", Media: testMedia(), RTSPURL: "rtsp://127.0.0.1:9/cam",
		Username: "admin", Password: "secret", Seed: seed.Seed(1),
	})
	resp, err := http.Get(srv.Endpoint("cam") + "/snapshot")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
	if resp.Header.Get("WWW-Authenticate") == "" {
		t.Error("missing WWW-Authenticate header")
	}
}

func TestSnapshotRejectsNonGET(t *testing.T) {
	srv := startTestServer(t, CameraConfig{ID: "cam", Media: testMedia(), RTSPURL: "rtsp://127.0.0.1:9/cam", Seed: seed.Seed(1)})
	resp, err := http.Post(srv.Endpoint("cam")+"/snapshot", "text/plain", strings.NewReader("x"))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 405 {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
}

func TestGetSnapshotUriAdvertisesTheRealEndpoint(t *testing.T) {
	srv := startTestServer(t, CameraConfig{ID: "cam", Media: testMedia(), RTSPURL: "rtsp://127.0.0.1:9/cam", Seed: seed.Seed(1)})

	body := `<?xml version="1.0"?><soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/"><soap:Body>` +
		`<GetSnapshotUri xmlns="http://www.onvif.org/ver10/media/wsdl"><ProfileToken>profile1</ProfileToken></GetSnapshotUri>` +
		`</soap:Body></soap:Envelope>`
	resp, err := http.Post(srv.Endpoint("cam")+"/media", soapContentType, strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	want := srv.Endpoint("cam") + "/snapshot"
	if !bytes.Contains(out, []byte(want)) {
		t.Errorf("response = %s, want it to carry %q", out, want)
	}
}
```

Append to `internal/onvif/dispatch_test.go`:

```go
func TestRewriteSnapshotURI(t *testing.T) {
	resp := &onvifserver.GetSnapshotURIResponse{MediaURI: onvifserver.MediaURI{URI: "http://wrong/snapshot?profile=profile1"}}
	rewriteSnapshotURI(resp, "http://real/onvif/cam/snapshot")
	if resp.MediaURI.URI != "http://real/onvif/cam/snapshot" {
		t.Errorf("URI = %q", resp.MediaURI.URI)
	}
	// Other response types pass through untouched.
	rewriteSnapshotURI(&onvifserver.GetStreamURIResponse{}, "http://real/onvif/cam/snapshot")
}
```

In `onvif_test.go`, replace the comment block and the `GetSnapshotUri` case (the three-line `// Snapshot is disabled ...` comment plus the case line) with:

```go
		{"GetSnapshotUri", "/media", mediaNS, "GetSnapshotUri", "<ProfileToken>profile1</ProfileToken>", "/onvif/cam/snapshot"},
```

(The test's camera ID is `cam`, so the advertised URI ends in `/onvif/cam/snapshot`.)

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/onvif/ . -count=1`
Expected: FAIL (compile error `undefined: rewriteSnapshotURI`).

- [ ] **Step 3: Implement**

Append to `internal/onvif/dispatch.go`:

```go
// rewriteSnapshotURI rewrites onvif-go's GetSnapshotUri answer, built from the
// same unbound Config.Host/Port as the XAddrs, to the camera's real snapshot
// endpoint on this fleet's ONVIF listener.
func rewriteSnapshotURI(resp any, snapshotURL string) {
	if r, ok := resp.(*onvifserver.GetSnapshotURIResponse); ok {
		r.MediaURI.URI = snapshotURL
	}
}
```

In `internal/onvif/server.go`:

1. Imports: add `"bytes"` (before `"fmt"`), `"image/jpeg"` (before `"net/http"`), and `"github.com/0x524a/camfarm/internal/screenshot"` (after the `media` import).
2. `camera` struct becomes:

```go
type camera struct {
	id      string
	srv     *onvifserver.Server
	rtspURL string
	digest  *digestAuth // nil means open
}
```

3. In `buildCamera`, replace `Snapshot: onvifserver.SnapshotConfig{Enabled: false},` with:

```go
				Snapshot: onvifserver.SnapshotConfig{
					Enabled:    true,
					Resolution: onvifserver.Resolution{Width: cc.Media.Width, Height: cc.Media.Height},
				},
```

and `cam := &camera{id: cc.ID, srv: srv}` with `cam := &camera{id: cc.ID, srv: srv, rtspURL: cc.RTSPURL}`.

4. After `mediaURL`, add:

```go
func (s *Server) snapshotURL(id string) string { return s.Endpoint(id) + "/snapshot" }
```

5. In `handle`, change the service check to `service != "device" && service != "media" && service != "snapshot"`; immediately before the `if r.Method != http.MethodPost {` block insert:

```go
	if service == "snapshot" {
		s.handleSnapshot(w, r, cam)
		return
	}
```

and directly after the `rewriteXAddr(...)` call add `rewriteSnapshotURI(resp, s.snapshotURL(id))`.

6. Append:

```go
// handleSnapshot serves one JPEG frame of the camera's live RTSP stream. It is
// the HTTP endpoint GetSnapshotUri advertises, gated behind the same Digest
// auth as the SOAP services.
func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request, cam *camera) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if cam.digest != nil && !cam.digest.verify(r) {
		cam.digest.challenge(w)
		return
	}

	img, err := screenshot.Capture(r.Context(), cam.rtspURL)
	if err != nil {
		s.log.Warn("snapshot capture failed", "camera", cam.id, "err", err)
		http.Error(w, "snapshot capture failed", http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		s.log.Warn("snapshot encode failed", "camera", cam.id, "err", err)
		http.Error(w, "snapshot encode failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	_, _ = w.Write(buf.Bytes())
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `gofmt -w internal/onvif && go vet ./... && go test -race -count=1 ./internal/onvif/ . -v 2>&1 | tail -40`
Expected: all PASS, including the updated `TestEveryDeviceAndMediaOperation/GetSnapshotUri`.

- [ ] **Step 5: Commit**

```bash
gofmt -l . ; git add internal/onvif onvif_test.go
git commit -m "Serve ONVIF snapshots: enable GetSnapshotUri and add /onvif/{id}/snapshot"
```

---

### Task 5: README, full verification

**Files:**
- Modify: `README.md` (Status section)

**Interfaces:**
- Consumes: everything above.
- Produces: user-facing documentation; a green full suite.

- [ ] **Step 1: Update the README**

In `README.md`'s Status list, insert after the `Fleet.AddCamera` bullet (before **Not built yet**):

```markdown
- **Works:** screenshots — `Camera.Screenshot(ctx)` returns one decoded frame
  from a camera's live RTSP stream, and each camera serves it as a JPEG at
  `/onvif/{id}/snapshot` (behind Digest auth when credentialed), which
  `GetSnapshotUri` advertises. It dials the real RTSP wire, so it waits for the
  stream's next keyframe (at most one loop of the source). The pure-Go decoders
  cover intra pictures (H.264 IDR, H.265 IDR/CRA) only; the bundled fixtures
  and similarly encoded files decode, and a source using features outside that
  subset fails with an error rather than a wrong picture.
```

Also confirm the "Scope / what this does not do" section still states the not-conformant disclaimer unchanged; do not edit it.

- [ ] **Step 2: Run the full gates CI runs**

Run:
```bash
gofmt -l . ; go vet ./... && CGO_ENABLED=0 go build ./... && go test -count=1 ./... && go test -race -count=1 ./...
```
Expected: no gofmt output; every package `ok`.

- [ ] **Step 3: Repeat the new tests for flakiness**

Run: `go test -race -count=5 -run 'Screenshot|Capture|Snapshot' ./...`
Expected: PASS every iteration.

- [ ] **Step 4: Commit**

```bash
git add README.md
git commit -m "Document screenshot capture and its decoder limits"
```

---

## Self-review

- **Spec coverage:** `Capture` leaf with live wire and SDP-driven codec (Tasks 1-2); `Camera.Screenshot`, `ErrNotReady`, ctx errors unwrapped, removal propagates as-is, `-race` against `RemoveCamera` (Task 3); ONVIF `Snapshot.Enabled`, Resolution, `rtspURL`, `/snapshot` route, Digest gating, JPEG, 404/500, `GetSnapshotUri` URI rewrite (Task 4); docs and honest limits (Task 5). Deferred items (priming, GOP cache, negotiation, retry, caching, dashboard) have no tasks, as the spec directs.
- **Placeholder scan:** none; every code step is complete. The only prose-described edits are mechanical import and one-line replacements with exact text given.
- **Type consistency:** `Capture(ctx, rtspURL) (image.Image, error)` is used identically in Tasks 3 and 4; `rewriteSnapshotURI(resp any, snapshotURL string)` matches its test and call site; `camera.rtspURL` set in `buildCamera` and read in `handleSnapshot`; `withParamSets(nalus, sets...)` signature is identical in Tasks 1 and 2.
- **Verified:** all code in this plan was run end to end in a scratch copy of the repository: the full suite passes under `-race`, `CGO_ENABLED=0 go build ./...` succeeds, and the existing `onvif_test.go` snapshot row was the only existing test that needed changing.
