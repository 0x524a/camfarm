# Dynamic camera lifecycle: `Fleet.AddCamera`/`RemoveCamera`

Status: approved design, not yet implemented.
Extends `2026-09-05-camfarm-architecture-design.md` §6 (Decision 9 — library and binary: "the
control API mutates [the spec] at runtime"), §10.1 (packages), §10.6 (Decision 8 — configuration
surface: "both, with one source of truth... the control API mutates it at runtime"), and §10.7
(testing strategy). This slice *is* the control-API layer both of those sections already
anticipated; it arrives now because a real consumer needs it — a minimal operator dashboard (see
`Motivation`) — not speculatively. It also resolves `CLAUDE.md`'s open item 6 ("Configuration
surface") in the direction §10.6 had already picked: a runtime API alongside the declarative spec,
brought forward from that section's original "UI last" phasing because the UI that needs it is
being built now.

## 0. What this slice adds

1. **`Fleet.AddCamera(spec CameraSpec) (*Camera, error)`** and **`Fleet.RemoveCamera(id string)
   error`** — the public Go primitive. Per §6's ordering ("the Go API is the contract, the HTTP
   control API is a client of it"), everything the later dashboard slice does is built on these
   two methods and nothing else.
2. **`internal/rtsp.Server.AddCamera`/`RemoveCamera`** — register or unregister a camera on an
   already-running server: build its `ServerStream`, `Initialize()` it, build its `Pump`, and run
   or stop that pump's goroutine independently of every other camera's.
3. **`internal/onvif.Server.AddCamera`/`RemoveCamera`** — insert or delete a camera from the
   server's existing dynamic dispatch map. No goroutine lifecycle: ONVIF is request/response only.
4. **Per-camera validation factored out of `Spec.validate()`** into a helper both `Start()` and
   `AddCamera` call, so the two paths can't drift on what a valid `CameraSpec` is.
5. **The media-source cache becomes fleet-lifetime state**, promoted from a local variable inside
   `Start()` to a `Fleet` field, so `AddCamera` can share an already-loaded source the same way
   `Start()` shares one across cameras present from the beginning.
6. **A monotonically increasing seed-index counter**, independent of `RemoveCamera`, so a
   dynamically added camera's derived seed (`root.Camera(nextIndex)`) is a pure function of *call
   order* — reproducible if that order is logged, per `CLAUDE.md`'s "Determinism is the product."

### 0.1 What it deliberately does not add

- **No forced disconnection of an in-flight RTSP session.** `RemoveCamera` stops a camera from
  answering new `DESCRIBE`/`SETUP` and stops its pump from producing further frames, but a client
  already mid-session on that path is not kicked — it simply stops receiving packets until it tears
  down on its own. Stated here so the implementation plan doesn't have to discover it by accident.
- **No media-source cache eviction.** A source stays loaded for the fleet's entire lifetime even
  after the last camera using it is removed. A deliberate simplicity-over-memory trade: the bundled
  fixtures are small, and eviction adds a reference-counting surface with no proven need yet.
- **No in-place mutation of a running camera's `VideoSpec` or `AuthSpec`.** Only whole-camera
  add/remove. In-place editing of `VideoSpec` isn't well-defined anyway while it stays
  passthrough-only (§5 of the master design).
- **No config-file-driven runtime reload.** That's the eventual binary's concern (a later slice),
  layered on top of this Go API. This slice is purely the primitive underneath it.
- **No dashboard, no screenshot.** Two separate slices, in that order, per the 3-slice decomposition
  agreed before this doc was written.

## 1. Motivation

A minimal operator dashboard needs to add and remove cameras from a running fleet — that's the
"load and off-load" requirement it exists to satisfy. Today `Fleet` builds its entire camera set
once inside `Start()` and has no notion of a camera arriving or leaving afterward. Both
`internal/rtsp.Server` and `internal/onvif.Server` already dispatch by looking a camera ID up in a
map under a lock, though, rather than through any build-time-only registration — so this is a
lifecycle problem, not a routing rewrite.

## 2. Mechanics — ONVIF side

`internal/onvif.Server` dispatches every request through one `http.ServeMux` pattern (`/onvif/`)
whose handler looks the camera up in `s.cams map[string]*camera`. Adding a camera is: validate
(non-empty ID, no duplicate), build one `onvif-go *server.Server` for it exactly as `New()` already
does per camera, call `UpdateStreamURI` with the RTSP URL `internal/rtsp.AddCamera` just returned,
and insert into `s.cams` under the write lock. Removing is: delete from `s.cams` under the write
lock. No goroutines, no stream to close — this side is genuinely simple.

## 3. Mechanics — RTSP side

This is the real work. `internal/rtsp.Server.Start()` today builds *every* pump under one shared
`context.Context`/`cancel`/`sync.WaitGroup`: correct for a fixed set built once, but there is no
way to stop a single pump without stopping all of them.

**Refactor:** `Server` keeps the root context it derives `cancel` from (not just the `cancel` func).
Every camera's pump — whether built during `Start()` or later by `AddCamera` — runs on its own
`context.WithCancel(rootCtx)` child, not on the root context directly. Cancelling the root (via
`Close()`) still cancels every child by ordinary context propagation, so `Close()`'s behavior is
unchanged. But because each camera has its *own* cancel, `RemoveCamera` can stop exactly one
camera's pump without touching any other — and this applies uniformly to a camera present since
`Start()` and one added afterward alike. "Off-load a stream" in the dashboard needs to work on any
camera the operator sees, not only ones added after the fact, so there is deliberately no
special-casing between the two origins. Alongside its own `cancel`, every camera gets its own `done
chan struct{}`, closed when its pump goroutine returns — a `sync.WaitGroup` can't wait for one
specific member, so `RemoveCamera` needs this to know when it's safe to close the camera's stream.

**`AddCamera(cc CameraConfig) error`:** under the write lock, run the same validation `New()`
already runs per camera (empty ID, reserved path characters, `cc.Media` non-nil, SPS/PPS present,
VPS present for H.265, no duplicate ID against the current `s.cams`). Build the RTP format via the
existing `newFormat`, build the `description.Media` and `gortsplib.ServerStream`, `Initialize()` it
against the running `gortsplib.Server` — this requires `s.srv != nil`, i.e. `AddCamera` before
`Start()` is refused. Build the `Pump`. Add to `s.cams`/`s.order`. Start its goroutine on the child
context described above, tracked in both the shared `s.wg` (so `Close()` still waits for it) and
its own `done` channel (so `RemoveCamera` can wait for exactly this one).

**`RemoveCamera(id string) error`:** under the write lock, look the camera up, delete it from
`s.cams`/`s.order` **first** — this is what makes new `DESCRIBE`/`SETUP` on that path 404
immediately — and capture its stream, cancel func, and done channel. Release the lock (same
"release before calling into gortsplib" discipline `Close()` already documents and follows), call
`cancel()`, wait on `done`, then close the stream.

## 4. Fleet-level orchestration

`AddCamera` on `Fleet`: run the shared per-camera validator (item 4 of §0). Look up the fleet's
persistent source cache (item 5) by `SourceSpec`; load and cache it if new, reuse it if not — same
sharing behavior `Start()` already gives cameras present from the beginning. Derive the seed from
the monotonic counter (item 6). Call `srv.AddCamera`, then `onvifSrv.AddCamera`; if the ONVIF call
fails, roll back by calling `srv.RemoveCamera` on the RTSP side before returning the error — the
same unwind-on-partial-failure discipline `Start()` already applies when one half of construction
succeeds and the other doesn't. On success, register the new `*Camera` in `f.cameras`/`f.order`
under `Fleet`'s own lock.

`RemoveCamera` on `Fleet`: best-effort once the ID is known to exist — there's nothing a caller
could sensibly do to retry a partial removal, so this doesn't need Add's rollback discipline.
Remove from ONVIF dispatch and RTSP dispatch (in either order, since both must complete for the
camera to be considered gone) before returning, then drop it from `f.cameras`/`f.order`.

## 5. Testing strategy

Per §10.7 of the master design ("mid-test mutation of a running fleet is a core feature, so racing
is a real risk here rather than a formality") — `go test -race` coverage of every add/remove path
is non-negotiable, not optional hardening.

- `internal/rtsp/server_test.go`: add a camera to a running server and dial it with a real
  `gortsplib` client while a pre-existing camera keeps serving unaffected; remove a camera and
  assert its path 404s while other cameras are unaffected; remove a camera with a session already
  set up on it and assert the pump stops producing new packets without panicking or racing.
- `internal/onvif/server_test.go`: same add/remove shape, asserting `GetStreamURI` on an added
  camera returns its real RTSP URL, and a removed camera's endpoint stops answering.
- `camfarm_test.go`/`integration_test.go`: end-to-end `Fleet.AddCamera` → dial the returned
  `Camera.RTSPURL()` with a real client → `Fleet.RemoveCamera` → same URL refused. A determinism
  test mirroring the existing `TestSameSeedReproducesCameraSeeds` pattern: two fleets built from the
  same seed, driven through the same sequence of `AddCamera` calls, derive the same seeds for the
  dynamically added cameras. A fault-bearing spec passed to `AddCamera` is refused with the same
  `ErrUnsupported` `Start()` already gives — the shared validator (§0 item 4) is what guarantees
  this rather than leaving it to be independently re-implemented and possibly missed.
- `go test -race ./...` covering every new test above, in the same CI job §10.7 already runs it in.

## 6. Public API additions

```go
func (f *Fleet) AddCamera(spec CameraSpec) (*Camera, error)
func (f *Fleet) RemoveCamera(id string) error
```

No new exported types. No new error values beyond existing `ErrUnsupported`/`ErrUnknownCamera`,
reused rather than duplicated.

## 7. What's deferred

- **Screenshot capture** — next slice, independent of this one (works against the fleet as it
  exists today, whether or not a camera was added dynamically).
- **The dashboard HTTP API and UI, and the `cmd/camfarm` binary** — slice after that, built on both
  this slice and screenshot capture.
- **Forced eviction of an in-flight RTSP session on `RemoveCamera`.** Not planned; §0.1 states why.
- **Media-source cache eviction.** Not planned; §0.1 states why.
- **Runtime mutation of a running camera's `VideoSpec`/`AuthSpec`.** Whole-camera add/remove only.
