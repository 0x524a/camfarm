package rtsp

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/pion/rtp"

	"github.com/0x524a/camfarm/internal/clock"
	"github.com/0x524a/camfarm/internal/fault"
	"github.com/0x524a/camfarm/internal/obs"
	"github.com/0x524a/camfarm/internal/seed"
)

// signalClock is a virtual clock that announces when run has created its
// ticker, so a test never advances time before the ticker exists.
type signalClock struct {
	*clock.Virtual
	ticker chan struct{}
}

func newSignalClock() *signalClock {
	return &signalClock{Virtual: clock.NewVirtual(time.Unix(1000, 0)), ticker: make(chan struct{})}
}

func (c *signalClock) NewTicker(d time.Duration) clock.Ticker {
	tk := c.Virtual.NewTicker(d)
	close(c.ticker)
	return tk
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

func jitterRunPump(t *testing.T, s seed.Seed, rec *obs.Recorder, w packetWriter, after func()) *Pump {
	t.Helper()
	p, err := NewPump(PumpConfig{
		CameraID: "cam", Media: regularMedia(t, 3000, 30), Medi: &description.Media{Type: description.MediaTypeVideo},
		Writer: w, Seed: s, Obs: rec, AfterFrame: after,
		Faults: []fault.Spec{{Kind: fault.KindJitter, MaxMs: 5000}},
	})
	if err != nil {
		t.Fatalf("NewPump: %v", err)
	}
	return p
}

func TestRunPacesJitteredFramesInOrderAndBunches(t *testing.T) {
	ck := newSignalClock()
	w := &clockRecorder{ck: ck}
	p := jitterRunPump(t, seed.Seed(3), obs.New(1000), w, nil)
	interval := p.interval()
	start := ck.Now()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.run(ctx, ck, nil); close(done) }()
	<-ck.ticker

	for i := 0; i < 3000; i++ {
		ck.Advance(time.Millisecond)
		runtime.Gosched()
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return after cancel")
	}

	ws := w.snapshot()
	if len(ws) < 5 {
		t.Fatalf("only %d packets written", len(ws))
	}
	bunched := false
	var prev stampedWrite
	for i, x := range ws {
		if i > 0 {
			if x.ts < prev.ts {
				t.Fatalf("write %d: timestamp %d after %d (order lost)", i, x.ts, prev.ts)
			}
			if x.seq != prev.seq+1 {
				t.Fatalf("write %d: sequence %d after %d", i, x.seq, prev.seq)
			}
			if x.ts != prev.ts && x.at.Sub(prev.at) < interval {
				bunched = true
			}
		}
		prev = x
		// Frame k (timestamp k*3000) is planned from the k-th tick at the
		// earliest, so it can never leave before (k+1) intervals.
		k := int(x.ts / 3000)
		if x.at.Before(start.Add(time.Duration(k+1) * interval)) {
			t.Fatalf("write %d (frame %d) at %v, before its nominal tick", i, k, x.at.Sub(start))
		}
	}
	if !bunched {
		t.Fatal("no two consecutive frames arrived less than one interval apart")
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
	p := jitterRunPump(t, s, rec, w, nil)
	base := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.run(ctx, ck, nil); close(done) }()
	<-ck.ticker
	ck.Advance(p.interval()) // delivers the first tick; the clock never moves again

	deadline := time.Now().Add(5 * time.Second)
	for len(rec.Events()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("run never planned a delay")
		}
		runtime.Gosched()
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
	for runtime.NumGoroutine() > base && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if got := runtime.NumGoroutine(); got > base {
		t.Fatalf("goroutines = %d, baseline %d", got, base)
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
	p := jitterRunPump(t, seed.Seed(3), obs.New(1000), w, after)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.run(ctx, ck, nil); close(done) }()
	<-ck.ticker
	for i := 0; i < 3000; i++ {
		ck.Advance(time.Millisecond)
		runtime.Gosched()
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
