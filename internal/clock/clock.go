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
	// After returns a channel, buffered to one, that receives the time once d
	// has elapsed on this clock. A d of zero or less fires immediately. Waiters
	// that fall due in one Virtual.Advance fire in registration order, not
	// deadline order.
	After(d time.Duration) <-chan time.Time
}

// Real is the wall clock.
type Real struct{}

// Now returns the current time.
func (Real) Now() time.Time { return time.Now() }

// NewTicker returns a ticker backed by time.Ticker.
func (Real) NewTicker(d time.Duration) Ticker { return &realTicker{t: time.NewTicker(d)} }

// After is time.After.
func (Real) After(d time.Duration) <-chan time.Time { return time.After(d) }

type realTicker struct{ t *time.Ticker }

func (r *realTicker) C() <-chan time.Time { return r.t.C }
func (r *realTicker) Stop()               { r.t.Stop() }

// Virtual is a clock that only moves when Advance is called.
type Virtual struct {
	mu      sync.Mutex
	now     time.Time
	tickers []*virtualTicker
	waiters []waiter
}

// waiter is a pending After: ch receives the virtual time once the clock
// reaches at.
type waiter struct {
	at time.Time
	ch chan time.Time
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

// After returns a channel that receives once Advance carries the clock to or
// past now+d. A d of zero or less fires immediately. When one Advance makes
// several waiters due they fire in registration order, not deadline order.
func (v *Virtual) After(d time.Duration) <-chan time.Time {
	v.mu.Lock()
	defer v.mu.Unlock()
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- v.now
		return ch
	}
	v.waiters = append(v.waiters, waiter{at: v.now.Add(d), ch: ch})
	return ch
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

	// Fire and drop due waiters. Each channel has a one-slot buffer and a
	// single send, so the send never blocks.
	kept := v.waiters[:0]
	for _, w := range v.waiters {
		if w.at.After(target) {
			kept = append(kept, w)
			continue
		}
		w.ch <- target
	}
	clear(v.waiters[len(kept):]) // do not retain fired channels in the dropped tail
	v.waiters = kept
}

// Waiters returns how many After calls are pending: registered and not yet
// due. It lets a test wait until a goroutine is parked on the clock.
func (v *Virtual) Waiters() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.waiters)
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
