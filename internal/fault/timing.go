package fault

// ticksPerMs is the pump's RTP clock rate (90 kHz) per millisecond.
const ticksPerMs = 90

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
