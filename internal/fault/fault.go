// Package fault decides, deterministically, when a camera should misbehave.
//
// It decides only; callers apply the effects. Every decision is a pure function
// of the camera's seed and a position counter: the frame counter for frame-level
// kinds, and the packet index (DecidePackets) for packet_loss. Nothing here consults wall-clock time, goroutine order, or map iteration
// order.
package fault

import (
	"errors"
	"fmt"
	"math/rand/v2"

	"github.com/0x524a/camfarm/internal/seed"
)

// Kind identifies a fault. Each names a class of real field bug; see the
// architecture design, section 7.
type Kind string

// The catalogued faults. See Implemented for which have an effect today.
const (
	// KindFrameDrop drops access units at a rate. Reproduces decoders and
	// trackers that assume contiguous frames.
	KindFrameDrop Kind = "frame_drop"
	// KindPacketLoss withholds individual RTP packets of an encoded frame,
	// leaving real sequence-number gaps with honest timestamps. Reproduces
	// lossy links and receivers that mishandle gaps.
	KindPacketLoss Kind = "packet_loss"
	// KindKeyframeStarvation withholds IDRs, so a client joining mid-stream
	// never gets one. The "works on camera restart, breaks on reconnect" bug.
	KindKeyframeStarvation Kind = "keyframe_starvation"
	// KindRTSPError answers some DESCRIBE, SETUP or PLAY requests with an RTSP
	// error status. Reproduces flaky cameras and retry logic.
	KindRTSPError Kind = "rtsp_error"
	// KindTeardown closes a live session after a number of frames. Reproduces
	// reconnect logic and resource leaks.
	KindTeardown Kind = "teardown"
	// KindSDPLie makes DESCRIBE misdescribe the stream.
	KindSDPLie Kind = "sdp_lie"
	// KindAcceptThenSilence accepts the TCP connection and never answers.
	KindAcceptThenSilence Kind = "accept_then_silence"
)

// Errors reported by New.
var (
	ErrUnknownKind = errors.New("fault: unknown kind")
	ErrBadRate     = errors.New("fault: rate must be in [0,1]")
	// ErrDuplicateKind reports a kind named twice for one camera.
	ErrDuplicateKind = errors.New("fault: kind repeated")
)

// knownKinds is the catalogue. Membership here means the kind can be named and
// decided, not that its effect is implemented.
var knownKinds = map[Kind]bool{
	KindFrameDrop:          true,
	KindPacketLoss:         true,
	KindKeyframeStarvation: true,
	KindRTSPError:          true,
	KindTeardown:           true,
	KindSDPLie:             true,
	KindAcceptThenSilence:  true,
}

// Known reports whether kind is catalogued.
func Known(kind Kind) bool { return knownKinds[kind] }

// Implemented reports whether a kind's effect exists in this version. Only
// implemented kinds are accepted by the public API; the rest stay refused.
func Implemented(kind Kind) bool {
	info, ok := Info(kind)
	return ok && info.Implemented
}

// Spec configures one fault.
type Spec struct {
	Kind Kind
	// Rate is the probability in [0,1] that the fault fires: per frame for
	// frame_drop, per packet for packet_loss.
	Rate float64
	// ProtectKeyframes, for frame_drop, suppresses a drop that would remove a
	// random-access access unit. The draw is still consumed, so the sequence
	// does not depend on which frames happen to be keyframes.
	ProtectKeyframes bool
	// MeanBurst, for packet_loss, selects burst loss when greater than 1: the
	// mean length, in packets, of a run of consecutive losses. Zero means
	// independent loss.
	MeanBurst float64
	// KeepEvery, for keyframe_starvation, delivers only every Nth
	// random-access access unit (the first is always delivered).
	KeepEvery int
	// On, for rtsp_error, names the method that fails: describe, setup, play
	// or all.
	On string
	// Status, for rtsp_error, is the RTSP status code of the error reply.
	Status int
	// OnlyRequest, for rtsp_error and sdp_lie, fires on exactly the Nth
	// matching request and ignores Rate. Zero means unset.
	OnlyRequest int
	// AfterFrames, for teardown, is the number of frames delivered before the
	// session is torn down.
	AfterFrames int
	// Mode selects a variant: close or reset for teardown; codec, sprop,
	// resolution or contentBase for sdp_lie.
	Mode string
}

// Decision is a fault that fired.
type Decision struct {
	Kind       Kind
	FrameIndex int
	// Suppressed reports that the fault fired but must not be applied (for
	// example a frame_drop on a keyframe with ProtectKeyframes set).
	Suppressed bool
}

// Engine makes fault decisions for one camera.
//
// It is not safe for concurrent use. Each camera owns one, driven by that
// camera's pump, so nothing is shared and no lock is needed.
type Engine struct {
	entries []entry
}

type entry struct {
	spec Spec
	rnd  *rand.Rand

	// seenRandomAccess counts random-access units seen by keyframe_starvation.
	seenRandomAccess int
	// bad is packet_loss burst state; p and q are its transition
	// probabilities (good->bad and bad->bad).
	bad  bool
	p, q float64
}

// New returns an Engine for one camera. Each kind draws from its own generator,
// seeded by kind name, so introducing a kind leaves every other kind's sequence
// byte-identical.
func New(s seed.Seed, specs []Spec) (*Engine, error) {
	e := &Engine{}
	seen := map[Kind]bool{}
	for _, sp := range specs {
		if !knownKinds[sp.Kind] {
			return nil, fmt.Errorf("%w: %q", ErrUnknownKind, sp.Kind)
		}
		if seen[sp.Kind] {
			return nil, fmt.Errorf("%w: %q", ErrDuplicateKind, sp.Kind)
		}
		seen[sp.Kind] = true
		// Validate skips parameter checks for unimplemented kinds, so the rate
		// range is checked here too, to refuse a bad rate on any kind.
		if sp.Rate < 0 || sp.Rate > 1 {
			return nil, fmt.Errorf("%w: %v for %q", ErrBadRate, sp.Rate, sp.Kind)
		}
		if err := Validate(sp); err != nil {
			return nil, err
		}
		en := entry{spec: sp, rnd: s.Stream(string(sp.Kind)).Rand()}
		if sp.Kind == KindPacketLoss && sp.MeanBurst > 1 {
			// Two-state loss model: long-run loss equals Rate and the mean
			// run of losses equals MeanBurst. Validate guarantees p <= 1.
			en.q = 1 - 1/sp.MeanBurst
			en.p = sp.Rate * (1 - en.q) / (1 - sp.Rate)
		}
		e.entries = append(e.entries, en)
	}
	return e, nil
}

// Enabled reports whether any fault is configured.
func (e *Engine) Enabled() bool { return len(e.entries) > 0 }

// DecideFrame returns the faults that fire for the given frame counter.
// randomAccess reports whether the access unit at that position is a random
// access point; ProtectKeyframes and keyframe_starvation consult it. It must be
// called exactly once per frame, since the engine counts random-access units.
//
// The counter is monotonic across loops of the source media, so a decision is
// tied to a position in the run rather than to a position in the file.
//
// Entries are walked in configured order, never map order, so the draw sequence
// is reproducible.
func (e *Engine) DecideFrame(frameIndex int, randomAccess bool) []Decision {
	var out []Decision
	for i := range e.entries {
		en := &e.entries[i]
		switch en.spec.Kind {
		case KindFrameDrop:
			// Draw unconditionally so the sequence does not depend on earlier
			// outcomes.
			if en.rnd.Float64() >= en.spec.Rate {
				continue
			}
			d := Decision{Kind: en.spec.Kind, FrameIndex: frameIndex}
			if en.spec.ProtectKeyframes && randomAccess {
				d.Suppressed = true
			}
			out = append(out, d)
		case KindKeyframeStarvation:
			// No randomness: the k-th random-access unit is delivered when
			// k mod KeepEvery == 0, so the first one always is.
			if !randomAccess {
				continue
			}
			k := en.seenRandomAccess
			en.seenRandomAccess++
			if k%en.spec.KeepEvery != 0 {
				out = append(out, Decision{Kind: en.spec.Kind, FrameIndex: frameIndex})
			}
		default:
			// Packet loss is decided per packet (DecidePackets); session,
			// transport and control-plane faults are not frame-scoped.
		}
	}
	return out
}

// PacketDecision is one packet a packet_loss fault fires on.
type PacketDecision struct {
	// Index is the packet's position in the frame's packet list.
	Index int
	// Suppressed reports that the loss fired but must not be applied (the
	// packet belongs to a random-access frame and ProtectKeyframes is set).
	Suppressed bool
}

// DecidePackets returns the packets of one encoded frame that a packet_loss
// fault fires on. Call it once per encoded frame, after DecideFrame: a frame
// that was never encoded has no packets and consumes no draws.
//
// Exactly one draw is consumed per packet, unconditionally, so the sequence
// depends only on the packet count so far, never on earlier outcomes. The first
// parameter (the frame index) is unused: decisions are keyed on packets drawn,
// not on frames.
func (e *Engine) DecidePackets(_, count int, randomAccess bool) []PacketDecision {
	var out []PacketDecision
	for i := range e.entries {
		en := &e.entries[i]
		if en.spec.Kind != KindPacketLoss {
			continue
		}
		for j := 0; j < count; j++ {
			u := en.rnd.Float64()
			var lost bool
			if en.spec.MeanBurst > 1 {
				if en.bad {
					en.bad = u < en.q
				} else {
					en.bad = u < en.p
				}
				lost = en.bad
			} else {
				lost = u < en.spec.Rate
			}
			if lost {
				out = append(out, PacketDecision{Index: j, Suppressed: en.spec.ProtectKeyframes && randomAccess})
			}
		}
	}
	return out
}
