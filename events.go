package camfarm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
)

// EventType names a change to a running fleet.
type EventType string

// Event types.
const (
	EventCameraAdded   EventType = "camera_added"
	EventCameraRemoved EventType = "camera_removed"
	EventFaultInjected EventType = "fault_injected"
	EventFaultCleared  EventType = "fault_cleared"
)

// Event is one change to a running fleet.
type Event struct {
	Seq    uint64     `json:"seq"`
	Type   EventType  `json:"type"`
	Camera string     `json:"camera"`
	Fault  *FaultSpec `json:"fault,omitempty"`
	// FrameIndex is the frame a fault change took effect on; -1 for add/remove.
	FrameIndex int `json:"frameIndex"`
	// Clock names what FrameIndex counts: "frame" or "request". Empty for
	// add/remove.
	Clock string `json:"clock,omitempty"`
}

// JournalEntry records one runtime change. Start-time cameras and faults are
// not journaled: the spec hash already identifies them.
type JournalEntry struct {
	Seq    uint64    `json:"seq"`
	Action string    `json:"action"` // add, remove, inject, clear
	Camera string    `json:"camera"`
	Fault  FaultSpec `json:"fault"`
	// FrameIndex is when a fault change landed. It depends on when the change
	// was requested, which is the limit of replaying an interactive run.
	FrameIndex int `json:"frameIndex"`
	// Clock says whether FrameIndex counts frames (per-frame faults) or RTSP
	// requests (control-plane faults). Part of the replay hash.
	Clock string `json:"clock,omitempty"`
}

const subscriberBuffer = 64

// subscriber is one Subscribe call. end closes both channels exactly once; the
// caller must have removed s from events.subs under events.mu first. done lets
// the per-subscription goroutine exit even when its ctx never ends.
type subscriber struct {
	ch   chan Event
	done chan struct{}
}

func (s *subscriber) end() {
	close(s.ch)
	close(s.done)
}

// events holds the fleet's change feed and journal.
type events struct {
	mu      sync.Mutex
	seq     uint64
	journal []JournalEntry
	subs    map[*subscriber]struct{}
	closed  bool
}

func (e *events) init() { e.subs = make(map[*subscriber]struct{}) }

// journalActions maps the event types that are journaled to their action name.
var journalActions = map[EventType]string{
	EventCameraAdded: "add", EventCameraRemoved: "remove",
	EventFaultInjected: "inject", EventFaultCleared: "clear",
}

// publish assigns the next sequence number, records the event in the journal
// and delivers it to subscribers without ever blocking: a subscriber whose
// buffer is full is dropped and its channel closed, so a stalled dashboard
// cannot stall the fleet. Subscribers re-list on reconnect, so a dropped
// subscriber misses nothing it cannot recover.
func (f *Fleet) publish(ev Event) {
	e := &f.ev
	e.mu.Lock()
	defer e.mu.Unlock()
	e.seq++
	ev.Seq = e.seq

	action := journalActions[ev.Type]
	if action != "" {
		var fs FaultSpec
		if ev.Fault != nil {
			fs = *ev.Fault
		}
		e.journal = append(e.journal, JournalEntry{
			Seq: ev.Seq, Action: action, Camera: ev.Camera, Fault: fs, FrameIndex: ev.FrameIndex, Clock: ev.Clock,
		})
	}

	for s := range e.subs {
		select {
		case s.ch <- ev:
		default:
			delete(e.subs, s)
			s.end()
		}
	}
}

// Subscribe returns a channel of fleet changes, closed when ctx ends, when the
// fleet closes, or if the subscriber falls too far behind.
func (f *Fleet) Subscribe(ctx context.Context) <-chan Event {
	s := &subscriber{ch: make(chan Event, subscriberBuffer), done: make(chan struct{})}
	e := &f.ev
	e.mu.Lock()
	if e.closed {
		s.end()
		e.mu.Unlock()
		return s.ch
	}
	e.subs[s] = struct{}{}
	e.mu.Unlock()

	go func() {
		select {
		case <-ctx.Done():
		case <-s.done:
			return
		}
		e.mu.Lock()
		if _, ok := e.subs[s]; ok {
			delete(e.subs, s)
			s.end()
		}
		e.mu.Unlock()
	}()
	return s.ch
}

// closeSubscribers ends every subscription. Called from Close.
func (f *Fleet) closeSubscribers() {
	e := &f.ev
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closed = true
	for s := range e.subs {
		delete(e.subs, s)
		s.end()
	}
}

// Journal returns the runtime changes made so far, oldest first.
func (f *Fleet) Journal() []JournalEntry {
	e := &f.ev
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]JournalEntry(nil), e.journal...)
}

// journalSuffix is appended to the replay line once the journal is non-empty.
func (f *Fleet) journalSuffix() string {
	j := f.Journal()
	if len(j) == 0 {
		return ""
	}
	b, _ := json.Marshal(j)
	sum := sha256.Sum256(b)
	return fmt.Sprintf(" journal=%d:%s", len(j), hex.EncodeToString(sum[:])[:12])
}

// noteFault records a fault change that took effect at index on the given
// clock ("frame" or "request").
func (f *Fleet) noteFault(action, camera string, spec FaultSpec, index int, clock string) {
	ev := Event{Camera: camera, FrameIndex: index, Clock: clock}
	if action == "inject" {
		ev.Type = EventFaultInjected
		ev.Fault = &spec
	} else {
		ev.Type = EventFaultCleared
	}
	f.publish(ev)
}
