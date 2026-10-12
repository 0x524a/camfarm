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
