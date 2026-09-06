// Package obs records what a fleet did, so a test can assert it and a failing
// test can report enough to reproduce it.
//
// It imports no other camfarm package on purpose: the fault engine records into
// it, so accepting a fault type here would be an import cycle. Fault events
// arrive as primitives.
package obs

import "sync"

// Event is one recorded fault decision.
type Event struct {
	// Seq numbers events from 1, across all cameras, in the order recorded.
	Seq        uint64
	CameraID   string
	FrameIndex int
	Kind       string
	Detail     string
}

// Counters are per-camera totals.
type Counters struct {
	FramesServed   uint64
	LoopsCompleted uint64
	FaultsFired    uint64
}

// Recorder collects counters and a bounded event log. It is safe for concurrent
// use.
type Recorder struct {
	mu        sync.Mutex
	maxEvents int
	seq       uint64
	dropped   uint64
	events    []Event
	counters  map[string]*Counters
}

// New returns a Recorder keeping at most maxEvents events. A non-positive
// maxEvents means keep none.
func New(maxEvents int) *Recorder {
	return &Recorder{
		maxEvents: maxEvents,
		counters:  make(map[string]*Counters),
	}
}

// counterFor returns the counters for id, creating them if needed. Caller holds
// r.mu.
func (r *Recorder) counterFor(id string) *Counters {
	c, ok := r.counters[id]
	if !ok {
		c = &Counters{}
		r.counters[id] = c
	}
	return c
}

// Frame records one access unit served. Frames are counted, never logged: there
// are far too many of them to keep individually.
func (r *Recorder) Frame(cameraID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counterFor(cameraID).FramesServed++
}

// Loop records one completed pass over the source media.
func (r *Recorder) Loop(cameraID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counterFor(cameraID).LoopsCompleted++
}

// Fault records a fault decision that fired.
func (r *Recorder) Fault(cameraID string, frameIndex int, kind, detail string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.counterFor(cameraID).FaultsFired++
	r.seq++

	if r.maxEvents <= 0 {
		r.dropped++
		return
	}
	ev := Event{
		Seq:        r.seq,
		CameraID:   cameraID,
		FrameIndex: frameIndex,
		Kind:       kind,
		Detail:     detail,
	}
	if len(r.events) == r.maxEvents {
		// Keep the most recent: a failing test cares about what just happened.
		copy(r.events, r.events[1:])
		r.events[len(r.events)-1] = ev
		r.dropped++
		return
	}
	r.events = append(r.events, ev)
}

// Counters returns a snapshot for one camera. An unknown camera reads as zero.
func (r *Recorder) Counters(cameraID string) Counters {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.counters[cameraID]; ok {
		return *c
	}
	return Counters{}
}

// Events returns a copy of the retained event log, oldest first.
func (r *Recorder) Events() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Event, len(r.events))
	copy(out, r.events)
	return out
}

// Dropped returns how many events were discarded because the log was full.
func (r *Recorder) Dropped() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dropped
}
