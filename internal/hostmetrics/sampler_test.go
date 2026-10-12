package hostmetrics

import (
	"context"
	"errors"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newClock() *fakeClock { return &fakeClock{t: time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)} }

// setProc writes a fake /proc whose cpu line has the given user and idle
// jiffies and whose process has used the given ticks.
func setProc(t *testing.T, dir string, user, idle, ticks string) {
	t.Helper()
	writeProc(t, dir, map[string]string{
		"proc/stat":       "cpu  " + user + " 0 0 " + idle + " 0 0 0 0\ncpu0 " + user + " 0 0 " + idle + " 0 0 0 0\n",
		"proc/meminfo":    meminfoFixture,
		"proc/loadavg":    "1.00 1.00 1.00 1/1 1\n",
		"proc/self/stat":  "1 (camfarm) S 1 1 1 0 -1 0 0 0 0 0 " + ticks + " 0 0 0 20 0 1 0 1 1 1 0\n",
		"proc/self/statm": "10 10 0 0 0 0 0\n",
		"proc/self/fd/0":  "",
	})
}

type harness struct {
	s     *Sampler
	clock *fakeClock
	ticks chan time.Time
	dir   string
}

func newHarness(t *testing.T, run Runner) *harness {
	t.Helper()
	dir := t.TempDir()
	setProc(t, dir, "100", "900", "0")
	clock := newClock()
	ticks := make(chan time.Time) // unbuffered: a send returns only once the loop took it
	s := New(Options{
		ProcRoot: dir + "/proc", CgroupRoot: dir + "/cg",
		Run: run, Ticks: ticks, Now: clock.Now,
	})
	t.Cleanup(s.Close)
	return &harness{s: s, clock: clock, ticks: ticks, dir: dir}
}

func (h *harness) tick(t *testing.T) {
	t.Helper()
	select {
	case h.ticks <- h.clock.Now():
	case <-time.After(5 * time.Second):
		t.Fatal("sampler did not take the tick")
	}
}

func recv(t *testing.T, ch <-chan Sample) Sample {
	t.Helper()
	select {
	case s, ok := <-ch:
		if !ok {
			t.Fatal("channel closed")
		}
		return s
	case <-time.After(5 * time.Second):
		t.Fatal("no sample")
		return Sample{}
	}
}

func okRunner(string) Runner { return runnerOut(oneGPU) }

func TestSamplerBaselineThenRealPercentages(t *testing.T) {
	h := newHarness(t, okRunner(""))
	sub := h.s.Subscribe(context.Background())

	h.tick(t)
	first := recv(t, sub)
	if first.CPU.UsagePercent != nil || first.Process.CPUPercent != nil {
		t.Fatalf("first sample has no baseline, want null: %+v %+v", first.CPU.UsagePercent, first.Process.CPUPercent)
	}
	if !first.CPU.Available || !first.Memory.Available || !first.Process.Available {
		t.Fatalf("first sample: %+v", first)
	}

	// busy 100 -> 200, total 1000 -> 2000: 10.0 %. Process 0 -> 50 ticks in 1 s: 50.0 %.
	setProc(t, h.dir, "200", "1800", "50")
	h.clock.Advance(time.Second)
	h.tick(t)
	second := recv(t, sub)
	if second.CPU.UsagePercent == nil || *second.CPU.UsagePercent != 10.0 {
		t.Fatalf("cpu %v", second.CPU.UsagePercent)
	}
	if second.Process.CPUPercent == nil || *second.Process.CPUPercent != 50.0 {
		t.Fatalf("proc %v", second.Process.CPUPercent)
	}
}

func TestSamplerCountersGoingBackwardsGiveNullNotNegative(t *testing.T) {
	h := newHarness(t, okRunner(""))
	sub := h.s.Subscribe(context.Background())
	h.tick(t)
	recv(t, sub)
	setProc(t, h.dir, "10", "90", "0") // counters reset (container restart)
	h.clock.Advance(time.Second)
	h.tick(t)
	if s := recv(t, sub); s.CPU.UsagePercent != nil {
		t.Fatalf("want null, got %v", *s.CPU.UsagePercent)
	}
}

func TestSamplerStaleBaselineIsNotAveraged(t *testing.T) {
	h := newHarness(t, okRunner(""))
	sub := h.s.Subscribe(context.Background())
	h.tick(t)
	recv(t, sub)
	setProc(t, h.dir, "200", "1800", "50")
	h.clock.Advance(time.Minute) // far longer than three intervals
	h.tick(t)
	if s := recv(t, sub); s.CPU.UsagePercent != nil || s.Process.CPUPercent != nil {
		t.Fatal("a stale baseline must give null")
	}
}

func TestSamplerIdlesWhenUnwatched(t *testing.T) {
	var calls atomic.Int32
	run := func(context.Context, string, ...string) ([]byte, []byte, error) {
		calls.Add(1)
		return []byte(oneGPU), nil, nil
	}
	h := newHarness(t, run)
	h.tick(t)
	h.tick(t) // barrier: the first tick is fully processed once the loop takes this one
	if _, ok := h.s.Latest(); ok || calls.Load() != 0 {
		t.Fatalf("unwatched sampler must not sample: latest=%v calls=%d", ok, calls.Load())
	}
	h.s.Touch()
	h.tick(t)
	h.tick(t)
	if _, ok := h.s.Latest(); !ok {
		t.Fatal("touched sampler must sample")
	}
	// Ten seconds without a touch and it goes quiet again.
	h.clock.Advance(11 * time.Second)
	before, _ := h.s.Latest()
	h.tick(t)
	h.tick(t)
	if after, _ := h.s.Latest(); !after.Time.Equal(before.Time) {
		t.Fatal("sampler kept sampling after the idle window")
	}
}

func TestSamplerGPUBackoff(t *testing.T) {
	var calls atomic.Int32
	run := func(context.Context, string, ...string) ([]byte, []byte, error) {
		calls.Add(1)
		return []byte("Failed to initialize NVML: Driver/library version mismatch\n"), nil, errors.New("exit status 18")
	}
	h := newHarness(t, run)
	sub := h.s.Subscribe(context.Background())

	// Tick until the first (asynchronous) GPU reading lands.
	for i := 0; ; i++ {
		if i > 200 {
			t.Fatal("GPU reading never arrived")
		}
		h.clock.Advance(time.Second)
		h.tick(t)
		if s := recv(t, sub); s.GPU.Reason != "pending" {
			if s.GPU.Available || s.GPU.Reason != "nvidia-smi failed: Failed to initialize NVML: Driver/library version mismatch" {
				t.Fatalf("gpu %+v", s.GPU)
			}
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	n0 := calls.Load()
	if n0 != 1 {
		t.Fatalf("want exactly one spawn so far, got %d", n0)
	}

	for i := 0; i < 5; i++ { // five seconds of ticks: still inside the 15 s retry window
		h.clock.Advance(time.Second)
		h.tick(t)
		if s := recv(t, sub); s.GPU.Available || s.GPU.Reason == "" {
			t.Fatalf("last reason must repeat: %+v", s.GPU)
		}
	}
	if calls.Load() != n0 {
		t.Fatalf("respawned inside the retry window: %d", calls.Load())
	}

	h.clock.Advance(15 * time.Second)
	h.tick(t)
	recv(t, sub)
	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() != n0+1 {
		if time.Now().After(deadline) {
			t.Fatalf("no retry after the window: %d", calls.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

var errNotFound = &exec.Error{Name: "nvidia-smi", Err: exec.ErrNotFound}

func TestSamplerNotFoundIsCachedForever(t *testing.T) {
	var n atomic.Int32
	notFound := func(context.Context, string, ...string) ([]byte, []byte, error) {
		n.Add(1)
		return nil, nil, errNotFound
	}
	h := newHarness(t, notFound)
	sub := h.s.Subscribe(context.Background())
	for i := 0; ; i++ {
		if i > 200 {
			t.Fatal("GPU reading never arrived")
		}
		h.clock.Advance(time.Second)
		h.tick(t)
		if s := recv(t, sub); s.GPU.Reason == "nvidia-smi not found" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.clock.Advance(time.Hour)
	h.tick(t)
	recv(t, sub)
	h.tick(t)
	recv(t, sub)
	if n.Load() != 1 {
		t.Fatalf("a missing binary must be tried once, got %d", n.Load())
	}
}

func TestSamplerSlowGPUDoesNotStallTheRest(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	run := func(ctx context.Context, _ string, _ ...string) ([]byte, []byte, error) {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil, nil, ctx.Err()
	}
	h := newHarness(t, run)
	sub := h.s.Subscribe(context.Background())
	h.tick(t)
	s := recv(t, sub) // would hang for 2 s if the GPU call were inline
	if !s.CPU.Available || !s.Memory.Available || !s.Process.Available {
		t.Fatalf("%+v", s)
	}
	if s.GPU.Available || s.GPU.Reason != "pending" {
		t.Fatalf("gpu %+v", s.GPU)
	}
	h.clock.Advance(time.Second)
	h.tick(t) // the loop is not blocked behind the hung call
	recv(t, sub)
}

func TestCurrentColdThenCached(t *testing.T) {
	h := newHarness(t, runnerOut(twoGPUWithNA))
	ctx := context.Background()
	first := h.s.Current(ctx)
	if !first.GPU.Available || len(first.GPU.Devices) != 2 {
		t.Fatalf("a cold Current must wait for the first GPU reading: %+v", first.GPU)
	}
	if first.CPU.UsagePercent != nil {
		t.Fatal("cold sample has no baseline")
	}
	h.clock.Advance(time.Second)
	if again := h.s.Current(ctx); !again.Time.Equal(first.Time) {
		t.Fatal("a sample younger than 2 s must be reused")
	}
	h.clock.Advance(5 * time.Second)
	if fresh := h.s.Current(ctx); fresh.Time.Equal(first.Time) {
		t.Fatal("an older sample must be replaced")
	}
}

func TestSlowSubscriberIsDropped(t *testing.T) {
	h := newHarness(t, okRunner(""))
	sub := h.s.Subscribe(context.Background())
	for i := 0; i < eventBuffer+2; i++ { // never read
		h.clock.Advance(time.Second)
		h.tick(t)
	}
	h.tick(t) // barrier
	n := 0
	for range sub { // a dropped subscription is closed, so this ends
		n++
		if n > eventBuffer {
			t.Fatal("subscriber was not dropped")
		}
	}
}

func TestCloseEndsSubscribersAndIsIdempotent(t *testing.T) {
	h := newHarness(t, okRunner(""))
	sub := h.s.Subscribe(context.Background())
	h.s.Close()
	h.s.Close()
	select {
	case _, ok := <-sub:
		if ok {
			t.Fatal("want closed channel")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not end the subscription")
	}
	if _, ok := <-h.s.Subscribe(context.Background()); ok {
		t.Fatal("Subscribe after Close must return a closed channel")
	}
}

func TestSubscribeEndsWithContext(t *testing.T) {
	h := newHarness(t, okRunner(""))
	ctx, cancel := context.WithCancel(context.Background())
	sub := h.s.Subscribe(ctx)
	cancel()
	select {
	case _, ok := <-sub:
		if ok {
			t.Fatal("want closed channel")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelling the context did not end the subscription")
	}
}
