package clock

import (
	"sync"
	"testing"
	"time"
)

func TestRealAdvances(t *testing.T) {
	var c Clock = Real{}
	t0 := c.Now()
	tk := c.NewTicker(5 * time.Millisecond)
	defer tk.Stop()
	select {
	case <-tk.C():
	case <-time.After(2 * time.Second):
		t.Fatal("real ticker never fired")
	}
	if !c.Now().After(t0) {
		t.Fatal("real clock did not advance")
	}
}

func TestVirtualOnlyMovesWhenAdvanced(t *testing.T) {
	start := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	v := NewVirtual(start)
	var c Clock = v

	if !c.Now().Equal(start) {
		t.Fatalf("Now() = %v, want %v", c.Now(), start)
	}

	tk := c.NewTicker(time.Second)
	defer tk.Stop()

	select {
	case <-tk.C():
		t.Fatal("virtual ticker fired without Advance")
	default:
	}

	v.Advance(time.Second)
	if !c.Now().Equal(start.Add(time.Second)) {
		t.Fatalf("Now() = %v after Advance", c.Now())
	}
	select {
	case at := <-tk.C():
		if !at.Equal(start.Add(time.Second)) {
			t.Fatalf("tick carried %v, want %v", at, start.Add(time.Second))
		}
	default:
		t.Fatal("virtual ticker did not fire after Advance")
	}
}

func TestVirtualAdvanceCoversMultiplePeriods(t *testing.T) {
	v := NewVirtual(time.Unix(0, 0).UTC())
	tk := v.NewTicker(100 * time.Millisecond)
	defer tk.Stop()

	// A single large Advance must not lose ticks silently beyond the one-slot
	// buffer that time.Ticker also uses; it must deliver at least one and leave
	// the clock at the right instant.
	v.Advance(350 * time.Millisecond)
	select {
	case <-tk.C():
	default:
		t.Fatal("no tick after a multi-period Advance")
	}
	if got := v.Now().Sub(time.Unix(0, 0).UTC()); got != 350*time.Millisecond {
		t.Fatalf("clock at %v, want 350ms", got)
	}
}

func TestVirtualStopSilencesTicker(t *testing.T) {
	v := NewVirtual(time.Unix(0, 0).UTC())
	tk := v.NewTicker(time.Second)
	tk.Stop()
	v.Advance(10 * time.Second)
	select {
	case <-tk.C():
		t.Fatal("stopped ticker still fired")
	default:
	}
}

// NewTicker documents that it panics on a non-positive interval; nothing
// exercised that path before this test.
func TestVirtualNewTickerPanicsOnNonPositiveDuration(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Second} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("NewTicker(%v) did not panic", d)
				}
			}()
			v := NewVirtual(time.Unix(0, 0).UTC())
			v.NewTicker(d)
		}()
	}
}

// TestVirtualConcurrentStopAndAdvance exercises Stop and Advance from two
// goroutines at once, for a sustained stretch of wall-clock time rather than
// a fixed number of iterations: a short, fixed-count race window frequently
// lets one goroutine finish before the other is even scheduled, which starves
// the race detector of an actual conflicting access. Advance reads and
// rewrites a ticker's stopped and next fields under v.mu on every tick it
// delivers, so Stop must take the same lock before touching stopped, or the
// two goroutines race on that field. That guard's protection is verified by
// the race detector (run this test with -race), and detection is
// probabilistic per run: an unguarded Stop is not guaranteed to fail any
// single run, only to become detectable given enough concurrent iterations.
// The two behavioural assertions at the end of this test are general
// correctness checks, not regression backstops for the race itself -- a data
// race between a guarded and an unguarded write to a bool is not something a
// deterministic assertion can distinguish, which is exactly why the race
// detector exists.
func TestVirtualConcurrentStopAndAdvance(t *testing.T) {
	v := NewVirtual(time.Unix(0, 0).UTC())
	tk := v.NewTicker(time.Nanosecond)

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)

	// Goroutine A hammers Stop() on the same ticker for the duration of the
	// test. Calling it repeatedly is idempotent and still a legitimate use:
	// it maximises the number of writes racing goroutine B's reads below.
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
				tk.Stop()
			}
		}
	}()

	// Goroutine B advances the clock concurrently and drains the ticker's
	// one-slot buffer after every step, counting how many advances it made
	// so the final state can be checked against that count.
	advances := 0
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
				v.Advance(time.Nanosecond)
				advances++
				select {
				case <-tk.C():
				default:
				}
			}
		}
	}()

	time.Sleep(20 * time.Millisecond)
	close(done)
	wg.Wait()

	// Behavioural assertions, independent of the race detector. First:
	// Advance's own bookkeeping must be exactly consistent with how many
	// times goroutine B actually called it, unaffected by the concurrent
	// Stop() calls landing alongside it.
	want := time.Unix(0, 0).UTC().Add(time.Duration(advances) * time.Nanosecond)
	if got := v.Now(); !got.Equal(want) {
		t.Fatalf("clock at %v after %d advances, want %v", got, advances, want)
	}

	// Second: the ticker has been (repeatedly) stopped, so it must deliver
	// nothing further, no matter how far the clock advances from here.
	v.Advance(time.Hour)
	select {
	case at := <-tk.C():
		t.Fatalf("stopped ticker delivered a tick at %v", at)
	default:
	}
}
