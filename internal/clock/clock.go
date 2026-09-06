// Package clock supplies the time source the media pump is paced by.
//
// It governs pacing only. Determinism does not rest on it: the pump's unit of
// work is one access unit, and fault decisions key on a frame counter, so a
// deterministic test drives the pump directly and never consults a clock.
// Virtual exists so the driver loop can be exercised without real sleeping.
package clock

import (
	"sync"
	"time"
)

// Ticker is the subset of time.Ticker the pump driver needs.
type Ticker interface {
	C() <-chan time.Time
	Stop()
}

// Clock is a source of time.
type Clock interface {
	Now() time.Time
	NewTicker(d time.Duration) Ticker
}

// Real is the wall clock.
type Real struct{}

// Now returns the current time.
func (Real) Now() time.Time { return time.Now() }

// NewTicker returns a ticker backed by time.Ticker.
func (Real) NewTicker(d time.Duration) Ticker { return &realTicker{t: time.NewTicker(d)} }

type realTicker struct{ t *time.Ticker }

func (r *realTicker) C() <-chan time.Time { return r.t.C }
func (r *realTicker) Stop()               { r.t.Stop() }

// Virtual is a clock that only moves when Advance is called.
type Virtual struct {
	mu      sync.Mutex
	now     time.Time
	tickers []*virtualTicker
}

// NewVirtual returns a Virtual clock reading start.
func NewVirtual(start time.Time) *Virtual { return &Virtual{now: start} }

// Now returns the current virtual time.
func (v *Virtual) Now() time.Time {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.now
}

// NewTicker returns a ticker that fires during Advance.
func (v *Virtual) NewTicker(d time.Duration) Ticker {
	if d <= 0 {
		panic("clock: non-positive ticker interval")
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	tk := &virtualTicker{
		owner:  v,
		ch:     make(chan time.Time, 1),
		period: d,
		next:   v.now.Add(d),
	}
	v.tickers = append(v.tickers, tk)
	return tk
}

// Advance moves the clock forward and delivers every tick that falls due.
//
// Delivery is non-blocking into a one-slot buffer, matching time.Ticker: a
// receiver that is not keeping up loses ticks rather than stalling the clock.
func (v *Virtual) Advance(d time.Duration) {
	v.mu.Lock()
	defer v.mu.Unlock()

	target := v.now.Add(d)
	for _, tk := range v.tickers {
		for !tk.stopped && !tk.next.After(target) {
			select {
			case tk.ch <- tk.next:
			default:
			}
			tk.next = tk.next.Add(tk.period)
		}
	}
	v.now = target
}

// stop marks tk stopped, holding the clock's lock so that a concurrent
// Advance cannot observe tk.stopped or tk.next mid-write. next is shared
// mutable state, not just a flag, so a plain atomic bool on stopped would
// not be enough: Advance also reads and rewrites next under v.mu.
func (v *Virtual) stop(tk *virtualTicker) {
	v.mu.Lock()
	defer v.mu.Unlock()
	tk.stopped = true
}

type virtualTicker struct {
	owner   *Virtual
	ch      chan time.Time
	period  time.Duration
	next    time.Time
	stopped bool
}

func (t *virtualTicker) C() <-chan time.Time { return t.ch }
func (t *virtualTicker) Stop()               { t.owner.stop(t) }
