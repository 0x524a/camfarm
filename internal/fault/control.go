package fault

import (
	"fmt"
	"math/rand/v2"
	"sync"

	"github.com/0x524a/camfarm/internal/seed"
)

// Method is an RTSP request method the control-plane faults can act on.
type Method string

// The methods a control-plane fault can match.
const (
	MethodDescribe Method = "describe"
	MethodSetup    Method = "setup"
	MethodPlay     Method = "play"
)

// ErrorDecision is an rtsp_error that fired: reply with Status.
type ErrorDecision struct{ Status int }

// LieDecision is an sdp_lie that fired: misdescribe the stream as Mode says.
type LieDecision struct{ Mode string }

// ControlDecision is the outcome for one RTSP request. Request is the 1-based
// index of the request among all requests since the engine was created.
type ControlDecision struct {
	Request int
	Error   *ErrorDecision
	Lie     *LieDecision
}

// Arming says a teardown fault has armed a freshly started session: close or
// reset it once AfterFrames frames have been delivered.
type Arming struct {
	AfterFrames int
	Mode        string
}

// controlKinds is the fixed order in which control-plane kinds are walked, so
// the draw sequence never depends on map iteration order.
var controlKinds = []Kind{KindRTSPError, KindSDPLie, KindTeardown}

type ctlEntry struct {
	spec Spec
	rnd  *rand.Rand
	// seen counts the requests this entry matched, per method, since it was
	// installed. OnlyRequest is measured against it, so a fault injected
	// mid-run counts from its own start.
	seen map[Method]int
}

// ControlEngine makes control-plane fault decisions for one camera. Unlike
// Engine it is safe for concurrent use: RTSP requests arrive on the server's
// goroutines while the dashboard swaps specs.
type ControlEngine struct {
	mu    sync.Mutex
	seed  seed.Seed
	specs map[Kind]*ctlEntry
	total int
}

// NewControlEngine returns an engine with no faults configured.
func NewControlEngine(s seed.Seed) *ControlEngine {
	return &ControlEngine{seed: s, specs: map[Kind]*ctlEntry{}}
}

// Set replaces the control-plane faults. Specs of other kinds are ignored. On
// any error nothing changes. Each installed kind gets a fresh stream seeded by
// kind name, exactly as New seeds it, and fresh counters, so a newly injected
// fault always starts the same sequence. The request total is not reset.
func (e *ControlEngine) Set(specs []Spec) error {
	next := map[Kind]*ctlEntry{}
	for _, sp := range specs {
		if !isControlKind(sp.Kind) {
			continue
		}
		if _, dup := next[sp.Kind]; dup {
			return fmt.Errorf("%w: %q", ErrDuplicateKind, sp.Kind)
		}
		if sp.Rate < 0 || sp.Rate > 1 {
			return fmt.Errorf("%w: %v for %q", ErrBadRate, sp.Rate, sp.Kind)
		}
		if err := Validate(sp); err != nil {
			return err
		}
		next[sp.Kind] = &ctlEntry{
			spec: sp,
			rnd:  e.seed.Stream(string(sp.Kind)).Rand(),
			seen: map[Method]int{},
		}
	}
	e.mu.Lock()
	e.specs = next
	e.mu.Unlock()
	return nil
}

func isControlKind(k Kind) bool {
	for _, c := range controlKinds {
		if c == k {
			return true
		}
	}
	return false
}

// Requests returns the number of requests decided so far.
func (e *ControlEngine) Requests() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.total
}

// Decide returns the faults that fire for one request. Call it exactly once per
// request. Each active kind that matches the method consumes one draw,
// unconditionally, so the sequence depends only on how many matching requests
// came before, never on earlier outcomes or on non-matching methods.
func (e *ControlEngine) Decide(m Method) ControlDecision {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.total++
	d := ControlDecision{Request: e.total}
	for _, k := range controlKinds {
		en := e.specs[k]
		if en == nil {
			continue
		}
		switch k {
		case KindRTSPError:
			if en.spec.On != "all" && en.spec.On != string(m) {
				continue
			}
			if en.fires(m, en.spec.Rate) {
				d.Error = &ErrorDecision{Status: en.spec.Status}
			}
		case KindSDPLie:
			if m != MethodDescribe {
				continue
			}
			if en.fires(m, rateOrOne(en.spec.Rate)) {
				d.Lie = &LieDecision{Mode: en.spec.Mode}
			}
		}
	}
	if d.Error != nil {
		// A lie inside a failed response has no SDP to misdescribe.
		d.Lie = nil
	}
	return d
}

// fires counts one matching request and draws once, then decides: OnlyRequest
// fires on exactly that matching request and ignores rate.
func (en *ctlEntry) fires(m Method, rate float64) bool {
	en.seen[m]++
	u := en.rnd.Float64()
	if en.spec.OnlyRequest > 0 {
		return en.seen[m] == en.spec.OnlyRequest
	}
	return u < rate
}

// rateOrOne treats an unset rate as 1. sdp_lie and teardown default to firing
// every time, and the JSON form omits a zero rate, so zero cannot mean "never"
// for them. Validate still refuses an explicit out-of-range value.
func rateOrOne(r float64) float64 {
	if r == 0 {
		return 1
	}
	return r
}

// ArmPlay decides, once per PLAY, whether the new session is torn down early.
// It returns nil when no teardown fault is configured or the draw misses.
func (e *ControlEngine) ArmPlay() *Arming {
	e.mu.Lock()
	defer e.mu.Unlock()
	en := e.specs[KindTeardown]
	if en == nil {
		return nil
	}
	if en.rnd.Float64() >= rateOrOne(en.spec.Rate) {
		return nil
	}
	return &Arming{AfterFrames: en.spec.AfterFrames, Mode: en.spec.Mode}
}
