# Dashboard, binary and the first fault effect — design

Date: 2026-10-09. Status: approved design, not yet implemented.

Slice 3 of the dashboard decomposition (slice 1: dynamic camera lifecycle; slice 2: screenshot
capture, PR #2). It depends on slice 2 being merged: the dashboard serves snapshots through
`Camera.Screenshot`.

It combines three things that the user chose to keep in one spec and one plan:

1. **The first fault effect**, `frame_drop`, plus runtime fault injection (library).
2. **Library additions** the dashboard needs: a change feed, sentinel errors, accessors.
3. **The dashboard**: HTTP API, embedded UI, and the `cmd/camfarm` binary.

Naming: the display name is CamFarm (page title and heading, README prose, `--help`); module path, package and binary stay lowercase `camfarm`.

Constraints inherited from `CLAUDE.md`: `CGO_ENABLED=0` keeps building; determinism is the product;
real sockets in tests; no coverage figure or badge not produced by CI; the README keeps its
"simulates, not ONVIF conformant" Scope section; no employer or client names anywhere.

## 1. The `frame_drop` fault

### 1.1 Semantics

`frame_drop` is **encoder-side**: when it fires for an access unit, that access unit is not
encoded and not written. RTP sequence numbers therefore stay contiguous, and only the RTP
timestamp jumps by one frame interval. This models a camera or encoder that skips frames, and
exercises clients that assume contiguous frames or steady timestamps.

It is deliberately *not* network loss. A separate kind, `packet_loss` (encode, then withhold
packets, leaving a sequence gap), is a later slice. Each kind maps to one real bug class.

### 1.2 Decision and effect

- The decision stays in `internal/fault`: `Engine.DecideFrame(frameIndex)` is a pure function of
  the camera seed and the monotonic frame counter. The draw is unconditional, so the sequence does
  not depend on earlier outcomes (existing behaviour, `internal/fault/fault.go`).
- The effect lands in `Pump.Step` (`internal/rtsp/pump.go`), at the existing fault seam: when a
  `frame_drop` decision fires, `Step` skips encoding and writing, but still advances `idx`,
  `frameCounter`, the timestamp offset and the loop bookkeeping, so the stream stays in step with
  the source and a drop never shifts later decisions.
- The recorded observation changes from "decided; effect not implemented" to "applied" for
  `frame_drop`. Kinds without an effect keep the "not implemented" detail.

### 1.3 `protect_keyframes`

`fault.Spec` gains `ProtectKeyframes bool` (default false). When true, a decision that would drop a
random-access access unit (`AccessUnit.RandomAccess`) is suppressed and recorded as such. When
false, keyframes can drop, which corrupts the GOP until the next one; that is the realistic
failure, and it interacts with `Camera.Screenshot`, which then waits for a later keyframe.

`keyframe_starvation` remains a separate, deliberate fault and stays unimplemented here.

### 1.4 Configuration and validation

- `FaultSpec` (root package) gains `ProtectKeyframes bool` with a JSON tag.
- `Spec` validation and `AddCamera` stop refusing `frame_drop`; every other kind keeps returning
  `ErrUnsupported`, naming the kind. Rate validation is unchanged (`[0,1]`).

### 1.5 Runtime inject and clear, journaled

`Camera.Inject(FaultSpec)` and `Camera.Clear()` hand a new fault set to the camera's pump. The pump
swaps it in **between frames**, so no frame is half-affected, and nothing in `Engine` needs a lock
(it stays single-goroutine, owned by the pump).

Every swap is appended to a per-fleet **fault journal**: `(camera, frameIndex, action, spec)`.
`Fleet.ReplayLine()` includes a hash or count of the journal, and the journal itself is exposed so
a test can print it. The draw sequence is always seed-derived; what a replay additionally needs is
*which frame the injection landed on*, and that depends on when a human clicked. The spec states
that limit plainly rather than claiming wall-clock-independent replay of interactive runs.

An engine created by a later `Inject` seeds each kind from the camera seed by kind name, exactly as
`fault.New` does today, so injecting the same fault into the same camera always starts the same
sequence.

*Later ruling:* injecting a fault identical to the one already active is a no-op (no restart, no
journal entry), and only the fault classes (per-frame, per-request) whose list actually changed
restart their streams. To restart a seeded sequence deliberately, `Clear` and then `Inject`.

## 2. Library additions

- **Change feed.** `Fleet.Subscribe(ctx) <-chan Event`, where `Event` carries a type
  (`camera_added`, `camera_removed`, `fault_injected`, `fault_cleared`), the camera ID, and a
  monotonically increasing sequence number. Delivery is best-effort with a bounded buffer per
  subscriber; a slow subscriber is dropped and its channel closed rather than blocking the fleet.
  The dashboard re-lists on reconnect, so no replay of missed events is needed.
- **Sentinel errors.** `ErrDuplicateCamera` and `ErrInvalidSpec`, wrapped by the existing
  validation messages, so the HTTP layer can map errors without string matching.
- **Accessors.** `Camera.Spec() CameraSpec` and `Camera.Auth() (user, pass string, ok bool)`, so the
  dashboard can show credentials and the ONVIF endpoint without reaching into internals. Additive;
  no existing signature changes.
- **Add journal.** Runtime `AddCamera` order fixes camera seeds (`nextIndex`), so the replay line
  also records the add/remove journal. This closes a gap the replay line has today for dynamic
  fleets.

## 3. The dashboard (`internal/dashboard`)

An `http.Handler` constructor taking `*camfarm.Fleet` and an options struct. It imports only the
public `camfarm` package; DTOs live in the dashboard package, not the root.

### 3.1 API (JSON, under `/api/v1/`)

| Method and path | Purpose |
|---|---|
| `GET /fleet` | seed, spec hash, replay line, fault journal |
| `GET /cameras` | list with status and stats |
| `POST /cameras` | add a camera (body: `CameraSpec`) |
| `GET /cameras/{id}` | one camera: URLs, auth, seed, stats, active faults |
| `DELETE /cameras/{id}` | remove a camera |
| `GET /cameras/{id}/snapshot.jpg` | one JPEG frame via `Camera.Screenshot` |
| `POST /cameras/{id}/faults` | inject a fault (body: `FaultSpec`) |
| `DELETE /cameras/{id}/faults` | clear faults |
| `GET /events` | Server-Sent Events from `Fleet.Subscribe` |

Status mapping: 409 duplicate, 404 unknown camera, 422 `ErrUnsupported` or `ErrInvalidSpec`,
504 snapshot timeout, 500 otherwise. Mutations require `Content-Type: application/json`.

### 3.2 Snapshots

On demand only; never auto-polled across all cameras. Each snapshot is a full RTSP dial that waits
for a keyframe, so the dashboard layer adds: per-camera in-flight coalescing (one capture shared by
concurrent requests), a request timeout (10 s, a named constant), and a global concurrency cap
(4, a named constant). `Capture` itself stays free of caching, per slice 2's spec.

### 3.3 UI

One embedded page (`go:embed`, HTML plus vanilla JS, no Node, no vendored library). It shows a
camera table (RTSP URL, ONVIF endpoint and its `/device`, `/media`, `/snapshot` paths, credentials
if set, seed, stats) with copy buttons; an add-camera form (ID, optional credentials, fixture or
file source); remove; an on-demand snapshot view; a `frame_drop` control (rate and
`protect_keyframes`) with a clear button; and the fleet replay line. The list is kept current from
`/events`, re-listing on connect. Fault kinds other than `frame_drop` are shown as "not
implemented" and disabled.

**Designing the UI with Playwright.** The page is built iteratively against a running dashboard:
each design pass is checked in a real browser through the Playwright tooling (accessibility
snapshot to confirm structure and labels, screenshots at ~400px and desktop widths, light and dark
colour schemes forced via media emulation, console and network inspection). The page must follow
the system colour scheme through CSS tokens, keep a 16px side gutter, and never scroll
horizontally; tables scroll inside their own container. Screenshots are working material and are
not committed.

## 4. The binary (`cmd/camfarm`)

`run(ctx, args, stdout, stderr) error` is the testable core; `main` only wires signals.

- Flags: `--spec file.json` (the existing `Spec` JSON), `--seed`, `--cameras N` (cameras `cam-1..N`
  on the bundled fixture), `--http-addr` (default `127.0.0.1:8080`), `--listen-host`,
  `--rtsp-port`, `--onvif-port`.
- If `--seed` is absent, one is drawn from `crypto/rand`. The replay line is printed to stderr at
  startup and shown in the UI, so any run can be repeated with `--seed`.
- Graceful shutdown on SIGINT/SIGTERM closes the HTTP server, then the fleet.

## 5. Security posture (stated, not mitigated)

The default bind is loopback. By decision there are no further guards in this slice: no token, no
Host-header check, no CSRF defence, and file sources are not gated. The API can start servers and,
with `source.kind=file`, make the process read local paths. A malicious web page can reach a
loopback API via DNS rebinding or CSRF. The README and `--help` say this plainly. Hardening
(`--allow-remote`, Host check, `--allow-file-sources`, token) is a recorded follow-up, not a
silent omission. The dashboard displays Digest credentials; treat that page as sensitive.

## 6. Testing

- **Pump (hermetic).** Drive `Step` directly with the recording writer: a fixed seed yields a fixed
  set of dropped frame indices; RTP sequence numbers stay contiguous across a drop; timestamps jump
  by exactly one frame interval; counters and loop bookkeeping are unchanged by a drop;
  `protect_keyframes` suppresses a drop on a random-access AU and records it.
- **Engine.** `ProtectKeyframes` handling; same seed and spec give an identical sequence after a
  mid-run swap.
- **Fleet.** Spec-time `frame_drop` accepted, other kinds refused with `ErrUnsupported`; inject and
  clear on a running camera take effect at a frame boundary and append to the journal; the replay
  line changes when the journal does; `Subscribe` delivers events in order and drops a stalled
  subscriber without blocking add/remove. Real RTSP: a camera with rate 1.0 and
  `protect_keyframes=false` stops delivering frames to a real client; with rate 0.5 and a fixed
  seed the client sees the expected gaps.
- **Dashboard.** `httptest` against a `StartT` fleet: add/remove round trip, error mapping (409,
  404, 422), snapshot decodes as a 320x240 JPEG, coalescing (concurrent requests produce one
  capture), snapshot timeout, fault inject/clear, SSE ordering and reconnect re-list. Run under
  `-race`; a snapshot racing `RemoveCamera` and a fault inject must not panic or hang.
- **Binary.** `run` tested directly (flags, seed printed, `--spec` load, shutdown). `CGO_ENABLED=0
  go build ./...` stays green.
- **Browser end to end (Playwright).** By decision, the dashboard is tested in a real browser, using
  `github.com/playwright-community/playwright-go` (pure Go driver, Chromium). The tests start the
  real handler on a loopback port with a real fleet and drive the page: the camera table renders;
  add-camera form creates a camera and the row appears via SSE; remove deletes it; the snapshot
  button shows an image whose natural size is 320x240; the `frame_drop` control posts the fault and
  the page shows it active and clearable; copy buttons put the RTSP URL on the clipboard; the page
  works at a ~400px viewport and in both light and dark colour schemes; the console has no errors.
  Placement: files carry the `e2e` build tag, so the default `go test ./...` (including the
  `CGO_ENABLED=0` build job) needs no browser. A dedicated CI job `e2e` installs Chromium
  (`go run github.com/playwright-community/playwright-go/cmd/playwright install --with-deps
  chromium`) and runs `go test -tags e2e ./internal/dashboard/...`. It is a required check like the
  others; no test is skipped when the browser is missing. This job's pass/fail is the only evidence
  for any claim about UI behaviour.
- **Real wire only.** No mocks of the RTSP or HTTP stacks; the hermetic pump test uses the existing
  recording writer, as it does today.

## 7. Out of scope (deferred)

`packet_loss`, `keyframe_starvation` and every other fault effect; SSE replay of missed events;
MJPEG or an auto-refreshing snapshot grid; H.265 snapshots and selecting the H.265 fixture from a
`Spec` (decide in the transcoding slice); transcoding; auth, token or Host-header hardening; a
persisted add/fault journal; cross-browser (Firefox, WebKit) testing, since only Chromium is run; WS-Discovery; any coverage or benchmark
claim.

## 8. Docs

README: a Works bullet for the dashboard and binary, a Works bullet for `frame_drop` (stating it is
encoder-side and that other fault kinds are still refused), the security posture from section 5,
and the Status "Not built yet" list updated. The Scope / not-ONVIF-conformant section is
unchanged.
