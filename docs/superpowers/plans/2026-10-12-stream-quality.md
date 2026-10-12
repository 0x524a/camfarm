# Stream Quality Test Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `POST/GET/DELETE /api/v1/cameras/{id}/quality` runs `rtspeek`'s stream analysis against a camera's own RTSP URL, asynchronously, and keeps the latest result per camera in memory.

**Architecture:** A new `internal/quality` package owns the runner (one goroutine per run, a global cap, one run per camera, an event bus). Thin handlers in `internal/dashboard` call it. Completion events reach the page through the dashboard's own bus merged into `/api/v1/events`, never through the fleet's feed, so a run cannot touch the journal or replay hash.

**Tech Stack:** Go 1.26 (`CGO_ENABLED=0`), `github.com/0x524A/rtspeek` v1.0.0 (`pkg/rtspeek`), standard `net/http`.

**Spec:** `docs/superpowers/specs/2026-10-12-stream-quality-design.md`

## Global Constraints

- Module path `github.com/0x524a/camfarm`. The rtspeek module path has a capital `A`: `github.com/0x524A/rtspeek`; import `github.com/0x524A/rtspeek/pkg/rtspeek` only.
- `CGO_ENABLED=0 go build ./...` must keep passing. `go mod tidy` must leave `go.mod`/`go.sum` unchanged (CI checks it). `mediacommon/v2` moves v2.9.4 to v2.9.5.
- rtspeek is imported from `internal/quality` only. The public `camfarm` package gains no dependency.
- The URL under test is always `Camera.RTSPURL()`; never read from a request.
- Quality events must NOT go through `Fleet.publish`/the fleet feed (journal and replay hash).
- Duration: default 5 s, range 1 to 30 s. Transport: `auto` (default), `udp`, `tcp`. Global cap default 2. One run per camera.
- Handlers: 202 start, 404 unknown camera, 409 already running, 429 cap full with `Retry-After`, 422 bad options, 400 unknown field, 415 wrong content type, 403 cross-site (same guard as the library's mutating routes).
- JSON names lower camel case, except the `analysis` object which is rtspeek's own encoding, returned as is.
- No coverage figure or claim in any doc or message that CI did not produce. No employer or client names anywhere. Commit only under the repo's configured identity. Do not push or merge.
- Determinism: measured numbers (jitter, fps, bitrate) are NOT replayable. Tests assert only the presence or absence of strong findings, never exact figures.

## Review Focus

1. A camera is removed (or the dashboard shuts down) while its test runs: the run is cancelled, nothing writes a stale result, no goroutine outlives `Close`. (Task 2, Task 3)
2. A finished run from a forgotten camera must not overwrite a newer run started under the same id. (Task 2)
3. A camera whose injected fault kills the session (`teardown`, `rtsp_error`) is reported as findings or a `failed` run, never a 5xx. (Task 5)
4. `POST` with no body, with a `null` body, with chunked transfer encoding, or with a huge duration or NaN-like value. (Task 3)
5. A slow or stalled `/api/v1/events` client must not block the runner or other subscribers. (Task 1, Task 4)

## File Structure

- Create `internal/quality/quality.go`: types, validation, verdict, `Runner` (`New`, `Start`, `Get`, `Subscribe`, `Close`), event bus.
- Create `internal/quality/lifecycle.go`: `Cancel`, `Forget`.
- Create `internal/quality/quality_test.go`, `internal/quality/lifecycle_test.go`: fake-analyser tests.
- Create `internal/dashboard/quality.go`: the three handlers, the `quality` camera summary, body helper.
- Modify `internal/dashboard/dashboard.go`: `Options.Quality`, `server.quality`, routes, DTO field, `removeCamera` forgets.
- Modify `internal/dashboard/events.go`: merge `event: quality`.
- Create `internal/dashboard/quality_test.go`, `internal/dashboard/quality_real_test.go`.
- Modify `cmd/camfarm/run.go`: create and close the runner.
- Modify `README.md`: a short section.
- Modify `go.mod`, `go.sum`.

---

### Task 1: Dependency and the runner core

**Files:**
- Modify: `go.mod`, `go.sum`
- Create: `internal/quality/quality.go`
- Test: `internal/quality/quality_test.go`

**Interfaces:**
- Consumes: `rtspeek.AnalyzeOptions`, `rtspeek.Analysis`, `rtspeek.Finding`, `rtspeek.DescribeStreamWithOptions`, `rtspeek.Options`, `StreamInfo.GetAnalysis()`, constants `rtspeek.SeverityWarn`, `rtspeek.SeverityError`.
- Produces (used by Tasks 2 to 5):
  ```go
  type Transport string  // TransportAuto "auto", TransportUDP "udp", TransportTCP "tcp"
  type Request struct { DurationSeconds float64 `json:"durationSeconds,omitempty"`; Transport Transport `json:"transport,omitempty"` }
  type State string      // StateRunning "running", StateDone "done", StateFailed "failed"
  type Run struct { Camera string `json:"camera"`; State State `json:"state"`; Request Request `json:"request"`; StartedAt time.Time `json:"startedAt"`; FinishedAt *time.Time `json:"finishedAt,omitempty"`; Verdict string `json:"verdict,omitempty"`; Analysis *rtspeek.Analysis `json:"analysis,omitempty"`; Error string `json:"error,omitempty"` }
  type Event struct { Camera string `json:"camera"`; State State `json:"state"`; Verdict string `json:"verdict,omitempty"` }
  type Options struct { MaxConcurrent int; Analyze func(ctx context.Context, url string, o rtspeek.AnalyzeOptions) (*rtspeek.Analysis, error) }
  var ErrBadRequest, ErrRunning, ErrBusy, ErrClosed error
  func New(opt Options) *Runner
  func (r *Runner) Start(camera, rtspURL string, req Request) (Run, error)
  func (r *Runner) Get(camera string) (Run, bool)
  func (r *Runner) Subscribe(ctx context.Context) <-chan Event
  func (r *Runner) Close()
  const VerdictOK = "ok"; VerdictWarn = "warn"; VerdictError = "error"
  ```

- [ ] **Step 1: Add the dependency**

Run:
```
go get github.com/0x524A/rtspeek@v1.0.0
```
Do not run `go mod tidy` yet (nothing imports it until Step 4).

- [ ] **Step 2: Write the failing tests**

Create `internal/quality/quality_test.go`:

```go
package quality

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0x524A/rtspeek/pkg/rtspeek"
)

// fake is the Analyze seam. A call blocks until release is closed or its
// context ends, so a test decides exactly when a run finishes.
type fake struct {
	release chan struct{}
	res     *rtspeek.Analysis
	err     error
	live    atomic.Int32
	urls    chan string
	opts    chan rtspeek.AnalyzeOptions
}

func newFake(res *rtspeek.Analysis, err error) *fake {
	return &fake{
		release: make(chan struct{}), res: res, err: err,
		urls: make(chan string, 64), opts: make(chan rtspeek.AnalyzeOptions, 64),
	}
}

func (f *fake) analyze(ctx context.Context, url string, o rtspeek.AnalyzeOptions) (*rtspeek.Analysis, error) {
	f.live.Add(1)
	defer f.live.Add(-1)
	f.urls <- url
	f.opts <- o
	select {
	case <-f.release:
		return f.res, f.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func analysis(severities ...string) *rtspeek.Analysis {
	a := &rtspeek.Analysis{}
	for _, s := range severities {
		a.Findings = append(a.Findings, rtspeek.Finding{Severity: s, Code: "x_" + s, Message: s})
	}
	return a
}

func waitState(t *testing.T, r *Runner, cam string, want State) Run {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if run, ok := r.Get(cam); ok && run.State == want {
			return run
		}
		if time.Now().After(deadline) {
			run, ok := r.Get(cam)
			t.Fatalf("camera %q never reached %q (have %+v, ok=%v)", cam, want, run, ok)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func newRunner(t *testing.T, f *fake, max int) *Runner {
	t.Helper()
	r := New(Options{MaxConcurrent: max, Analyze: f.analyze})
	t.Cleanup(r.Close)
	return r
}

func TestStartAppliesDefaultsAndPassesTheURL(t *testing.T) {
	f := newFake(analysis(), nil)
	r := newRunner(t, f, 0)

	run, err := r.Start("cam", "rtsp://127.0.0.1:1/cam", Request{})
	if err != nil {
		t.Fatal(err)
	}
	if run.State != StateRunning || run.Camera != "cam" || run.StartedAt.IsZero() || run.FinishedAt != nil {
		t.Errorf("run = %+v", run)
	}
	if run.Request != (Request{DurationSeconds: 5, Transport: TransportAuto}) {
		t.Errorf("request after defaults = %+v", run.Request)
	}
	if u := <-f.urls; u != "rtsp://127.0.0.1:1/cam" {
		t.Errorf("analysed %q", u)
	}
	if o := <-f.opts; o.Duration != 5*time.Second || o.Transport != rtspeek.TransportAuto {
		t.Errorf("options = %+v", o)
	}

	close(f.release)
	done := waitState(t, r, "cam", StateDone)
	if done.Verdict != VerdictOK || done.FinishedAt == nil || done.Analysis == nil || done.Error != "" {
		t.Errorf("done = %+v", done)
	}
}

func TestVerdictIsTheWorstSeverity(t *testing.T) {
	for name, tc := range map[string]struct {
		in   []string
		want string
	}{
		"no findings":       {nil, VerdictOK},
		"info only":         {[]string{rtspeek.SeverityInfo}, VerdictOK},
		"warning":           {[]string{rtspeek.SeverityInfo, rtspeek.SeverityWarn}, VerdictWarn},
		"error beats warn":  {[]string{rtspeek.SeverityWarn, rtspeek.SeverityError, rtspeek.SeverityInfo}, VerdictError},
		"unknown severity":  {[]string{"mystery"}, VerdictOK},
	} {
		if got := verdict(analysis(tc.in...)); got != tc.want {
			t.Errorf("%s: verdict = %q, want %q", name, got, tc.want)
		}
	}
}

func TestFailedRuns(t *testing.T) {
	for name, tc := range map[string]struct {
		res     *rtspeek.Analysis
		err     error
		wantErr string
		keepAn  bool
	}{
		"analyser error":       {nil, errors.New("dial refused"), "dial refused", false},
		"analysis carries err": {&rtspeek.Analysis{Error: "setup failed"}, nil, "setup failed", true},
		"no analysis":          {nil, nil, "no analysis", false},
	} {
		f := newFake(tc.res, tc.err)
		close(f.release)
		r := newRunner(t, f, 0)
		if _, err := r.Start("cam", "rtsp://x/cam", Request{}); err != nil {
			t.Fatal(err)
		}
		run := waitState(t, r, "cam", StateFailed)
		if run.Error == "" || !strings.Contains(run.Error, tc.wantErr) || run.Verdict != "" || run.FinishedAt == nil {
			t.Errorf("%s: run = %+v", name, run)
		}
		if (run.Analysis != nil) != tc.keepAn {
			t.Errorf("%s: analysis kept = %v, want %v", name, run.Analysis != nil, tc.keepAn)
		}
	}
}

func TestStartValidatesTheRequest(t *testing.T) {
	f := newFake(analysis(), nil)
	close(f.release)
	r := newRunner(t, f, 0)
	for name, req := range map[string]Request{
		"too short":       {DurationSeconds: 0.5},
		"too long":        {DurationSeconds: 31},
		"negative":        {DurationSeconds: -1},
		"not a number":    {DurationSeconds: math.NaN()},
		"infinite":        {DurationSeconds: math.Inf(1)},
		"unknown network": {Transport: "sctp"},
	} {
		if _, err := r.Start("cam", "rtsp://x", req); !errors.Is(err, ErrBadRequest) {
			t.Errorf("%s: err = %v, want ErrBadRequest", name, err)
		}
	}
	if _, ok := r.Get("cam"); ok || f.live.Load() != 0 || len(f.urls) != 0 {
		t.Error("a rejected request must not create a run or call the analyser")
	}
	for name, req := range map[string]Request{
		"lower bound": {DurationSeconds: 1, Transport: TransportUDP},
		"upper bound": {DurationSeconds: 30, Transport: TransportTCP},
	} {
		if _, err := r.Start(name, "rtsp://x", req); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestOnlyOneRunPerCamera(t *testing.T) {
	f := newFake(analysis(), nil)
	r := newRunner(t, f, 0)
	if _, err := r.Start("a", "rtsp://x/a", Request{}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Start("a", "rtsp://x/a", Request{}); !errors.Is(err, ErrRunning) {
		t.Errorf("second start on a = %v, want ErrRunning", err)
	}
	if _, err := r.Start("b", "rtsp://x/b", Request{}); err != nil {
		t.Errorf("start on another camera = %v", err)
	}
	close(f.release)
	waitState(t, r, "a", StateDone)
	if _, err := r.Start("a", "rtsp://x/a", Request{}); err != nil {
		t.Errorf("start after the run finished = %v", err)
	}
}

func TestGlobalCapAndSlotRelease(t *testing.T) {
	f := newFake(analysis(), nil)
	r := newRunner(t, f, 0) // 0 means the default of 2
	for _, id := range []string{"a", "b"} {
		if _, err := r.Start(id, "rtsp://x/"+id, Request{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.Start("c", "rtsp://x/c", Request{}); !errors.Is(err, ErrBusy) {
		t.Fatalf("third start = %v, want ErrBusy", err)
	}
	close(f.release)
	waitState(t, r, "a", StateDone)
	waitState(t, r, "b", StateDone)
	if _, err := r.Start("c", "rtsp://x/c", Request{}); err != nil {
		t.Errorf("start after slots freed = %v", err)
	}
}

func TestSlotIsReleasedWhenTheAnalyserFails(t *testing.T) {
	f := newFake(nil, errors.New("boom"))
	close(f.release)
	r := newRunner(t, f, 1)
	if _, err := r.Start("a", "rtsp://x/a", Request{}); err != nil {
		t.Fatal(err)
	}
	waitState(t, r, "a", StateFailed)
	if _, err := r.Start("b", "rtsp://x/b", Request{}); err != nil {
		t.Errorf("a failed run kept its slot: %v", err)
	}
}

func TestGetUnknownCamera(t *testing.T) {
	r := newRunner(t, newFake(nil, nil), 0)
	if _, ok := r.Get("nope"); ok {
		t.Error("Get on an unknown camera must report false")
	}
}

func TestANewRunReplacesThePreviousResult(t *testing.T) {
	f := newFake(analysis(rtspeek.SeverityWarn), nil)
	close(f.release)
	r := newRunner(t, f, 0)
	_, _ = r.Start("a", "rtsp://x/a", Request{})
	waitState(t, r, "a", StateDone)

	f2 := newFake(analysis(), nil) // a second run that stays blocked
	r.analyze = f2.analyze
	run, err := r.Start("a", "rtsp://x/a", Request{})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := r.Get("a")
	if got.State != StateRunning || got.Analysis != nil || got.FinishedAt != nil || got.Verdict != "" || run.State != StateRunning {
		t.Errorf("after restart: %+v", got)
	}
}

func recv(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("event channel closed")
		}
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("no event")
	}
	return Event{}
}

func TestEventsArriveInOrder(t *testing.T) {
	f := newFake(analysis(rtspeek.SeverityWarn), nil)
	r := newRunner(t, f, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := r.Subscribe(ctx)

	_, _ = r.Start("a", "rtsp://x/a", Request{})
	if ev := recv(t, ch); ev != (Event{Camera: "a", State: StateRunning}) {
		t.Errorf("first event = %+v", ev)
	}
	close(f.release)
	if ev := recv(t, ch); ev != (Event{Camera: "a", State: StateDone, Verdict: VerdictWarn}) {
		t.Errorf("second event = %+v", ev)
	}
}

func TestASlowSubscriberIsDroppedAndTheRunnerKeepsWorking(t *testing.T) {
	f := newFake(analysis(), nil)
	close(f.release)
	r := newRunner(t, f, 0)
	ch := r.Subscribe(context.Background()) // never read until the end

	for i := 0; i < eventBuffer; i++ { // two events per run: far more than the buffer
		id := string(rune('a' + i))
		if _, err := r.Start(id, "rtsp://x/"+id, Request{}); err != nil {
			t.Fatal(err)
		}
		waitState(t, r, id, StateDone)
	}
	n := 0
	deadline := time.After(5 * time.Second)
	for open := true; open; {
		select {
		case _, ok := <-ch:
			open = ok
			if ok {
				n++
			}
		case <-deadline:
			t.Fatal("the slow subscriber's channel was never closed")
		}
	}
	if n > eventBuffer {
		t.Errorf("a dropped subscriber received %d events, buffer is %d", n, eventBuffer)
	}
	if _, err := r.Start("z", "rtsp://x/z", Request{}); err != nil {
		t.Errorf("the runner stopped working after dropping a subscriber: %v", err)
	}
}

func TestSubscribeEndsWithItsContext(t *testing.T) {
	r := newRunner(t, newFake(nil, nil), 0)
	ctx, cancel := context.WithCancel(context.Background())
	ch := r.Subscribe(ctx)
	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("got an event instead of a close")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("channel not closed after the context ended")
	}
}

func TestCloseCancelsEveryRunAndWaits(t *testing.T) {
	f := newFake(analysis(), nil) // never released
	r := New(Options{Analyze: f.analyze})
	ch := r.Subscribe(context.Background())
	_, _ = r.Start("a", "rtsp://x/a", Request{})
	_, _ = r.Start("b", "rtsp://x/b", Request{})
	for len(f.urls) < 2 {
		time.Sleep(time.Millisecond)
	}

	r.Close()
	if n := f.live.Load(); n != 0 {
		t.Fatalf("%d analyses still running after Close", n)
	}
	if run, _ := r.Get("a"); run.State != StateFailed || run.Error != "cancelled" {
		t.Errorf("run after Close = %+v", run)
	}
	if _, err := r.Start("c", "rtsp://x/c", Request{}); !errors.Is(err, ErrClosed) {
		t.Errorf("Start after Close = %v, want ErrClosed", err)
	}
	for range ch { // must terminate: Close ends every subscription
	}
	r.Close() // idempotent
}
```

Note: the test file uses `r.analyze` and `eventBuffer`, so the implementation must name its fields and constant exactly that.

- [ ] **Step 3: Run the tests to see them fail**

Run: `go test ./internal/quality/ 2>&1 | head`
Expected: compile failure (package has no non-test files / undefined symbols).

- [ ] **Step 4: Write the implementation**

Create `internal/quality/quality.go`:

```go
// Package quality runs rtspeek stream analyses against a fleet's own cameras
// and keeps the latest result per camera in memory.
//
// A run is a measurement of what a client receives, not a change to the fleet:
// nothing here publishes to the fleet's event feed, so a run cannot alter a
// fleet's journal or replay hash. The measured numbers (jitter, fps, bitrate)
// depend on timing and machine load and are not reproducible from a seed.
package quality

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/0x524A/rtspeek/pkg/rtspeek"
)

// Transport selects how the analysis connects to the camera.
type Transport string

// The transports rtspeek supports.
const (
	TransportAuto Transport = "auto" // UDP, then TCP if no packet arrives
	TransportUDP  Transport = "udp"
	TransportTCP  Transport = "tcp"
)

// Request is what a caller may choose. Thresholds stay rtspeek's defaults so
// results are comparable between runs.
type Request struct {
	DurationSeconds float64   `json:"durationSeconds,omitempty"` // 0 means DefaultDuration
	Transport       Transport `json:"transport,omitempty"`       // "" means auto
}

// State is where a run is in its life.
type State string

// The run states.
const (
	StateRunning State = "running"
	StateDone    State = "done"
	StateFailed  State = "failed"
)

// The verdicts: the worst finding severity of a finished run.
const (
	VerdictOK    = "ok"
	VerdictWarn  = "warn"
	VerdictError = "error"
)

// Bounds on a request, in seconds.
const (
	DefaultDuration = 5.0
	MinDuration     = 1.0
	MaxDuration     = 30.0
)

const (
	defaultMaxConcurrent = 2
	eventBuffer          = 16
	// connectTimeout bounds dial, OPTIONS and DESCRIBE.
	connectTimeout = 10 * time.Second
	// slack is added to the measurement window to bound a whole run: the
	// connection budget (connectTimeout), rtspeek's 5s first-packet wait, the 3s
	// auto-mode UDP fallback wait, and some margin.
	slack = 20 * time.Second
)

// Errors returned by Start.
var (
	ErrBadRequest = errors.New("invalid stream quality request")
	ErrRunning    = errors.New("a stream quality test is already running on this camera")
	ErrBusy       = errors.New("too many stream quality tests are running")
	ErrClosed     = errors.New("the stream quality runner is closed")
)

// Run is one test and its result.
type Run struct {
	Camera     string            `json:"camera"`
	State      State             `json:"state"`
	Request    Request           `json:"request"` // after defaults
	StartedAt  time.Time         `json:"startedAt"`
	FinishedAt *time.Time        `json:"finishedAt,omitempty"`
	Verdict    string            `json:"verdict,omitempty"` // set when done
	Analysis   *rtspeek.Analysis `json:"analysis,omitempty"`
	Error      string            `json:"error,omitempty"` // set when failed
}

// Event announces a change of state. Verdict is set on completion.
type Event struct {
	Camera  string `json:"camera"`
	State   State  `json:"state"`
	Verdict string `json:"verdict,omitempty"`
}

// Options configures a Runner.
type Options struct {
	// MaxConcurrent caps runs in flight across cameras; zero means 2.
	MaxConcurrent int
	// Analyze is the rtspeek seam; nil means the real call. Tests only.
	Analyze func(ctx context.Context, url string, o rtspeek.AnalyzeOptions) (*rtspeek.Analysis, error)
}

// Runner runs and remembers stream quality tests.
type Runner struct {
	analyze func(ctx context.Context, url string, o rtspeek.AnalyzeOptions) (*rtspeek.Analysis, error)
	max     int

	mu     sync.Mutex
	runs   map[string]*entry
	active int // analyses in flight, including ones whose entry was forgotten
	subs   map[*sub]struct{}
	closed bool
	wg     sync.WaitGroup
}

// entry is the runner's record of one run. Only the goroutine that owns it,
// and only while it is still the camera's current entry, writes to run.
type entry struct {
	run    Run
	cancel context.CancelFunc
	done   chan struct{} // closed when the analysis goroutine has exited
}

type sub struct {
	ch   chan Event
	gone chan struct{} // closed when the subscription is dropped
}

// New returns a Runner.
func New(opt Options) *Runner {
	r := &Runner{
		analyze: opt.Analyze,
		max:     opt.MaxConcurrent,
		runs:    map[string]*entry{},
		subs:    map[*sub]struct{}{},
	}
	if r.analyze == nil {
		r.analyze = realAnalyze
	}
	if r.max <= 0 {
		r.max = defaultMaxConcurrent
	}
	return r
}

// realAnalyze runs rtspeek against url.
func realAnalyze(ctx context.Context, url string, o rtspeek.AnalyzeOptions) (*rtspeek.Analysis, error) {
	info, err := rtspeek.DescribeStreamWithOptions(ctx, url, rtspeek.Options{Timeout: connectTimeout, Analyze: &o})
	var a *rtspeek.Analysis
	if info != nil {
		a = info.GetAnalysis()
	}
	return a, err
}

func normalize(req Request) (Request, error) {
	if req.DurationSeconds == 0 {
		req.DurationSeconds = DefaultDuration
	}
	// Written so a NaN fails the range check.
	if !(req.DurationSeconds >= MinDuration && req.DurationSeconds <= MaxDuration) {
		return req, fmt.Errorf("%w: durationSeconds must be between %g and %g", ErrBadRequest, MinDuration, MaxDuration)
	}
	switch req.Transport {
	case "":
		req.Transport = TransportAuto
	case TransportAuto, TransportUDP, TransportTCP:
	default:
		return req, fmt.Errorf("%w: transport must be auto, udp or tcp", ErrBadRequest)
	}
	return req, nil
}

// verdict is the worst finding severity; info findings alone are ok.
func verdict(a *rtspeek.Analysis) string {
	v := VerdictOK
	for _, f := range a.Findings {
		switch f.Severity {
		case rtspeek.SeverityError:
			return VerdictError
		case rtspeek.SeverityWarn:
			v = VerdictWarn
		}
	}
	return v
}

// Start validates req and begins a test of rtspURL for camera, returning the
// running Run at once. The analysis runs on its own context, not the caller's.
func (r *Runner) Start(camera, rtspURL string, req Request) (Run, error) {
	req, err := normalize(req)
	if err != nil {
		return Run{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return Run{}, ErrClosed
	}
	if e := r.runs[camera]; e != nil && e.run.State == StateRunning {
		return Run{}, ErrRunning
	}
	if r.active >= r.max {
		return Run{}, ErrBusy
	}
	ctx, cancel := context.WithCancel(context.Background())
	e := &entry{
		run:    Run{Camera: camera, State: StateRunning, Request: req, StartedAt: time.Now()},
		cancel: cancel,
		done:   make(chan struct{}),
	}
	r.runs[camera] = e
	r.active++
	r.wg.Add(1)
	r.publishLocked(Event{Camera: camera, State: StateRunning})
	go r.execute(ctx, e, rtspURL, req)
	return e.run, nil
}

// Get returns the latest run for camera.
func (r *Runner) Get(camera string) (Run, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.runs[camera]
	if e == nil {
		return Run{}, false
	}
	return e.run, true
}

func (r *Runner) execute(ctx context.Context, e *entry, url string, req Request) {
	defer r.wg.Done()
	defer close(e.done)
	d := time.Duration(req.DurationSeconds * float64(time.Second))
	actx, acancel := context.WithTimeout(ctx, d+slack)
	defer acancel()
	a, err := r.analyze(actx, url, rtspeek.AnalyzeOptions{Duration: d, Transport: rtspeek.AnalyzeTransport(req.Transport)})
	cancelled := ctx.Err() != nil
	timedOut := errors.Is(actx.Err(), context.DeadlineExceeded)

	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.active--
	if r.runs[e.run.Camera] != e {
		return // forgotten, or replaced by a newer run: leave the record alone
	}
	e.run.FinishedAt = &now
	switch {
	case cancelled:
		e.run.State, e.run.Error = StateFailed, "cancelled"
	case timedOut:
		e.run.State, e.run.Error = StateFailed, "timed out"
	case err != nil:
		e.run.State, e.run.Error, e.run.Analysis = StateFailed, err.Error(), a
	case a == nil:
		e.run.State, e.run.Error = StateFailed, "no analysis was produced"
	case a.Error != "":
		e.run.State, e.run.Error, e.run.Analysis = StateFailed, a.Error, a
	default:
		e.run.State, e.run.Verdict, e.run.Analysis = StateDone, verdict(a), a
	}
	r.publishLocked(Event{Camera: e.run.Camera, State: e.run.State, Verdict: e.run.Verdict})
}

// Subscribe returns a channel of events that ends when ctx does, when the
// subscriber falls behind, or when the runner closes. A dropped subscriber
// re-lists on reconnect; nothing is replayed.
func (r *Runner) Subscribe(ctx context.Context) <-chan Event {
	s := &sub{ch: make(chan Event, eventBuffer), gone: make(chan struct{})}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		close(s.ch)
		return s.ch
	}
	r.subs[s] = struct{}{}
	r.wg.Add(1)
	r.mu.Unlock()
	go func() {
		defer r.wg.Done()
		select {
		case <-ctx.Done():
			r.mu.Lock()
			r.dropLocked(s)
			r.mu.Unlock()
		case <-s.gone:
		}
	}()
	return s.ch
}

// publishLocked never blocks: a full subscriber is dropped.
func (r *Runner) publishLocked(ev Event) {
	for s := range r.subs {
		select {
		case s.ch <- ev:
		default:
			r.dropLocked(s)
		}
	}
}

func (r *Runner) dropLocked(s *sub) {
	if _, ok := r.subs[s]; !ok {
		return
	}
	delete(r.subs, s)
	close(s.ch)
	close(s.gone)
}

// Close cancels every run, ends every subscription and waits for the
// goroutines to stop. It is safe to call twice.
func (r *Runner) Close() {
	r.mu.Lock()
	if !r.closed {
		r.closed = true
		for _, e := range r.runs {
			if e.run.State == StateRunning {
				e.cancel()
			}
		}
		for s := range r.subs {
			r.dropLocked(s)
		}
	}
	r.mu.Unlock()
	r.wg.Wait()
}
```

Note for the implementer: a run forgotten by Task 2's `Forget` is cancelled there; `Close` only needs to cancel entries still in the map.

- [ ] **Step 5: Tidy and run the tests**

Run:
```
go mod tidy
go test -race -count=1 ./internal/quality/
go vet ./...
CGO_ENABLED=0 go build ./...
```
Expected: PASS, vet clean, build clean. `git diff go.mod` shows rtspeek v1.0.0 added and `mediacommon/v2` at v2.9.5. If `TestASlowSubscriberIsDropped...` is flaky on timing, the cause is a real bug (publish must never block): fix the code, not the test.

- [ ] **Step 6: Commit**

```
git add go.mod go.sum internal/quality
git commit -m "quality: add the stream quality runner on rtspeek v1.0.0"
```

---

### Task 2: Cancel and Forget

**Files:**
- Create: `internal/quality/lifecycle.go`
- Test: `internal/quality/lifecycle_test.go`

**Interfaces:**
- Consumes: `Runner`, `entry`, `r.runs`, `r.mu` from Task 1.
- Produces: `func (r *Runner) Cancel(camera string) bool` (true if a running test was cancelled; waits for the analysis to stop; the run then reads `failed` with Error `"cancelled"` and an event is published), `func (r *Runner) Forget(camera string)` (cancels a running test, waits for it to stop, drops the record; publishes no event).

- [ ] **Step 1: Write the failing tests**

Create `internal/quality/lifecycle_test.go`:

```go
package quality

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/0x524A/rtspeek/pkg/rtspeek"
)

func TestCancelStopsARunningTest(t *testing.T) {
	f := newFake(analysis(), nil) // never released
	r := newRunner(t, f, 1)
	ch := r.Subscribe(context.Background())
	_, _ = r.Start("a", "rtsp://x/a", Request{})
	<-f.urls
	recv(t, ch) // running

	if !r.Cancel("a") {
		t.Fatal("Cancel on a running test = false")
	}
	if f.live.Load() != 0 {
		t.Error("the analysis was still running when Cancel returned")
	}
	run, _ := r.Get("a")
	if run.State != StateFailed || run.Error != "cancelled" || run.FinishedAt == nil || run.Analysis != nil {
		t.Errorf("run = %+v", run)
	}
	if ev := recv(t, ch); ev != (Event{Camera: "a", State: StateFailed}) {
		t.Errorf("event = %+v", ev)
	}
	if _, err := r.Start("b", "rtsp://x/b", Request{}); err != nil {
		t.Errorf("the cancelled run kept its slot: %v", err)
	}
}

func TestCancelWithNothingRunning(t *testing.T) {
	f := newFake(analysis(), nil)
	close(f.release)
	r := newRunner(t, f, 0)
	if r.Cancel("nope") {
		t.Error("Cancel on an unknown camera = true")
	}
	_, _ = r.Start("a", "rtsp://x/a", Request{})
	waitState(t, r, "a", StateDone)
	if r.Cancel("a") {
		t.Error("Cancel on a finished run = true")
	}
	if run, _ := r.Get("a"); run.State != StateDone {
		t.Errorf("Cancel changed a finished run: %+v", run)
	}
}

func TestForgetCancelsAndDropsTheResult(t *testing.T) {
	f := newFake(analysis(), nil) // never released
	r := newRunner(t, f, 0)
	_, _ = r.Start("a", "rtsp://x/a", Request{})
	<-f.urls

	r.Forget("a")
	if f.live.Load() != 0 {
		t.Error("the analysis was still running when Forget returned")
	}
	if _, ok := r.Get("a"); ok {
		t.Error("Forget left a result behind")
	}
	r.Forget("a")    // forgetting twice is fine
	r.Forget("nope") // and so is an unknown camera
}

func TestForgetDropsAFinishedResult(t *testing.T) {
	f := newFake(analysis(), nil)
	close(f.release)
	r := newRunner(t, f, 0)
	_, _ = r.Start("a", "rtsp://x/a", Request{})
	waitState(t, r, "a", StateDone)
	r.Forget("a")
	if _, ok := r.Get("a"); ok {
		t.Error("Forget left a finished result behind")
	}
}

// A run that was forgotten and replaced under the same id must never have its
// record overwritten by the old run finishing late.
func TestAForgottenRunCannotOverwriteANewerOne(t *testing.T) {
	var calls atomic.Int32
	second := make(chan struct{})
	analyze := func(ctx context.Context, _ string, _ rtspeek.AnalyzeOptions) (*rtspeek.Analysis, error) {
		if calls.Add(1) == 1 {
			<-ctx.Done() // the first run only ends when it is cancelled
			return nil, errors.New("late failure from the first run")
		}
		<-second
		return analysis(rtspeek.SeverityWarn), nil
	}
	r := New(Options{Analyze: analyze})
	t.Cleanup(r.Close)

	_, _ = r.Start("a", "rtsp://x/a", Request{})
	for calls.Load() < 1 {
	}
	r.Forget("a") // returns after the first run has fully stopped

	if _, err := r.Start("a", "rtsp://x/a", Request{}); err != nil {
		t.Fatal(err)
	}
	close(second)
	run := waitState(t, r, "a", StateDone)
	if run.Verdict != VerdictWarn || run.Error != "" {
		t.Errorf("the newer run was overwritten: %+v", run)
	}
}
```

- [ ] **Step 2: Run to see failure**

Run: `go test ./internal/quality/ 2>&1 | head -5`
Expected: compile failure, `r.Cancel`/`r.Forget` undefined.

- [ ] **Step 3: Implement**

Create `internal/quality/lifecycle.go`:

```go
package quality

// Cancel stops camera's running test and waits for its analysis to end. It
// reports whether a running test was found; the run is then recorded as
// failed with the error "cancelled". A test that finishes on its own in the
// instant between the check and the cancel keeps its own result.
func (r *Runner) Cancel(camera string) bool {
	r.mu.Lock()
	e := r.runs[camera]
	if e == nil || e.run.State != StateRunning {
		r.mu.Unlock()
		return false
	}
	r.mu.Unlock()
	e.cancel()
	<-e.done
	return true
}

// Forget cancels camera's running test, waits for it to stop, and drops the
// camera's result. It publishes no event: it is for a camera that no longer
// exists.
func (r *Runner) Forget(camera string) {
	r.mu.Lock()
	e := r.runs[camera]
	delete(r.runs, camera)
	r.mu.Unlock()
	if e == nil {
		return
	}
	e.cancel()
	<-e.done
}
```

- [ ] **Step 4: Run the tests**

Run: `go test -race -count=3 ./internal/quality/ && go vet ./internal/quality/`
Expected: PASS. Note the busy-wait `for calls.Load() < 1 {}` in the last test spins briefly; replace it with a `runtime.Gosched()` loop if lint objects.

- [ ] **Step 5: Commit**

```
git add internal/quality
git commit -m "quality: cancel and forget a camera's test"
```

---

### Task 3: The three endpoints, the camera summary, and forgetting a removed camera

**Files:**
- Create: `internal/dashboard/quality.go`
- Modify: `internal/dashboard/dashboard.go` (Options, server, routes, `cameraDTO`, `dto`, `removeCamera`)
- Test: `internal/dashboard/quality_test.go`

**Interfaces:**
- Consumes: `quality.Runner` (`Start`, `Get`, `Cancel`, `Forget`), `quality.Request`, `quality.Run`, the errors `ErrBadRequest`, `ErrRunning`, `ErrBusy`, `ErrClosed`; in the dashboard package `sameOrigin`, `decodeJSON`, `writeJSON`, `writeError`, `writeErr`, `server.fleet`.
- Produces: `Options.Quality *quality.Runner` (nil disables the feature: the three routes answer 404 "stream quality tests are not enabled", like `needLibrary`); camera DTO field `quality` (`{state, verdict?, finishedAt?}`, absent when the camera has no result).

- [ ] **Step 1: Write the failing tests**

Create `internal/dashboard/quality_test.go`:

```go
package dashboard

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0x524A/rtspeek/pkg/rtspeek"

	"github.com/0x524a/camfarm"
	"github.com/0x524a/camfarm/internal/quality"
)

// qfake is the analyser seam: a call blocks until release is closed or its
// context ends.
type qfake struct {
	release chan struct{}
	res     *rtspeek.Analysis
	live    atomic.Int32
	urls    chan string
	opts    chan rtspeek.AnalyzeOptions
}

func newQFake(res *rtspeek.Analysis) *qfake {
	return &qfake{release: make(chan struct{}), res: res, urls: make(chan string, 16), opts: make(chan rtspeek.AnalyzeOptions, 16)}
}

func (f *qfake) analyze(ctx context.Context, url string, o rtspeek.AnalyzeOptions) (*rtspeek.Analysis, error) {
	f.live.Add(1)
	defer f.live.Add(-1)
	f.urls <- url
	f.opts <- o
	select {
	case <-f.release:
		return f.res, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// qualityServer serves a two-camera fleet ("front-door", "back-door") with a
// runner whose analyser is f.
func qualityServer(t *testing.T, f *qfake, max int) (*httptest.Server, *camfarm.Fleet, *quality.Runner) {
	t.Helper()
	fleet := camfarm.StartT(t, camfarm.Spec{Seed: 1, Cameras: []camfarm.CameraSpec{{ID: "front-door"}, {ID: "back-door"}}})
	r := quality.New(quality.Options{MaxConcurrent: max, Analyze: f.analyze})
	t.Cleanup(r.Close)
	ts := httptest.NewServer(Handler(fleet, Options{Quality: r}))
	t.Cleanup(ts.Close)
	return ts, fleet, r
}

func decodeRun(t *testing.T, resp *http.Response) quality.Run {
	t.Helper()
	var run quality.Run
	if err := json.NewDecoder(resp.Body).Decode(&run); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	return run
}

func qURL(ts *httptest.Server, id string) string { return ts.URL + "/api/v1/cameras/" + id + "/quality" }

func waitRun(t *testing.T, ts *httptest.Server, id string, want quality.State) quality.Run {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp := do(t, "GET", qURL(ts, id), "")
		if resp.StatusCode == 200 {
			if run := decodeRun(t, resp); run.State == want {
				return run
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("camera %q never reached %q", id, want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestQualityRoutesAre404WhenDisabled(t *testing.T) {
	ts, _ := newServer(t, Options{})
	for _, m := range []string{"POST", "GET", "DELETE"} {
		if r := do(t, m, qURL(ts, "front-door"), ""); r.StatusCode != 404 {
			t.Errorf("%s = %d, want 404", m, r.StatusCode)
		}
	}
}

func TestStartQualityTestsTheCamerasOwnURL(t *testing.T) {
	f := newQFake(&rtspeek.Analysis{Findings: []rtspeek.Finding{{Severity: "info", Code: "x", Message: "m"}}})
	ts, fleet, _ := qualityServer(t, f, 0)
	cam, _ := fleet.Camera("front-door")

	resp := do(t, "POST", qURL(ts, "front-door"), "") // no body at all
	if resp.StatusCode != 202 {
		t.Fatalf("start = %d", resp.StatusCode)
	}
	run := decodeRun(t, resp)
	if run.State != quality.StateRunning || run.Camera != "front-door" ||
		run.Request != (quality.Request{DurationSeconds: 5, Transport: quality.TransportAuto}) {
		t.Errorf("run = %+v", run)
	}
	if u := <-f.urls; u != cam.RTSPURL() {
		t.Errorf("analysed %q, want the camera's own %q", u, cam.RTSPURL())
	}

	close(f.release)
	done := waitRun(t, ts, "front-door", quality.StateDone)
	if done.Verdict != "ok" || done.Analysis == nil || len(done.Analysis.Findings) != 1 {
		t.Errorf("done = %+v", done)
	}
}

func TestStartQualityPassesTheOptionsThrough(t *testing.T) {
	f := newQFake(&rtspeek.Analysis{})
	ts, _, _ := qualityServer(t, f, 0)
	resp := do(t, "POST", qURL(ts, "front-door"), `{"durationSeconds":2,"transport":"tcp"}`)
	if resp.StatusCode != 202 {
		t.Fatalf("start = %d", resp.StatusCode)
	}
	<-f.urls
	if o := <-f.opts; o.Duration != 2*time.Second || o.Transport != rtspeek.TransportTCP {
		t.Errorf("options = %+v", o)
	}
}

func TestStartQualityAcceptsNullAndChunkedBodies(t *testing.T) {
	f := newQFake(&rtspeek.Analysis{})
	close(f.release)
	ts, _, _ := qualityServer(t, f, 0)

	if r := do(t, "POST", qURL(ts, "front-door"), `null`); r.StatusCode != 202 {
		t.Errorf("null body = %d, want 202", r.StatusCode)
	}
	waitRun(t, ts, "front-door", quality.StateDone)

	for name, body := range map[string]string{"empty chunked": "", "json chunked": `{"durationSeconds":2}`} {
		req, _ := http.NewRequest("POST", qURL(ts, "back-door"), io.NopCloser(strings.NewReader(body))) // unknown length: chunked
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 202 {
			t.Errorf("%s = %d, want 202", name, resp.StatusCode)
		}
		waitRun(t, ts, "back-door", quality.StateDone)
	}
}

func TestQualityStatusCodes(t *testing.T) {
	f := newQFake(&rtspeek.Analysis{})
	ts, _, _ := qualityServer(t, f, 0)

	for _, m := range []string{"POST", "GET", "DELETE"} {
		if r := do(t, m, qURL(ts, "no-such-camera"), ""); r.StatusCode != 404 {
			t.Errorf("%s on an unknown camera = %d, want 404", m, r.StatusCode)
		}
	}
	if r := do(t, "GET", qURL(ts, "front-door"), ""); r.StatusCode != 404 {
		t.Errorf("GET before any run = %d, want 404", r.StatusCode)
	}
	if r := do(t, "DELETE", qURL(ts, "front-door"), ""); r.StatusCode != 404 {
		t.Errorf("DELETE with nothing running = %d, want 404", r.StatusCode)
	}

	for name, body := range map[string]string{
		"too short": `{"durationSeconds":0.5}`, "too long": `{"durationSeconds":99}`,
		"negative": `{"durationSeconds":-3}`, "bad transport": `{"transport":"sctp"}`,
	} {
		if r := do(t, "POST", qURL(ts, "front-door"), body); r.StatusCode != 422 {
			t.Errorf("%s = %d, want 422", name, r.StatusCode)
		}
	}
	if r := do(t, "POST", qURL(ts, "front-door"), `{"url":"rtsp://elsewhere/x"}`); r.StatusCode != 400 {
		t.Errorf("a url field = %d, want 400 (the URL is never taken from the request)", r.StatusCode)
	}
	req, _ := http.NewRequest("POST", qURL(ts, "front-door"), strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "text/plain")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 415 {
		t.Errorf("wrong content type = %d, want 415", resp.StatusCode)
	}
	if len(f.urls) != 0 {
		t.Error("a rejected request reached the analyser")
	}

	if r := do(t, "POST", qURL(ts, "front-door"), ""); r.StatusCode != 202 {
		t.Fatalf("start = %d", r.StatusCode)
	}
	if r := do(t, "POST", qURL(ts, "front-door"), ""); r.StatusCode != 409 {
		t.Errorf("second start = %d, want 409", r.StatusCode)
	}
}

func TestQualityCapIs429WithRetryAfter(t *testing.T) {
	f := newQFake(&rtspeek.Analysis{})
	ts, _, _ := qualityServer(t, f, 1)
	if r := do(t, "POST", qURL(ts, "front-door"), ""); r.StatusCode != 202 {
		t.Fatalf("first start = %d", r.StatusCode)
	}
	r := do(t, "POST", qURL(ts, "back-door"), "")
	if r.StatusCode != 429 || r.Header.Get("Retry-After") == "" {
		t.Errorf("start beyond the cap = %d, Retry-After %q", r.StatusCode, r.Header.Get("Retry-After"))
	}
}

func TestDeleteQualityCancelsARunningTest(t *testing.T) {
	f := newQFake(&rtspeek.Analysis{})
	ts, _, _ := qualityServer(t, f, 0)
	do(t, "POST", qURL(ts, "front-door"), "")
	<-f.urls
	if r := do(t, "DELETE", qURL(ts, "front-door"), ""); r.StatusCode != 204 {
		t.Fatalf("DELETE = %d, want 204", r.StatusCode)
	}
	run := decodeRun(t, do(t, "GET", qURL(ts, "front-door"), ""))
	if run.State != quality.StateFailed || run.Error != "cancelled" {
		t.Errorf("run = %+v", run)
	}
	if f.live.Load() != 0 {
		t.Error("the analysis was still running after DELETE returned")
	}
}

func TestQualityRefusesCrossSiteRequests(t *testing.T) {
	f := newQFake(&rtspeek.Analysis{})
	ts, _, _ := qualityServer(t, f, 0)
	for _, m := range []string{"POST", "DELETE"} {
		req, _ := http.NewRequest(m, qURL(ts, "front-door"), nil)
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 403 {
			t.Errorf("cross-site %s = %d, want 403", m, resp.StatusCode)
		}
	}
	if len(f.urls) != 0 {
		t.Error("a cross-site request started a test")
	}
}

func TestCameraDTOCarriesTheQualitySummary(t *testing.T) {
	f := newQFake(&rtspeek.Analysis{Findings: []rtspeek.Finding{{Severity: "warn", Code: "w", Message: "m"}}})
	ts, _, _ := qualityServer(t, f, 0)
	get := func() map[string]any {
		var m map[string]any
		_ = json.NewDecoder(do(t, "GET", ts.URL+"/api/v1/cameras/front-door", "").Body).Decode(&m)
		return m
	}
	if _, has := get()["quality"]; has {
		t.Error("a camera with no result must not carry a quality field")
	}
	do(t, "POST", qURL(ts, "front-door"), "")
	if q, _ := get()["quality"].(map[string]any); q["state"] != "running" {
		t.Errorf("running summary = %v", q)
	}
	close(f.release)
	waitRun(t, ts, "front-door", quality.StateDone)
	q, _ := get()["quality"].(map[string]any)
	if q["state"] != "done" || q["verdict"] != "warn" || q["finishedAt"] == nil {
		t.Errorf("done summary = %v", q)
	}
	var list []map[string]any
	_ = json.NewDecoder(do(t, "GET", ts.URL+"/api/v1/cameras", "").Body).Decode(&list)
	for _, c := range list {
		if _, has := c["quality"]; has != (c["id"] == "front-door") {
			t.Errorf("camera %v: quality present = %v", c["id"], has)
		}
	}
}

func TestRemovingACameraCancelsAndForgetsItsTest(t *testing.T) {
	f := newQFake(&rtspeek.Analysis{})
	ts, _, r := qualityServer(t, f, 0)
	do(t, "POST", qURL(ts, "front-door"), "")
	<-f.urls
	if resp := do(t, "DELETE", ts.URL+"/api/v1/cameras/front-door", ""); resp.StatusCode != 204 {
		t.Fatalf("remove camera = %d", resp.StatusCode)
	}
	if f.live.Load() != 0 {
		t.Error("the test kept running after its camera was removed")
	}
	if _, ok := r.Get("front-door"); ok {
		t.Error("the removed camera's result was kept")
	}
	// A failed removal must not forget anything.
	do(t, "POST", qURL(ts, "back-door"), "")
	<-f.urls
	if resp := do(t, "DELETE", ts.URL+"/api/v1/cameras/no-such-camera", ""); resp.StatusCode != 404 {
		t.Fatalf("remove unknown = %d", resp.StatusCode)
	}
	if run, ok := r.Get("back-door"); !ok || run.State != quality.StateRunning {
		t.Errorf("an unrelated removal disturbed back-door: %+v %v", run, ok)
	}
}
```

- [ ] **Step 2: Run to see failure**

Run: `go test ./internal/dashboard/ -run 'Quality' 2>&1 | head`
Expected: compile failure (`Options.Quality` undefined).

- [ ] **Step 3: Implement the handlers**

Create `internal/dashboard/quality.go`:

```go
package dashboard

import (
	"bufio"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/0x524a/camfarm/internal/quality"
)

// qualitySummary is the small form of a camera's latest stream quality run
// that rides on the camera DTO, so a card can show a badge without a second
// request.
type qualitySummary struct {
	State      quality.State `json:"state"`
	Verdict    string        `json:"verdict,omitempty"`
	FinishedAt *time.Time    `json:"finishedAt,omitempty"`
}

// needQuality replies 404 and returns false when stream quality tests are not
// enabled.
func (s *server) needQuality(w http.ResponseWriter) bool {
	if s.quality == nil {
		writeError(w, http.StatusNotFound, "stream quality tests are not enabled")
		return false
	}
	return true
}

func (s *server) qualityOf(id string) *qualitySummary {
	if s.quality == nil {
		return nil
	}
	run, ok := s.quality.Get(id)
	if !ok {
		return nil
	}
	return &qualitySummary{State: run.State, Verdict: run.Verdict, FinishedAt: run.FinishedAt}
}

// startQuality begins a test of the camera's own RTSP URL. The URL is never
// read from the request, so this endpoint cannot be aimed at another host.
func (s *server) startQuality(w http.ResponseWriter, r *http.Request) {
	if !s.needQuality(w) {
		return
	}
	id := r.PathValue("id")
	cam, err := s.fleet.Camera(id)
	if err != nil {
		writeErr(w, err)
		return
	}
	var req quality.Request
	if !decodeOptionalJSON(w, r, &req) {
		return
	}
	run, err := s.quality.Start(id, cam.RTSPURL(), req)
	if err != nil {
		switch {
		case errors.Is(err, quality.ErrBadRequest):
			writeError(w, http.StatusUnprocessableEntity, err.Error())
		case errors.Is(err, quality.ErrRunning):
			writeError(w, http.StatusConflict, err.Error())
		case errors.Is(err, quality.ErrBusy):
			w.Header().Set("Retry-After", "5")
			writeError(w, http.StatusTooManyRequests, err.Error())
		case errors.Is(err, quality.ErrClosed):
			writeError(w, http.StatusServiceUnavailable, err.Error())
		default:
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusAccepted, run)
}

func (s *server) getQuality(w http.ResponseWriter, r *http.Request) {
	if !s.needQuality(w) {
		return
	}
	id := r.PathValue("id")
	if _, err := s.fleet.Camera(id); err != nil {
		writeErr(w, err)
		return
	}
	run, ok := s.quality.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "no stream quality result for this camera")
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *server) cancelQuality(w http.ResponseWriter, r *http.Request) {
	if !s.needQuality(w) {
		return
	}
	id := r.PathValue("id")
	if _, err := s.fleet.Camera(id); err != nil {
		writeErr(w, err)
		return
	}
	if !s.quality.Cancel(id) {
		writeError(w, http.StatusNotFound, "no stream quality test is running on this camera")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// decodeOptionalJSON is decodeJSON for a body that may be absent: an empty
// body leaves v at its zero value and needs no Content-Type.
func decodeOptionalJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	br := bufio.NewReader(r.Body)
	if _, err := br.Peek(1); errors.Is(err, io.EOF) {
		return true
	}
	r.Body = struct {
		io.Reader
		io.Closer
	}{br, r.Body}
	return decodeJSON(w, r, v)
}
```

- [ ] **Step 4: Wire it into the server**

In `internal/dashboard/dashboard.go`:
- import `"github.com/0x524a/camfarm/internal/quality"`;
- add to `Options`: `// Quality, when non-nil, enables the stream quality endpoints. The caller closes it.` `Quality *quality.Runner`;
- add to `server`: `quality *quality.Runner`, and set `quality: opt.Quality` in `Handler`;
- register, next to the faults routes:
  ```go
  mux.HandleFunc("POST /api/v1/cameras/{id}/quality", sameOrigin(s.startQuality))
  mux.HandleFunc("GET /api/v1/cameras/{id}/quality", s.getQuality)
  mux.HandleFunc("DELETE /api/v1/cameras/{id}/quality", sameOrigin(s.cancelQuality))
  ```
- add to `cameraDTO`: `Quality *qualitySummary `json:"quality,omitempty"``, and in `dto`: `d.Quality = s.qualityOf(st.ID)`;
- in `removeCamera`, after a successful `RemoveCamera`: `if s.quality != nil { s.quality.Forget(id) }` (take `id := r.PathValue("id")` once). A failed removal must not forget anything.

- [ ] **Step 5: Run the tests**

Run: `go test -race -count=1 ./internal/dashboard/ && go vet ./...`
Expected: PASS. Existing dashboard tests must still pass unchanged.

- [ ] **Step 6: Commit**

```
git add internal/dashboard
git commit -m "dashboard: stream quality endpoints and camera summary"
```

---

### Task 4: The `quality` event on the SSE stream

**Files:**
- Modify: `internal/dashboard/events.go`
- Test: `internal/dashboard/quality_events_test.go`

**Interfaces:**
- Consumes: `server.quality`, `Runner.Subscribe`, `quality.Event`.
- Produces: `event: quality` frames on `GET /api/v1/events` with data `{"camera","state","verdict?"}`. The fleet's own events are unchanged. A stream whose quality subscription ends (client too slow, runner closed) ends, as for the fleet feed: the client reconnects and re-lists.

- [ ] **Step 1: Write the failing tests**

Create `internal/dashboard/quality_events_test.go`. Read the existing SSE tests in `internal/dashboard/dashboard_test.go` and `sse_deadline_test.go` first and reuse any frame-reading helper there; otherwise read frames with a `bufio.Scanner` as below.

```go
package dashboard

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/0x524A/rtspeek/pkg/rtspeek"

	"github.com/0x524a/camfarm/internal/quality"
)

// sseFrames connects to /api/v1/events and delivers (event, data) pairs.
func sseFrames(t *testing.T, base string) <-chan [2]string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, "GET", base+"/api/v1/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	out := make(chan [2]string, 16)
	go func() {
		defer close(out)
		sc := bufio.NewScanner(resp.Body)
		var ev string
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				ev = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				out <- [2]string{ev, strings.TrimPrefix(line, "data: ")}
			}
		}
	}()
	return out
}

func nextFrame(t *testing.T, ch <-chan [2]string, event string) string {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case fr, ok := <-ch:
			if !ok {
				t.Fatalf("stream ended before a %q event", event)
			}
			if fr[0] == event {
				return fr[1]
			}
		case <-deadline:
			t.Fatalf("no %q event", event)
		}
	}
}

func TestQualityEventsArriveOnTheEventStream(t *testing.T) {
	f := newQFake(&rtspeek.Analysis{Findings: []rtspeek.Finding{{Severity: "error", Code: "e", Message: "m"}}})
	ts, _, _ := qualityServer(t, f, 0)
	frames := sseFrames(t, ts.URL)
	time.Sleep(50 * time.Millisecond) // let the handler subscribe before the test starts

	do(t, "POST", qURL(ts, "front-door"), "")
	var ev quality.Event
	_ = json.Unmarshal([]byte(nextFrame(t, frames, "quality")), &ev)
	if ev != (quality.Event{Camera: "front-door", State: quality.StateRunning}) {
		t.Errorf("first event = %+v", ev)
	}
	close(f.release)
	_ = json.Unmarshal([]byte(nextFrame(t, frames, "quality")), &ev)
	if ev != (quality.Event{Camera: "front-door", State: quality.StateDone, Verdict: "error"}) {
		t.Errorf("second event = %+v", ev)
	}
}

// A quality run is a measurement, not a change to the fleet: it must not touch
// the journal, the spec hash, the replay line, or the fleet's own feed.
func TestAQualityRunDoesNotTouchTheFleetsReplayIdentity(t *testing.T) {
	f := newQFake(&rtspeek.Analysis{})
	close(f.release)
	ts, fleet, _ := qualityServer(t, f, 0)

	hash, replay, journal := fleet.SpecHash(), fleet.ReplayLine(), len(fleet.Journal())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	feed := fleet.Subscribe(ctx)

	do(t, "POST", qURL(ts, "front-door"), "")
	waitRun(t, ts, "front-door", quality.StateDone)
	do(t, "POST", qURL(ts, "back-door"), "")
	do(t, "DELETE", qURL(ts, "back-door"), "")

	if fleet.SpecHash() != hash || fleet.ReplayLine() != replay || len(fleet.Journal()) != journal {
		t.Errorf("a quality run changed the fleet's identity: %q %q %d -> %q %q %d",
			hash, replay, journal, fleet.SpecHash(), fleet.ReplayLine(), len(fleet.Journal()))
	}
	select {
	case ev := <-feed:
		t.Errorf("a quality run published to the fleet's feed: %+v", ev)
	default:
	}
}

func TestEventStreamWorksWithoutAQualityRunner(t *testing.T) {
	ts, _ := newServer(t, Options{})
	frames := sseFrames(t, ts.URL)
	time.Sleep(50 * time.Millisecond)
	if r := do(t, "POST", ts.URL+"/api/v1/cameras", `{"id":"late"}`); r.StatusCode != 201 {
		t.Fatalf("add camera = %d", r.StatusCode)
	}
	nextFrame(t, frames, "camera_added") // a fleet event still flows when quality is disabled
}
```

- [ ] **Step 2: Run to see failure**

Run: `go test ./internal/dashboard/ -run 'QualityEvents|ReplayIdentity|WithoutAQualityRunner' 2>&1 | head`
Expected: the first test fails (no `quality` event arrives).

- [ ] **Step 3: Implement**

In `internal/dashboard/events.go`, in `events`, after `ch := s.fleet.Subscribe(r.Context())` add:

```go
	// qch is nil (and so never ready) when stream quality tests are disabled.
	// Quality events come from the runner's own bus, not the fleet's feed: a
	// measurement is not a fleet change and must not reach the journal.
	var qch <-chan quality.Event
	if s.quality != nil {
		qch = s.quality.Subscribe(r.Context())
	}
```

and a second case in the `select` loop:

```go
		case qe, ok := <-qch:
			if !ok {
				return // dropped or closed: the client reconnects and re-lists
			}
			b, err := json.Marshal(qe)
			if err != nil {
				continue
			}
			if !send("event: quality\ndata: %s\n\n", b) {
				return
			}
```

Add the `quality` import. Update the doc comment of `events` to mention the merged quality events.

- [ ] **Step 4: Run the tests**

Run: `go test -race -count=3 ./internal/dashboard/ && go vet ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```
git add internal/dashboard
git commit -m "dashboard: announce stream quality results on the event stream"
```

---

### Task 5: Binary wiring, end-to-end tests with real rtspeek, README

**Files:**
- Modify: `cmd/camfarm/run.go`
- Modify: `README.md`
- Test: `internal/dashboard/quality_real_test.go`

**Interfaces:**
- Consumes: everything from Tasks 1 to 4; `camfarm.StartT`, `camfarm.Spec`, `camfarm.CameraSpec{ID, Faults}`, `camfarm.FaultSpec{Kind, Rate, MeanBurst, ...}`, `library.New`, `postFile`, `fileDTO`, `fixture.Bytes()` from the existing dashboard tests.
- Produces: the shipped binary serves the feature; no new exported API.

- [ ] **Step 1: Wire the runner into the binary**

In `cmd/camfarm/run.go`, next to where `dashboard.Handler(fleet, dashboard.Options{Library: lib})` is built (around line 198), create the runner after `defer fleet.Close()` so that its deferred `Close` runs first on return:

```go
	qr := quality.New(quality.Options{})
	defer qr.Close()
```

and pass `Quality: qr` in the `dashboard.Options`. Import `github.com/0x524a/camfarm/internal/quality`. Do not add a flag: the feature is always on in the binary, like the faults API.

Run: `go build ./... && go vet ./cmd/...`

- [ ] **Step 2: Write the end-to-end tests**

Create `internal/dashboard/quality_real_test.go`. These use the real `rtspeek` over real loopback sockets (no build tag; they run in the normal suite). Read `rtspeek`'s `pkg/rtspeek/analysis.go` (`Analysis`, `TrackAnalysis`, `FrameStats`, the `Finding*` constants) while writing the assertions. Assertions are coarse: a finding is present or absent, a count is positive. Never assert an exact figure.

```go
package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/0x524A/rtspeek/pkg/rtspeek"

	"github.com/0x524a/camfarm"
	"github.com/0x524a/camfarm/internal/library"
	"github.com/0x524a/camfarm/internal/media/fixture"
	"github.com/0x524a/camfarm/internal/quality"
)

func realServer(t *testing.T, spec camfarm.Spec, lib *library.Library) (*httptest.Server, *camfarm.Fleet) {
	t.Helper()
	fleet := camfarm.StartT(t, spec)
	r := quality.New(quality.Options{}) // the real rtspeek
	t.Cleanup(r.Close)
	ts := httptest.NewServer(Handler(fleet, Options{Quality: r, Library: lib}))
	t.Cleanup(ts.Close)
	return ts, fleet
}

// runReal starts a test and waits for it to leave the running state.
func runReal(t *testing.T, ts *httptest.Server, id, body string) quality.Run {
	t.Helper()
	if r := do(t, "POST", qURL(ts, id), body); r.StatusCode != 202 {
		t.Fatalf("start = %d", r.StatusCode)
	}
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		run := decodeRun(t, do(t, "GET", qURL(ts, id), ""))
		if run.State != quality.StateRunning {
			return run
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("the test on %q never finished", id)
	return quality.Run{}
}

func hasFinding(a *rtspeek.Analysis, code string) bool {
	for _, f := range a.Findings {
		if f.Code == code {
			return true
		}
	}
	return false
}

func TestRealQualityOnACleanCamera(t *testing.T) {
	ts, fleet := realServer(t, camfarm.Spec{Seed: 1, Cameras: []camfarm.CameraSpec{{ID: "clean"}}}, nil)
	hash, replay, journal := fleet.SpecHash(), fleet.ReplayLine(), len(fleet.Journal())

	run := runReal(t, ts, "clean", `{"durationSeconds":2,"transport":"tcp"}`)
	if run.State != quality.StateDone || run.Analysis == nil || len(run.Analysis.Tracks) == 0 {
		t.Fatalf("run = %+v", run)
	}
	frames := uint64(0)
	for _, tr := range run.Analysis.Tracks {
		if tr.Frames != nil {
			frames += tr.Frames.Frames
		}
	}
	if frames == 0 {
		t.Error("no frames were counted")
	}
	for _, f := range run.Analysis.Findings {
		if f.Severity == rtspeek.SeverityError {
			t.Errorf("a clean camera produced an error finding: %+v", f)
		}
	}
	if fleet.SpecHash() != hash || fleet.ReplayLine() != replay || len(fleet.Journal()) != journal {
		t.Error("a real quality run changed the fleet's replay identity")
	}
}

func TestRealQualitySeesASeededPacketLoss(t *testing.T) {
	spec := camfarm.Spec{Seed: 7, Cameras: []camfarm.CameraSpec{{
		ID: "lossy", Faults: []camfarm.FaultSpec{{Kind: "packet_loss", Rate: 0.3, MeanBurst: 3}},
	}}}
	ts, _ := realServer(t, spec, nil)
	// Transport: try "tcp" first. If the finding does not appear, the loss is
	// not visible over TCP: use "udp" and say why in a comment here. Whichever
	// is used, assert only that the packet_loss finding is present.
	run := runReal(t, ts, "lossy", `{"durationSeconds":3,"transport":"tcp"}`)
	if run.State != quality.StateDone || run.Analysis == nil {
		t.Fatalf("run = %+v", run)
	}
	if !hasFinding(run.Analysis, rtspeek.FindingPacketLoss) {
		t.Errorf("no packet_loss finding for a camera dropping 30%% of packets: %+v", run.Analysis.Findings)
	}
}

// A fault that kills the session is a finding or a failed run, never a 5xx.
func TestRealQualityOnASessionKillingFault(t *testing.T) {
	// Look the teardown parameters up in GET /api/v1/faults (the catalogue) if
	// afterFrames below is rejected.
	spec := camfarm.Spec{Seed: 3, Cameras: []camfarm.CameraSpec{{
		ID: "dropper", Faults: []camfarm.FaultSpec{{Kind: "teardown", AfterFrames: 5}},
	}}}
	ts, _ := realServer(t, spec, nil)
	run := runReal(t, ts, "dropper", `{"durationSeconds":3,"transport":"tcp"}`)
	if run.State != quality.StateDone && run.State != quality.StateFailed {
		t.Fatalf("state = %q", run.State)
	}
	if run.State == quality.StateFailed && run.Error == "" {
		t.Error("a failed run must say why")
	}
}

func TestRealQualityCancelStopsPromptly(t *testing.T) {
	ts, _ := realServer(t, camfarm.Spec{Seed: 1, Cameras: []camfarm.CameraSpec{{ID: "slow"}}}, nil)
	if r := do(t, "POST", qURL(ts, "slow"), `{"durationSeconds":30,"transport":"tcp"}`); r.StatusCode != 202 {
		t.Fatalf("start = %d", r.StatusCode)
	}
	time.Sleep(300 * time.Millisecond) // let it connect
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "DELETE", qURL(ts, "slow"), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE did not return within 5s (does rtspeek honour context cancellation?): %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("DELETE = %d, want 204", resp.StatusCode)
	}
	if run := decodeRun(t, do(t, "GET", qURL(ts, "slow"), "")); run.State != quality.StateFailed || run.Error != "cancelled" {
		t.Errorf("run = %+v", run)
	}
}

// The whole flow for a user's own file: upload, make a camera from it, test it.
func TestRealQualityOnALibraryFileCamera(t *testing.T) {
	lib, err := library.New(library.Options{Roots: []string{t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	ts, _ := realServer(t, camfarm.Spec{Seed: 1, Cameras: []camfarm.CameraSpec{{ID: "seed-cam"}}}, lib)

	var f fileDTO
	resp := postFile(t, ts.URL, "clip.ts", fixture.Bytes())
	if resp.StatusCode != 201 {
		t.Fatalf("upload = %d", resp.StatusCode)
	}
	_ = json.NewDecoder(resp.Body).Decode(&f)
	if r := do(t, "POST", ts.URL+"/api/v1/cameras", `{"id":"from-file","libraryId":"`+f.ID+`"}`); r.StatusCode != 201 {
		t.Fatalf("add camera from the library = %d", r.StatusCode)
	}

	run := runReal(t, ts, "from-file", `{"durationSeconds":2,"transport":"tcp"}`)
	if run.State != quality.StateDone || run.Analysis == nil || len(run.Analysis.Tracks) == 0 {
		t.Errorf("run = %+v", run)
	}
}
```

If a field name used above is wrong (`camfarm.FaultSpec` has no `AfterFrames`, `FindingPacketLoss` is named differently, `TrackAnalysis.Frames` is not a pointer), fix the test to the real API; do not change what it asserts. If `rtspeek` does not stop within 5 s of a cancelled context, STOP and report BLOCKED with the evidence: the fix is then a design change (`Cancel` must not wait), not a test change.

- [ ] **Step 3: Run the tests**

Run: `go test -race -count=1 -run 'RealQuality' -v ./internal/dashboard/`
Expected: PASS. Then run each real test five times (`-count=5`) to look for flakiness; a flaky assertion means it is asserting a timing-dependent number, so loosen it to presence or absence, never add a retry or a sleep to hide it.

- [ ] **Step 4: Smoke the real binary**

Build and run the binary on loopback (check `go run ./cmd/camfarm -h` for the flags to add one camera and a dashboard address), then with `curl`: `POST /api/v1/cameras/<id>/quality` (expect 202), `GET` until `state` is `done` (expect `analysis.tracks`), and `GET /api/v1/cameras/<id>` (expect a `quality` object). Kill the process afterwards and confirm it exits promptly on SIGINT while a 30 s test is running (the runner's `Close` must cancel it). Put the commands and results in the report; commit nothing from this step.

- [ ] **Step 5: README**

Add a section "## Stream quality test" to `README.md` just before "## Use as a library" with: what it does (analyses a camera's own RTSP stream with rtspeek and reports packet loss, jitter, bitrate, frame rate, keyframes, timestamp jumps, broken frames and B-frame reordering as findings ranked info/warn/error); the three endpoints with a `curl` example for each; the options (`durationSeconds` 1 to 30, default 5; `transport` auto, udp or tcp); the verdict rule; that results live in memory only and the latest per camera is kept; that a test occupies one session on the camera, at most 2 run at once and one per camera; that `quality` events arrive on `/api/v1/events` and do not touch the journal or replay hash; and plainly that the measured numbers are not reproducible from a seed, only the presence of a strong seeded fault is. State that TCP hides network loss in transit (rtspeek's own caveat) and that `auto` is the default. Do not state any coverage figure. Match the README's existing tone and heading style.

- [ ] **Step 6: Whole-suite checks and commit**

Run:
```
gofmt -l .
go mod tidy && git diff --exit-code -- go.mod go.sum
go vet ./...
CGO_ENABLED=0 go build ./...
go test -race -count=1 ./...
```
Expected: no output from `gofmt -l`, no diff after tidy, everything else passes.

```
git add cmd/camfarm README.md internal/dashboard
git commit -m "quality: wire the runner into the binary; end-to-end tests with real rtspeek; README"
```
