package fault

import "time"

// ticksPerMs is the pump's RTP clock rate (90 kHz) per millisecond.
const ticksPerMs = 90

// HoldDecision is burst_delivery's verdict for one frame.
type HoldDecision int

// The verdicts DecideHold returns.
const (
	// HoldNone: deliver the frame normally.
	HoldNone HoldDecision = iota
	// HoldHeld: withhold the frame; it is released with the burst.
	HoldHeld
	// HoldFlush: the last frame of a hold. Release everything held, then this
	// frame, back-to-back.
	HoldFlush
)

// HoldFrames returns the configured burst_delivery hold length, or zero when
// the fault is not configured.
func (e *Engine) HoldFrames() int {
	for i := range e.entries {
		if e.entries[i].spec.Kind == KindBurstDelivery {
			return e.entries[i].spec.HoldFrames
		}
	}
	return 0
}

// DecideHold returns burst_delivery's verdict for the frame. Call it exactly
// once per frame, including frames another fault removes.
//
// One draw is consumed per frame, unconditionally and during a hold too, so the
// sequence depends only on how many frames have passed, never on earlier
// outcomes or on HoldFrames. When no hold is running, a draw below Rate starts
// one of HoldFrames frames including this frame: the first HoldFrames-1 return
// HoldHeld and the HoldFrames-th returns HoldFlush.
func (e *Engine) DecideHold(_ int) HoldDecision {
	for i := range e.entries {
		en := &e.entries[i]
		if en.spec.Kind != KindBurstDelivery {
			continue
		}
		u := en.rnd.Float64()
		if en.remaining == 0 {
			if u >= en.spec.Rate {
				return HoldNone
			}
			en.remaining = en.spec.HoldFrames
		}
		en.remaining--
		if en.remaining == 0 {
			return HoldFlush
		}
		return HoldHeld
	}
	return HoldNone
}

// TimestampKinds returns the configured timestamp faults, in configured order,
// so a caller can attribute a non-zero TimestampShift.
func (e *Engine) TimestampKinds() []Kind {
	var out []Kind
	for i := range e.entries {
		if k := e.entries[i].spec.Kind; k == KindPTSDTSMismatch || k == KindPTSDelayed {
			out = append(out, k)
		}
	}
	return out
}

// TimestampShift returns the RTP-tick offset to add to the frame's nominal
// timestamp, summed over every configured timestamp fault. frameDur is the
// nominal frame spacing in ticks.
//
// Call it exactly once per frame, including frames another fault removes, so
// the counters stay frame-clocked. It uses no randomness: the result depends
// only on frameIndex, frameDur and how many times this entry has been asked.
//
// pts_dts_mismatch: with window W = ReorderDepth+1, the first frame of each
// window is stamped (W-1) frames late and the others one frame early, so each
// window of W timestamps is a permutation of its own nominal values.
//
// pts_delayed: a constant lag of DelayMs, plus a lag that grows by DriftPpm of
// a frame per frame since the entry was installed (not since frame zero, so a
// fault added mid-run starts at no drift). Integer arithmetic only, multiplying
// before dividing.
func (e *Engine) TimestampShift(frameIndex int, frameDur int64) int64 {
	var shift int64
	for i := range e.entries {
		en := &e.entries[i]
		switch en.spec.Kind {
		case KindPTSDTSMismatch:
			w := en.spec.ReorderDepth + 1
			if frameIndex%w == 0 {
				shift += int64(w-1) * frameDur
			} else {
				shift -= frameDur
			}
		case KindPTSDelayed:
			n := int64(en.calls)
			en.calls++
			shift -= int64(en.spec.DelayMs) * ticksPerMs
			shift -= n * frameDur * int64(en.spec.DriftPpm) / 1_000_000
		default:
		}
	}
	return shift
}

// FrameDelay returns jitter's delay for the frame, or zero when jitter is not
// configured. Call it exactly once per frame, including frames another fault
// removes and frames burst_delivery holds, so the sequence depends only on how
// many frames have passed.
//
// The delay is uniform in [0, MaxMs) milliseconds, truncated to whole
// microseconds so float noise cannot change a replayed schedule.
func (e *Engine) FrameDelay(_ int) time.Duration {
	for i := range e.entries {
		en := &e.entries[i]
		if en.spec.Kind != KindJitter {
			continue
		}
		u := en.rnd.Float64()
		return time.Duration(u * float64(en.spec.MaxMs) * float64(time.Millisecond)).Truncate(time.Microsecond)
	}
	return 0
}
