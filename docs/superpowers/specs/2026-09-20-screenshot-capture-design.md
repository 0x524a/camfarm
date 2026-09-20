# Screenshot capture: `Camera.Screenshot` and ONVIF `GetSnapshotURI`

Status: approved design, not yet implemented.
Extends `2026-09-05-camfarm-architecture-design.md` and follows
`2026-09-13-dynamic-camera-lifecycle-design.md` (slice 1 of the 3-slice dashboard decomposition:
dynamic camera lifecycle, then screenshot capture — this doc — then the dashboard HTTP API/UI and
`cmd/camfarm` binary). It arrives now for the same reason slice 1 did: a real consumer, the eventual
operator dashboard, needs "see what a camera is currently showing" as a primitive, and `GetSnapshotURI`
is a real ONVIF client expectation this project's control plane currently faults on.

## 0. What this slice adds

1. **`internal/screenshot.Capture(ctx context.Context, rtspURL string) (image.Image, error)`** — the
   single leaf primitive. Dials `rtspURL` like any other RTSP client, waits for the next
   `RandomAccess` access unit, decodes it, and returns the image. Codec (H.264 vs H.265) is read from
   the DESCRIBE response's SDP, not passed in.
2. **`Camera.Screenshot(ctx context.Context) (image.Image, error)`** — the public Go API. Calls
   `screenshot.Capture(ctx, c.RTSPURL())`. No new exported types.
3. **ONVIF `GetSnapshotURI` support** — flip `onvif-go`'s `SnapshotConfig.Enabled` to `true`, add a new
   HTTP route on `internal/onvif.Server`'s existing mux that serves the actual bytes, and correct the
   SOAP response's advertised URL to point at that route under camfarm's real listener.

### 0.1 What it deliberately does not add

- **No keyframe priming/buffering on `internal/rtsp.Server`.** `Capture` waits naturally for the pump's
  next `RandomAccess` AU on the existing shared broadcast timeline; no per-session GOP cache is added.
- **No historical or timestamp-addressed frames.** Always "whatever comes next," never a cached
  previous frame.
- **No dashboard HTTP API, UI, or `cmd/camfarm` binary.** Slice 3, unstarted. This slice's only HTTP
  surface is the one ONVIF snapshot route, justified purely by `GetSnapshotURI` needing something real
  to point at.
- **No resolution/format negotiation.** Always native resolution, always PNG.
- **No decode retry, no caching, no rate limiting.** One dial, one wait, one decode attempt per call.

## 1. Motivation

The eventual operator dashboard needs to show what a camera currently sees — a live preview thumbnail
is the "off-load and glance" complement to slice 1's "load and off-load" cameras. Separately, a real
ONVIF client that calls `GetSnapshotURI` (a common capability probe) gets a fault today because
`internal/onvif/server.go` sets `Snapshot.Enabled: false`. Both needs are served by the same underlying
capability: capture and decode one frame from a running camera.

## 2. `internal/screenshot` — capture and decode

```go
package screenshot

// Capture dials rtspURL, waits for the next RandomAccess access unit the
// server sends, decodes it, and returns the image. The codec (H.264 vs
// H.265) is read from the DESCRIBE response's SDP, not passed in — the
// server already advertises it.
func Capture(ctx context.Context, rtspURL string) (image.Image, error)
```

Mechanics:

1. Dial `rtspURL` and send DESCRIBE with a real `gortsplib` client.
2. `FindFormat` for the advertised codec; read SPS/PPS (H.264) or VPS/SPS/PPS (H.265) directly off the
   SDP-populated `format.H264`/`format.H265` struct. `internal/screenshot` does not import
   `internal/media` — everything it needs comes from the live DESCRIBE, not from `*media.Media`.
3. SETUP and PLAY. Depacketize incoming RTP packets into NALUs via `rtph264.Decoder`/`rtph265.Decoder`
   (`Decode(pkt *rtp.Packet) ([][]byte, error)`, from `gortsplib/pkg/format/rtph264`/`rtph265`).
4. For each reassembled access unit, check `RandomAccess` using the existing
   `internal/media`-equivalent logic (`h264.IsRandomAccess`/`h265.IsRandomAccess` from
   `mediacommon`, already proven in `internal/media`'s own codec adapters, including the H.265 CRA
   open-GOP case). Discard non-random-access AUs; keep waiting.
5. Once a random-access AU arrives, format it as Annex B and decode:
   - H.264: `github.com/Eyevinn/hi264/pkg/decoder` — `dec := decoder.New(); frame, err :=
     dec.DecodeAnnexB(data)` (singular frame).
   - H.265: `github.com/Eyevinn/hi265/pkg/decoder` — `dec := decoder.New(); frames, err :=
     dec.DecodeAnnexB(data)` (plural — take the first).
6. Convert the decoded frame to `image.Image`:
   - H.264: `hi264/pkg/yuv.FrameToImage(f *frame.Frame) *image.NRGBA`, already provided by the library.
   - H.265: no equivalent ships in `hi265/pkg/yuv` (it has none). A small local adapter builds an
     `image.YCbCr` directly from `hi265/pkg/frame.Frame`'s `Y`/`Cb`/`Cr`/`StrideY`/`StrideC` fields —
     `image.YCbCr` natively models planar 4:2:0 YUV and its `.At()` does the BT.601-style conversion,
     so no manual color-space math is needed.
7. Respect `ctx` throughout: cancellation/deadline aborts the dial, the wait loop, and returns
   `ctx.Err()` unwrapped. This also bounds a pathological fixture that never produces a keyframe.

## 3. Public API — `Camera.Screenshot`

```go
// Screenshot captures and decodes the camera's current video frame.
//
// It dials the camera's own RTSP endpoint like any other client and waits
// for the next random-access frame the running stream produces — there is
// no shortcut around the wire. Latency is therefore bounded by the fixture's
// GOP structure: every source's first access unit is already guaranteed
// RandomAccess (internal/media's own invariant), so the wait is at most one
// full loop of the fixture. Cancel ctx to bound it yourself.
func (c *Camera) Screenshot(ctx context.Context) (image.Image, error) {
	return screenshot.Capture(ctx, c.RTSPURL())
}
```

Lives in `camfarm.go` next to `Stats`/`Inject`/`Clear`. No `Fleet`-level duplicate — Camera-scoped,
consistent with every other per-camera method.

Refusals, reusing the existing taxonomy (`errors.go`):

- **Fleet not started (RTSP server never bound):** `ErrNotReady`. Checked explicitly (e.g. via a
  `Started() bool` on `internal/rtsp.Server`) before dialing, so the refusal is immediate rather than
  waiting on a TCP timeout.
- **Camera removed mid-capture, or a caller holding a `*Camera` for an ID that no longer exists:** the
  dial itself fails naturally (matching the precedent in slice 1's
  `TestAddCameraDialableThenRemovedRefuses`); the error propagates as-is, not remapped to
  `ErrUnknownCamera` — this is a real TOCTOU race inherent to holding a `*Camera` value, not a new
  error case to invent.
- **`ctx` cancelled/deadline exceeded while waiting for a keyframe:** propagate `ctx.Err()` unwrapped.

## 4. ONVIF wiring

- Flip `Snapshot.Enabled: false` → `true` in `internal/onvif/server.go` (currently line ~150), add
  `Resolution` from the camera's `Media.Width`/`Height`.
- Add a `rtspURL string` field to ONVIF's per-camera `camera` struct (not currently stored — the RTSP
  URL is passed once into `onvif-go`'s config at construction and otherwise unused by camfarm's own
  dispatch layer).
- New route on `internal/onvif.Server`'s existing mux, alongside `/onvif/`:
  ```go
  mux.HandleFunc("/onvif/{id}/snapshot", s.handleSnapshot)
  ```
  Handler: look up the camera by ID under the same lock discipline as SOAP dispatch, call
  `screenshot.Capture(ctx, cam.rtspURL)`, encode as PNG, write with `image/png` content type. Same
  Digest auth requirement as every other ONVIF route — an unauthenticated frame grab would be a new
  fault this slice must not open.
- Extend `rewriteXAddr` (or a sibling function beside it in `dispatch.go`) to correct
  `GetSnapshotURIResponse.MediaURI.URI`. `onvif-go`'s `HandleGetSnapshotURI` always *derives* this URL
  from `advertisedBaseURL()` as `fmt.Sprintf("%s/snapshot?profile=%s", ...)` — the same class of
  self-referential-URL problem `rewriteXAddr` already patches for `GetCapabilities`/`GetServices`.
  The rewrite points the response at camfarm's real listener and the new `/onvif/{id}/snapshot` path
  instead of onvif-go's guessed shape.

## 5. Error handling & refusals

No new error values; every failure mode maps onto behavior that already exists elsewhere:

- **Snapshot HTTP route, unknown camera ID:** 404, same as every other ONVIF path miss today.
- **Snapshot HTTP route, capture fails** (dial refused, decode error, ctx deadline): 500 with the
  underlying error's message. No SOAP fault wrapping — this is a plain HTTP route, not a SOAP handler.
- **`GetSnapshotURI` SOAP call:** unaffected by capture failures — like `GetStreamURI` today, it only
  returns a URL. Only refuses if the camera ID itself doesn't resolve (existing dispatch behavior).
- **`Camera.Screenshot`:** per §3 — `ErrNotReady` if the RTSP server isn't started, natural
  dial/decode/`ctx` errors otherwise, unwrapped.

## 6. Testing strategy

Real dials, real decode throughout — no mocking `gortsplib`, `onvif-go`, or the decoder libraries, per
project convention.

- **`internal/screenshot` package tests:** a real `internal/rtsp.Server` serving both an H.264 fixture
  and an H.265 fixture; `Capture(ctx, url)` against each asserts a decoded `image.Image` with the
  fixture's known `Width`/`Height`. This is the one place both codec paths get direct coverage, given
  the hi264/hi265 frame-type asymmetry (§2).
- **Cancellation test:** cancel `ctx` before/during capture; assert `Capture` returns promptly with
  `ctx.Err()` — not a hang, not a leaked goroutine. Also guards against a pathological fixture with no
  keyframe ever.
- **`camfarm_test.go`:** `Camera.Screenshot(ctx)` end-to-end against a running `Fleet`, both codecs
  (the two bundled fixtures already differ by codec). `ErrNotReady` case against a fleet constructed
  but not started, or via the `Camera`-after-`RemoveCamera` TOCTOU path from §3.
- **`internal/onvif/server_test.go`:** `GetSnapshotURI` returns a URL under camfarm's real listener
  (regression test for the §4 URI fix); a real HTTP GET against `/onvif/{id}/snapshot` with Digest
  auth returns a decodable PNG.
- **`go test -race ./...`:** mandatory per existing CI job. Screenshot capture opens a fresh RTSP
  client connection per call — explicitly race this against concurrent `Fleet.RemoveCamera`/normal
  streaming reads on the same camera, since slice 1 just made `internal/rtsp.Server` dynamically
  mutable and this is new concurrent access onto that same path.

## 7. What's deferred

- **Keyframe priming/buffering on `internal/rtsp.Server`.** `Capture` accepts the natural wait,
  bounded by one loop of the fixture; no per-session GOP cache. A future slice could add this to cut
  latency, but it is a change to the pump's core broadcast model — real surface area not justified by
  this slice alone.
- **Screenshotting a specific or historical frame.** Always "whatever comes next." No history buffer
  exists anywhere in `camfarm` today.
- **The dashboard HTTP API, UI, and `cmd/camfarm` binary.** Slice 3, built on this slice and slice 1
  together.
- **Snapshot resolution/format options.** Always native resolution, always PNG.
- **Decode failure recovery/retry.** One dial, one wait, one decode attempt; surfaces as an ordinary
  error per §5.
- **Screenshot caching or rate limiting.** Every call is a fresh dial and fresh decode; no shared
  cache, no debounce.
