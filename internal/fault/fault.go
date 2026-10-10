// Package fault decides, deterministically, when a camera should misbehave.
//
// It decides only. In this version no decision changes a byte on the wire: the
// seam exists so that the code paths carrying media and control responses are
// written with an interception point from the start, because a seam added later
// would not be reachable from paths built without one.
//
// Every decision is a pure function of the camera's seed and the frame counter.
// Nothing here consults wall-clock time, goroutine order, or map iteration
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

// The catalogued faults. Only the decision half of each exists in this version.
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
	// KindTeardownMidSession closes a live session. Reproduces reconnect logic
	// and resource leaks.
	KindTeardownMidSession Kind = "teardown_mid_session"
	// KindAcceptThenSilence accepts the TCP connection and never answers.
	KindAcceptThenSilence Kind = "accept_then_silence"
	// KindSDPCodecLie advertises one codec and sends another.
	KindSDPCodecLie Kind = "sdp_codec_lie"
	// KindSPSResolutionLie advertises a geometry the frames do not match.
	KindSPSResolutionLie Kind = "sps_resolution_lie"
)

// Errors reported by New.
var (
	ErrUnknownKind = errors.New("fault: unknown kind")
	ErrBadRate     = errors.New("fault: rate must be in [0,1]")
)

// knownKinds is the catalogue. Membership here means the kind can be named and
// decided, not that its effect is implemented.
var knownKinds = map[Kind]bool{
	KindFrameDrop:          true,
	KindPacketLoss:         true,
	KindKeyframeStarvation: true,
	KindTeardownMidSession: true,
	KindAcceptThenSilence:  true,
	KindSDPCodecLie:        true,
	KindSPSResolutionLie:   true,
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
	// Rate is the per-frame probability for rate-based kinds, in [0,1].
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
}

// New returns an Engine for one camera. Each kind draws from its own generator,
// seeded by kind name, so introducing a kind leaves every other kind's sequence
// byte-identical.
func New(s seed.Seed, specs []Spec) (*Engine, error) {
	e := &Engine{}
	for _, sp := range specs {
		if !knownKinds[sp.Kind] {
			return nil, fmt.Errorf("%w: %q", ErrUnknownKind, sp.Kind)
		}
		if sp.Rate < 0 || sp.Rate > 1 {
			return nil, fmt.Errorf("%w: %v for %q", ErrBadRate, sp.Rate, sp.Kind)
		}
		if err := Validate(sp); err != nil {
			return nil, err
		}
		e.entries = append(e.entries, entry{
			spec: sp,
			rnd:  s.Stream(string(sp.Kind)).Rand(),
		})
	}
	return e, nil
}

// Enabled reports whether any fault is configured.
func (e *Engine) Enabled() bool { return len(e.entries) > 0 }

// DecideFrame returns the faults that fire for the given frame counter.
// randomAccess reports whether the access unit at that position is a random
// access point; only ProtectKeyframes consults it.
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
		case KindFrameDrop, KindKeyframeStarvation:
			// Draw unconditionally so the sequence does not depend on earlier
			// outcomes.
			if en.rnd.Float64() >= en.spec.Rate {
				continue
			}
			d := Decision{Kind: en.spec.Kind, FrameIndex: frameIndex}
			if en.spec.Kind == KindFrameDrop && en.spec.ProtectKeyframes && randomAccess {
				d.Suppressed = true
			}
			out = append(out, d)
		default:
			// Session, transport and control-plane faults are not frame-scoped.
			// They are catalogued and validated, and decided elsewhere when
			// their effects land.
		}
	}
	return out
}
