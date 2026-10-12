package hostmetrics

import (
	"context"
	"sync"
	"time"
)

const (
	defaultInterval   = time.Second
	defaultIdleAfter  = 10 * time.Second
	defaultGPURetry   = 15 * time.Second
	defaultGPUTimeout = 2 * time.Second
	defaultProcRoot   = "/proc"
	defaultCgroupRoot = "/sys/fs/cgroup"
	freshFor          = 2 * time.Second
	eventBuffer       = 8
	reasonPending     = "pending"
)

// Options configures a Sampler. Zero values select the defaults named below.
type Options struct {
	ProcRoot   string // default /proc
	CgroupRoot string // default /sys/fs/cgroup (cgroup v2)
	Run        Runner // default: exec nvidia-smi
	Interval   time.Duration
	// Ticks replaces the internal ticker. Tests use it with an unbuffered
	// channel so a completed send means the previous tick is fully processed.
	Ticks      <-chan time.Time
	Now        func() time.Time
	IdleAfter  time.Duration // default 10 s
	GPURetry   time.Duration // default 15 s
	GPUTimeout time.Duration // default 2 s
}

// base is the previous reading of one counter, used to turn it into a rate.
type base[T any] struct {
	ok bool
	at time.Time
	v  T
}

// prev returns the previous value if it is recent enough to average over.
func (b *base[T]) prev(now time.Time, maxAge time.Duration) (T, bool) {
	var zero T
	if !b.ok || !now.After(b.at) || now.Sub(b.at) > maxAge {
		return zero, false
	}
	return b.v, true
}

func (b *base[T]) set(now time.Time, v T) { b.ok, b.at, b.v = true, now, v }
func (b *base[T]) reset()                 { b.ok = false }

type gpuState struct {
	mu       sync.Mutex
	sec      GPU
	have     bool
	at       time.Time
	inflight bool
	missing  bool
	done     chan struct{}
}

type sub struct {
	ch   chan Sample
	gone chan struct{} // closed when the subscription is dropped
}

// Sampler samples while it is watched: an event-stream client is subscribed,
// or Current or Touch was called within IdleAfter. Otherwise it does nothing.
type Sampler struct {
	rd         procReader
	run        Runner
	now        func() time.Time
	interval   time.Duration
	idleAfter  time.Duration
	gpuRetry   time.Duration
	gpuTimeout time.Duration

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu          sync.Mutex // latest, wantedUntil, subs, closed
	latest      *Sample
	wantedUntil time.Time
	subs        map[*sub]struct{}
	closed      bool

	cmu      sync.Mutex // the counter baselines
	cpuBase  base[cpuTimes]
	procBase base[uint64]

	g gpuState
}

// New starts a Sampler. Close it when done.
func New(opt Options) *Sampler {
	s := &Sampler{
		rd:         procReader{proc: opt.ProcRoot, cgroup: opt.CgroupRoot},
		run:        opt.Run,
		now:        opt.Now,
		interval:   opt.Interval,
		idleAfter:  opt.IdleAfter,
		gpuRetry:   opt.GPURetry,
		gpuTimeout: opt.GPUTimeout,
		subs:       map[*sub]struct{}{},
	}
	if s.rd.proc == "" {
		s.rd.proc = defaultProcRoot
	}
	if s.rd.cgroup == "" {
		s.rd.cgroup = defaultCgroupRoot
	}
	if s.run == nil {
		s.run = execRunner
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.interval <= 0 {
		s.interval = defaultInterval
	}
	if s.idleAfter <= 0 {
		s.idleAfter = defaultIdleAfter
	}
	if s.gpuRetry <= 0 {
		s.gpuRetry = defaultGPURetry
	}
	if s.gpuTimeout <= 0 {
		s.gpuTimeout = defaultGPUTimeout
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())

	ticks := opt.Ticks
	var ticker *time.Ticker
	if ticks == nil {
		ticker = time.NewTicker(s.interval)
		ticks = ticker.C
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		if ticker != nil {
			defer ticker.Stop()
		}
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-ticks:
				if s.watched() {
					s.publish(s.collect(s.ctx, false))
				}
			}
		}
	}()
	return s
}

func (s *Sampler) watched() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.subs) > 0 || s.now().Before(s.wantedUntil)
}

// Touch marks the sampler wanted for IdleAfter.
func (s *Sampler) Touch() {
	s.mu.Lock()
	s.wantedUntil = s.now().Add(s.idleAfter)
	s.mu.Unlock()
}

// Latest returns the most recent sample, if any.
func (s *Sampler) Latest() (Sample, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.latest == nil {
		return Sample{}, false
	}
	return *s.latest, true
}

// Current marks the sampler wanted and returns a sample no older than 2 s,
// sampling synchronously if it has to. A cold call waits for the first GPU
// reading (at most the GPU timeout).
func (s *Sampler) Current(ctx context.Context) Sample {
	s.Touch()
	if l, ok := s.Latest(); ok && s.now().Sub(l.Time) < freshFor {
		return l
	}
	smp := s.collect(ctx, true)
	s.publish(smp)
	return smp
}

func (s *Sampler) collect(ctx context.Context, waitGPU bool) Sample {
	now := s.now()
	smp := Sample{Time: now.UTC()}
	maxAge := 3 * s.interval

	s.cmu.Lock()
	ct, cpu, cerr := s.rd.cpu()
	if cerr == nil {
		if p, ok := s.cpuBase.prev(now, maxAge); ok && ct.total > p.total && ct.busy >= p.busy {
			v := percent(ct.busy-p.busy, ct.total-p.total)
			cpu.UsagePercent = &v
		}
		s.cpuBase.set(now, ct)
	} else {
		s.cpuBase.reset()
	}
	smp.CPU = cpu

	smp.Memory, _ = s.rd.memory()

	ticks, proc, perr := s.rd.process()
	if perr == nil {
		if p, ok := s.procBase.prev(now, maxAge); ok && ticks >= p {
			secs := now.Sub(s.procBase.at).Seconds()
			v := round1(float64(ticks-p) / clockTick / secs * 100)
			proc.CPUPercent = &v
		}
		s.procBase.set(now, ticks)
	} else {
		s.procBase.reset()
	}
	smp.Process = proc
	s.cmu.Unlock()

	// The GPU step runs outside cmu so a slow nvidia-smi never holds up the
	// counters of another caller.
	smp.GPU = s.gpu(ctx, now, waitGPU)

	s.mu.Lock()
	s.latest = &smp
	s.mu.Unlock()
	return smp
}

// gpu returns the cached GPU reading, starting a refresh when one is due:
// every sample while the last reading worked, and after a failure no more
// often than GPURetry. A missing nvidia-smi is never retried. With wait set, a
// call that has no reading yet waits for the one in flight.
func (s *Sampler) gpu(ctx context.Context, now time.Time, wait bool) GPU {
	g := &s.g
	g.mu.Lock()
	if !g.inflight && !g.missing && (!g.have || g.sec.Available || now.Sub(g.at) >= s.gpuRetry) {
		s.mu.Lock()
		if !s.closed {
			s.wg.Add(1)
			g.inflight = true
			g.done = make(chan struct{})
			go s.runGPU(g.done)
		}
		s.mu.Unlock()
	}
	done, have := g.done, g.have
	g.mu.Unlock()

	if wait && !have && done != nil {
		select {
		case <-done:
		case <-ctx.Done():
		}
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.have {
		return unavailableGPU(reasonPending)
	}
	return g.sec
}

func (s *Sampler) runGPU(done chan struct{}) {
	defer s.wg.Done()
	ctx, cancel := context.WithTimeout(s.ctx, s.gpuTimeout)
	defer cancel()
	sec, missing := queryGPU(ctx, s.run)
	g := &s.g
	g.mu.Lock()
	g.sec, g.have, g.at, g.inflight, g.missing = sec, true, s.now(), false, missing
	g.mu.Unlock()
	close(done)
}

// Subscribe returns a channel of samples that ends when ctx does, when the
// subscriber falls behind, or when the sampler closes. While at least one
// subscription is open the sampler samples every tick.
func (s *Sampler) Subscribe(ctx context.Context) <-chan Sample {
	c := &sub{ch: make(chan Sample, eventBuffer), gone: make(chan struct{})}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		close(c.ch)
		return c.ch
	}
	s.subs[c] = struct{}{}
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		select {
		case <-ctx.Done():
			s.mu.Lock()
			s.dropLocked(c)
			s.mu.Unlock()
		case <-c.gone:
		}
	}()
	return c.ch
}

// publish never blocks: a full subscriber is dropped and reconnects.
func (s *Sampler) publish(smp Sample) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.subs {
		select {
		case c.ch <- smp:
		default:
			s.dropLocked(c)
		}
	}
}

func (s *Sampler) dropLocked(c *sub) {
	if _, ok := s.subs[c]; !ok {
		return
	}
	delete(s.subs, c)
	close(c.ch)
	close(c.gone)
}

// Close stops the sampler, ends every subscription and waits for its
// goroutines. It is safe to call twice.
func (s *Sampler) Close() {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		s.cancel()
		for c := range s.subs {
			s.dropLocked(c)
		}
	}
	s.mu.Unlock()
	s.wg.Wait()
}
