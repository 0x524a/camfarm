# Stream quality test — design

Date: 2026-10-12. Status: approved design, not yet implemented.

A "test stream quality" action for each camera in the dashboard API, built on `rtspeek` v1.0.0.
This spec covers the API and the runner only; the button and results panel belong to the dashboard
redesign sub-project.

## Purpose

CamFarm injects faults on the server side. Until now the only way to see what a client actually
receives was to point another tool at the camera by hand. This feature runs `rtspeek`'s stream
analysis against a camera's own RTSP URL and reports what arrives: packet loss, jitter, bitrate,
frame rate, keyframe interval, timestamp jumps, broken frames and B-frame reordering, with findings
ranked by severity.

Two uses motivate it:

- **See a fault from the outside.** Inject `packet_loss` or `pts_dts_mismatch`, run the test, and
  see the matching finding.
- **Check a library file end to end.** Add a camera from a library file (`libraryId`), then run the
  test to see how the file actually streams.

Success:

- `POST` starts a test and returns at once; the result is fetched later and survives a page reload.
- A test on a camera with an injected fault reports the corresponding finding.
- A test cannot be aimed at anything but the camera it was started for.
- Nothing about a test changes a fleet's journal, replay hash or injected faults.

Constraints inherited from `CLAUDE.md`: `CGO_ENABLED=0` keeps building; determinism is the product
(see "What is and is not reproducible"); real sockets in tests; no coverage figure or claim that CI
did not produce; no employer or client names.

## Decisions

| Question | Decision |
|---|---|
| How does a run deliver its result? | Asynchronous. `POST` returns 202; the latest result per camera is kept in memory; an SSE event announces completion. |
| What can a caller set? | Duration (default 5 s, 1 to 30 s) and transport (`auto`, `udp`, `tcp`). Thresholds stay `rtspeek`'s defaults, so results are comparable between runs. |
| Where does it live? | `internal/quality` (runner) and thin handlers in `internal/dashboard`. Not in the public `camfarm` package. |
| How do events reach the page? | Through the dashboard's own bus, merged into `/api/v1/events`. Not through the fleet's event feed. |

The fleet's feed is excluded on purpose: `Fleet.publish` records journal entries and feeds the
replay hash. A quality run is a measurement, not a change to the fleet, so routing it there would
change a fleet's replay identity.

## 1. Package `internal/quality`

```go
type Transport string // "auto" | "udp" | "tcp"

type Request struct {
    DurationSeconds float64   `json:"durationSeconds,omitempty"` // 0 means 5
    Transport       Transport `json:"transport,omitempty"`       // "" means auto
}

type State string // "running" | "done" | "failed"

type Run struct {
    Camera     string            `json:"camera"`
    State      State             `json:"state"`
    Request    Request           `json:"request"`     // after defaults
    StartedAt  time.Time         `json:"startedAt"`
    FinishedAt *time.Time        `json:"finishedAt,omitempty"`
    Verdict    string            `json:"verdict,omitempty"` // "ok" | "warn" | "error"; set when done
    Analysis   *rtspeek.Analysis `json:"analysis,omitempty"`
    Error      string            `json:"error,omitempty"`   // set when failed
}

type Options struct {
    MaxConcurrent int // 0 means 2
    // Analyze is the rtspeek seam; nil means the real call. Tests only.
    Analyze func(ctx context.Context, url string, o rtspeek.AnalyzeOptions) (*rtspeek.Analysis, error)
}

func New(opt Options) *Runner
func (r *Runner) Start(camera, rtspURL string, req Request) (Run, error)
func (r *Runner) Get(camera string) (Run, bool)
func (r *Runner) Cancel(camera string) bool
func (r *Runner) Forget(camera string)   // cancels a running test and drops the result
func (r *Runner) Subscribe(ctx context.Context) <-chan Event
func (r *Runner) Close()                 // cancels every run and ends subscriptions
```

Errors: `ErrBadRequest` (options out of range or unknown transport), `ErrRunning` (that camera
already has a test running), `ErrBusy` (the global cap is full).

### Behaviour

- `Start` validates the request, applies defaults, refuses a second concurrent run on the same
  camera, and refuses beyond `MaxConcurrent` runs in total. It then runs the analysis in a
  goroutine with its own context (not the HTTP request's), bounded by the duration plus
  `rtspeek`'s connection and first-packet budgets.
- The verdict is the worst finding severity: `error` if any finding is an error, else `warn` if any
  is a warning, else `ok` (info findings alone are `ok`).
- A run whose analysis reports an `Error`, or whose connection fails, is `failed` and carries the
  message. Findings may still be present.
- `rtspeek` reports a session that dies or never delivers as findings (`no_packets`,
  `stream_interrupted`, `setup_failed`) inside a normal `done` result. Only a failure to run the
  analysis at all is `failed`.
- A camera fault that breaks the session (`teardown`, `rtsp_error`) therefore shows up as findings
  or a `failed` run. It is never a server error.
- Results are held in memory, the latest per camera. A new run replaces the previous result once it
  starts. They are lost on restart.
- `Forget` runs when a camera is removed; `Close` runs at shutdown. Both cancel running tests and
  wait for them to stop, so no goroutine outlives the dashboard.
- `Event{Camera, State, Verdict}` is published on start, completion, failure and cancellation.
  Subscribers that fall behind are dropped (they re-list on reconnect), as the fleet feed does.

## 2. Dashboard API

All handlers are thin: parse, call the runner, encode. JSON names are lower camel case.

| Method and path | Behaviour |
|---|---|
| `POST /api/v1/cameras/{id}/quality` | JSON body `{durationSeconds?, transport?}` (an empty body is allowed). 202 with the `Run`. 404 unknown camera; 409 a test is already running on it; 429 the global cap is full (with `Retry-After`); 422 bad options. |
| `GET /api/v1/cameras/{id}/quality` | 200 with the latest `Run`; 404 if the camera is unknown or has no result yet. |
| `DELETE /api/v1/cameras/{id}/quality` | 204 if a running test was cancelled; 404 if nothing is running. |

- The camera DTO gains `quality`: `{state, verdict?, finishedAt?}` when a result exists, so a camera
  card can show a badge without a second request.
- `/api/v1/events` merges the runner's events into the same stream as `event: quality` with
  `{camera, state, verdict}`. The fleet's own events are unchanged.
- The URL under test is always `Camera.RTSPURL()`. It is never read from the request, so the
  endpoint cannot be used to reach another host.
- `POST` requires `Content-Type: application/json`, like every other mutating camera endpoint. It
  also goes through the same cross-site check as the library's mutating endpoints.
- Responses contain the `Analysis` as `rtspeek` encodes it. No credentials are involved: camfarm's
  RTSP is open.

## 3. What is and is not reproducible

CamFarm's guarantee is that an injected fault is reproducible from a seed. A quality test does not
extend that guarantee to its own numbers. Jitter, measured fps and bitrate depend on timing and
load on the machine, and the window is a few seconds. What a test can show reliably is the presence
of a strong fault, such as a seeded `packet_loss` at a high rate producing a `packet_loss` finding.
The README and the API documentation say this plainly, and say that a test occupies one session on
the camera (the camera's reader count rises by one while it runs).

## 4. Dependency

- Adds `github.com/0x524A/rtspeek` v1.0.0. The module path has a capital `A`; Go module paths are
  case-sensitive.
- Moves `github.com/bluenviron/mediacommon/v2` from v2.9.4 to v2.9.5, which `rtspeek` requires.
- `rtspeek` is MIT licensed. It needs no cgo, so `CGO_ENABLED=0` still builds.
- Only `pkg/rtspeek` is imported; the CLI dependencies in its `go.mod` are not linked.
- It is imported from `internal/quality` only, so the public `camfarm` package gains no new
  dependency.

## 5. Testing

- **Runner, with the fake analyser seam:** each state transition; a second run on one camera is
  refused; the global cap is enforced and slots are released on every path (done, failed,
  cancelled, forgotten); `Cancel` and `Forget` stop a blocked analysis; `Close` leaves no goroutine
  (checked under `-race`); the verdict mapping for each severity mix; defaults and range checks
  for duration and transport; events are published in order and a slow subscriber is dropped.
- **Dashboard handlers:** every status in the table above, including 429 with `Retry-After`, the
  empty body, an unknown field (400), a wrong content type (415), the cross-site refusal, and the
  `quality` summary on the camera DTO. Removing a camera forgets its result and cancels its run.
  The `quality` SSE event arrives on `/api/v1/events`, and the fleet's journal and replay hash are
  identical with and without a test run.
- **End to end, real `rtspeek`:** a fixture camera tested over TCP for about 2 s returns tracks, a
  positive frame count and no error-severity finding. The same flow on a camera with a seeded,
  strong `packet_loss` fault returns a `packet_loss` finding. Assertions are coarse (a finding is
  present or absent), never exact figures.
- A library-file flow: upload, add a camera from the file with `libraryId`, run the test, get a
  result.
- Coverage is whatever CI reports. No figure is written into the README.

## 6. Out of scope

- The dashboard button and results panel (redesign sub-project).
- Overriding thresholds, a history of runs, comparing two runs, scheduled or repeated tests.
- Testing a stream that is not one of the fleet's cameras.
- Persisting results across restarts.
- Exposing the analysis through the public `camfarm` package (a later `Camera.AnalyzeQuality` could
  reuse the runner once the shape is proven).

## 7. Risks

- **Flaky end-to-end assertions.** Short windows make jitter and fps noisy. Mitigated by asserting
  only on the presence of strong faults and on the absence of error findings for a clean fixture
  camera, and by keeping any timing-sensitive number out of assertions.
- **Load.** Each run is a real RTSP client reading for up to 30 s. The cap of 2 and the
  one-per-camera rule bound it; a UDP run also opens sockets on the host.
- **A fault that hides the problem.** `rtspeek` documents that TCP hides network loss. The UI
  sub-project should default the transport to `auto` and explain `udp`.
- **Dependency drift.** `rtspeek`'s finding codes are documented as stable once released, but the
  `Analysis` shape is its own. The API returns it as is; the end-to-end tests pin the fields camfarm
  relies on.
