package rtsp

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/pion/rtp"

	"github.com/0x524a/camfarm/internal/clock"
	"github.com/0x524a/camfarm/internal/fault"
	"github.com/0x524a/camfarm/internal/obs"
	"github.com/0x524a/camfarm/internal/seed"
)

// signalClock is a virtual clock that announces when run has registered its
// first deadline, so a test never advances time before run is waiting on it.
type signalClock struct {
	*clock.Virtual
	started chan struct{}
	once    sync.Once
}

func newSignalClock() *signalClock {
	return &signalClock{Virtual: clock.NewVirtual(time.Unix(1000, 0)), started: make(chan struct{})}
}

func (c *signalClock) After(d time.Duration) <-chan time.Time {
	ch := c.Virtual.After(d) // registered before the signal, so a waiter exists once started closes
	c.once.Do(func() { close(c.started) })
	return ch
}

// advanceSettled advances the clock by d and returns once run has absorbed the
// advance. run paces on absolute deadlines and always waits on a clock waiter
// while idle, and the virtual clock never moves on its own, so run has settled
// exactly when it is parked on a waiter again: everything that fell due,
// including every emit and AfterFrame it led to, has happened. It polls,
// bounded and without sleeping. Only valid before cancel, which can leave an
// abandoned waiter behind.
func advanceSettled(t *testing.T, ck *signalClock, d time.Duration) {
	t.Helper()
	ck.Advance(d)
	deadline := time.Now().Add(5 * time.Second)
	for ck.Waiters() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("run did not settle after the clock advanced")
		}
		runtime.Gosched()
	}
}

// waitParked polls until run is parked on at least one clock waiter.
func waitParked(t *testing.T, ck *signalClock) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for ck.Waiters() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("run never parked on a clock waiter")
		}
		runtime.Gosched()
	}
}

type stampedWrite struct {
	ts  uint32
	seq uint16
	at  time.Time
}

// clockRecorder is a packetWriter that notes the virtual time of each write.
type clockRecorder struct {
	mu sync.Mutex
	ck clock.Clock
	ws []stampedWrite
}

func (r *clockRecorder) WritePacketRTP(_ *description.Media, pkt *rtp.Packet) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ws = append(r.ws, stampedWrite{ts: pkt.Timestamp, seq: pkt.SequenceNumber, at: r.ck.Now()})
	return nil
}

func (r *clockRecorder) snapshot() []stampedWrite {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]stampedWrite(nil), r.ws...)
}

// jitterRunPump builds a 30 fps pump (one frame is 3000 ticks) with jitter at
// maxMs, or with no fault at all when maxMs is zero.
func jitterRunPump(t *testing.T, s seed.Seed, maxMs int, rec *obs.Recorder, w packetWriter, after func()) *Pump {
	t.Helper()
	var faults []fault.Spec
	if maxMs > 0 {
		faults = []fault.Spec{{Kind: fault.KindJitter, MaxMs: maxMs}}
	}
	p, err := NewPump(PumpConfig{
		CameraID: "cam", Media: regularMedia(t, 3000, 30), Medi: &description.Media{Type: description.MediaTypeVideo},
		Writer: w, Seed: s, Obs: rec, AfterFrame: after, Faults: faults,
	})
	if err != nil {
		t.Fatalf("NewPump: %v", err)
	}
	return p
}

// jitterRun runs a jittered pump on a virtual clock for steps one-millisecond
// advances and returns what it wrote, with the start time and frame interval.
func jitterRun(t *testing.T, s seed.Seed, maxMs, steps int) (ws []stampedWrite, start time.Time, interval time.Duration) {
	t.Helper()
	ck := newSignalClock()
	w := &clockRecorder{ck: ck}
	p := jitterRunPump(t, s, maxMs, obs.New(10), w, nil)
	interval = p.interval()
	start = ck.Now()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.run(ctx, ck, nil); close(done) }()
	<-ck.started
	for i := 0; i < steps; i++ {
		advanceSettled(t, ck, time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return after cancel")
	}
	return w.snapshot(), start, interval
}

// jitterDue is the model's schedule, computed independently of run: frame k's
// nominal slot is start + (k+1)*interval, it is due at slot + its seeded delay,
// and never before the frame ahead of it.
func jitterDue(t *testing.T, s seed.Seed, maxMs, n int, start time.Time, interval time.Duration) []time.Time {
	t.Helper()
	e, err := fault.New(s.Stream("fault"), []fault.Spec{{Kind: fault.KindJitter, MaxMs: maxMs}})
	if err != nil {
		t.Fatal(err)
	}
	due := make([]time.Time, n)
	var prev time.Time
	for k := range due {
		d := start.Add(time.Duration(k+1) * interval).Add(e.FrameDelay(k))
		if prev.After(d) {
			d = prev
		}
		due[k], prev = d, d
	}
	return due
}

// checkJitterSchedule asserts the written frames follow the model exactly:
// frames 0, 1, 2, ... in order, each written at its due time (to the 1 ms
// advance step), sequence numbers contiguous, and exactly the frames due by end.
// It returns the per-frame write times.
func checkJitterSchedule(t *testing.T, ws []stampedWrite, due []time.Time, end time.Time) []time.Time {
	t.Helper()
	var at []time.Time
	for i, x := range ws {
		if i > 0 && x.seq != ws[i-1].seq+1 {
			t.Fatalf("write %d: sequence %d after %d", i, x.seq, ws[i-1].seq)
		}
		if i > 0 && x.ts == ws[i-1].ts {
			continue // another packet of the same frame
		}
		k := len(at)
		if want := uint32(k * 3000); x.ts != want {
			t.Fatalf("frame %d carries timestamp %d, want %d (order lost)", k, x.ts, want)
		}
		if k >= len(due) {
			t.Fatalf("frame %d written; the model has only %d frames", k, len(due))
		}
		if x.at.Before(due[k]) || !x.at.Before(due[k].Add(time.Millisecond)) {
			t.Fatalf("frame %d written at %v, due at %v", k, x.at, due[k])
		}
		at = append(at, x.at)
	}
	want := 0
	for want < len(due) && !due[want].After(end) {
		want++
	}
	if len(at) != want {
		t.Fatalf("%d frames written, the model has %d due by the end of the run", len(at), want)
	}
	return at
}

func TestRunPacesJitteredFramesInOrderAndBunches(t *testing.T) {
	const (
		maxMs = 5000
		steps = 12000 // 12 s
	)
	s := seed.Seed(3)
	ws, start, interval := jitterRun(t, s, maxMs, steps)
	end := start.Add(steps * time.Millisecond)
	due := jitterDue(t, s, maxMs, 1000, start, interval)
	at := checkJitterSchedule(t, ws, due, end)

	bunched := false
	for k := 1; k < len(at); k++ {
		if at[k].Sub(at[k-1]) < interval {
			bunched = true
		}
		// Never before the nominal slot.
		if slot := start.Add(time.Duration(k+1) * interval); at[k].Before(slot) {
			t.Fatalf("frame %d at %v, before its nominal slot %v", k, at[k].Sub(start), slot.Sub(start))
		}
	}
	if !bunched {
		t.Fatal("no two consecutive frames arrived less than one interval apart")
	}

	// The rate stays nominal: every frame is due before its slot + maxMs, so by
	// the end every frame whose slot is at least maxMs earlier has been written,
	// and none whose slot is still ahead.
	minFrames := int((end.Sub(start)-maxMs*time.Millisecond)/interval) - 1
	maxFrames := int(end.Sub(start)/interval) + 1
	if len(at) < minFrames || len(at) > maxFrames {
		t.Fatalf("%d frames written in %v, want %d to %d at the nominal rate", len(at), end.Sub(start), minFrames, maxFrames)
	}
}

// TestRunJitterKeepsTheNominalFrameRate is the regression test for jitter
// slowing the stream: with a delay up to 1 s on a 33 ms interval, every frame
// whose slot falls in the first T seconds is written by T + maxMs. A loop that
// measured each delay from the previous wake-up would fall to a few frames per
// second here.
func TestRunJitterKeepsTheNominalFrameRate(t *testing.T) {
	const (
		maxMs = 1000
		T     = 20 * time.Second
	)
	s := seed.Seed(5)
	steps := int((T + maxMs*time.Millisecond) / time.Millisecond)
	ws, start, interval := jitterRun(t, s, maxMs, steps)
	due := jitterDue(t, s, maxMs, 1000, start, interval)
	at := checkJitterSchedule(t, ws, due, start.Add(time.Duration(steps)*time.Millisecond))

	inT := 0
	for k := range at {
		if !start.Add(time.Duration(k+1) * interval).After(start.Add(T)) {
			inT++
		}
	}
	nominal := int(T / interval)
	if inT < nominal-1 || inT > nominal+1 {
		t.Fatalf("%d frames with a slot in the first %v were written, want %d +/- 1 (nominal rate)", inT, T, nominal)
	}
}

// TestRunCatchesUpBelowTheGuardAndResyncsAboveIt covers the resync guard with
// no jitter. A stall shorter than max(1s, 10 intervals) is caught up: every
// missed slot's frame goes out at once, so the rate stays nominal. A stall
// longer than that (a suspended process) is not: the loop plans one frame and
// restarts its schedule from now, rather than emitting an hour of frames.
// Timestamps stay frame-counted throughout, so no frame is lost from the RTP
// timeline; only wall-clock slots are skipped.
func TestRunCatchesUpBelowTheGuardAndResyncsAboveIt(t *testing.T) {
	ck := newSignalClock()
	w := &clockRecorder{ck: ck}
	var frames atomic.Int64
	p := jitterRunPump(t, seed.Seed(1), 0, obs.New(10), w, func() { frames.Add(1) })
	interval := p.interval()
	if g := resyncAfter(interval); g != time.Second {
		t.Fatalf("guard %v at a %v interval, want 1s", g, interval)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.run(ctx, ck, nil); close(done) }()
	<-ck.started
	defer func() {
		cancel()
		<-done
	}()

	// 100 ms: frames at slots 1, 2, 3.
	for i := 0; i < 100; i++ {
		advanceSettled(t, ck, time.Millisecond)
	}
	if n := frames.Load(); n != 3 {
		t.Fatalf("%d frames in 100 ms at 30 fps, want 3", n)
	}

	// A 900 ms stall, below the guard: slots 4 to 30 all fall due and are
	// caught up back-to-back at the instant the clock lands.
	advanceSettled(t, ck, 900*time.Millisecond)
	stallEnd := ck.Now()
	if n := frames.Load(); n != 30 {
		t.Fatalf("%d frames after a 900 ms stall, want 30 (caught up)", n)
	}
	caught := 0
	stalled := w.snapshot()
	for i, x := range stalled {
		if x.at.Equal(stallEnd) && (i == 0 || x.ts != stalled[i-1].ts) {
			caught++
		}
	}
	if caught != 27 {
		t.Fatalf("%d frames written as the stall ended, want 27", caught)
	}

	// An hour-long stall, far above the guard: one frame, then the schedule
	// restarts from now.
	before := frames.Load()
	advanceSettled(t, ck, time.Hour)
	resyncAt := ck.Now()
	if n := frames.Load() - before; n != 1 {
		t.Fatalf("%d frames written as an hour-long stall ended, want 1 (resync, no catch-up burst)", n)
	}
	for i := 0; i < 100; i++ {
		advanceSettled(t, ck, time.Millisecond)
	}
	if n := frames.Load() - before; n != 4 {
		t.Fatalf("%d frames in the 100 ms after the resync, want 4 (resync frame + 3 slots)", n)
	}

	ws := w.snapshot()
	var firsts []stampedWrite // the first packet of each frame
	for i, x := range ws {
		if i > 0 && x.seq != ws[i-1].seq+1 {
			t.Fatalf("write %d: sequence %d after %d", i, x.seq, ws[i-1].seq)
		}
		if i > 0 && x.ts == ws[i-1].ts {
			continue
		}
		if want := uint32(len(firsts) * 3000); x.ts != want {
			t.Fatalf("frame %d: timestamp %d, want %d: frames lost from the timeline", len(firsts), x.ts, want)
		}
		firsts = append(firsts, x)
	}
	// The frames after the resync sit on the new schedule: resyncAt + j*interval.
	tail := firsts[len(firsts)-4:]
	for j, x := range tail {
		slot := resyncAt.Add(time.Duration(j) * interval)
		if x.at.Before(slot) || !x.at.Before(slot.Add(time.Millisecond)) {
			t.Fatalf("frame %d after the resync at %v, want its new slot %v", j, x.at.Sub(resyncAt), slot.Sub(resyncAt))
		}
	}
}

func TestRunReturnsPromptlyWhileWaitingOnADeadline(t *testing.T) {
	// A seed whose first delay is long, so run is parked on it.
	var s seed.Seed
	for n := 1; n < 1000; n++ {
		e, _ := fault.New(seed.Seed(n).Stream("fault"), []fault.Spec{{Kind: fault.KindJitter, MaxMs: 5000}})
		if e.FrameDelay(0) >= 2*time.Second {
			s = seed.Seed(n)
			break
		}
	}
	if s == 0 {
		t.Fatal("no seed with a long first delay")
	}
	ck := newSignalClock()
	rec := obs.New(100)
	w := &clockRecorder{ck: ck}
	p := jitterRunPump(t, s, 5000, rec, w, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.run(ctx, ck, nil); close(done) }()
	<-ck.started
	ck.Advance(p.interval()) // reaches the first slot; the clock never moves again

	waitParked(t, ck)
	if n := ck.Waiters(); n != 1 {
		t.Fatalf("%d clock waiters while parked on one delay, want 1", n)
	}
	if len(rec.Events()) == 0 {
		t.Fatal("run parked without recording the planned delay")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("run did not return while waiting on a deadline")
	}
	if n := len(w.snapshot()); n != 0 {
		t.Fatalf("%d packets written despite the cancelled wait", n)
	}
	// The cancelled wait leaves nothing for the clock to wake: advancing past
	// the abandoned deadline must neither block nor reach a writer.
	ck.Advance(time.Hour)
	if n := len(w.snapshot()); n != 0 {
		t.Fatalf("%d packets written after cancel and advance", n)
	}
}

func TestAfterFrameFiresAfterTheDelayedEmit(t *testing.T) {
	ck := newSignalClock()
	w := &clockRecorder{ck: ck}
	var mu sync.Mutex
	var written []int
	after := func() {
		n := len(w.snapshot())
		mu.Lock()
		written = append(written, n)
		mu.Unlock()
	}
	p := jitterRunPump(t, seed.Seed(3), 5000, obs.New(1000), w, after)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.run(ctx, ck, nil); close(done) }()
	<-ck.started
	for i := 0; i < 3000; i++ {
		advanceSettled(t, ck, time.Millisecond)
	}
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if len(written) == 0 {
		t.Fatal("AfterFrame never called")
	}
	for i := 1; i < len(written); i++ {
		if written[i] <= written[i-1] {
			t.Fatalf("AfterFrame %d saw %d packets written, previous %d: it fired before its frame was emitted", i, written[i], written[i-1])
		}
	}
}
