package rtsp

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph264"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph265"
	"github.com/pion/rtp"

	"github.com/0x524a/camfarm/internal/clock"
	"github.com/0x524a/camfarm/internal/fault"
	"github.com/0x524a/camfarm/internal/media"
	"github.com/0x524a/camfarm/internal/obs"
	"github.com/0x524a/camfarm/internal/seed"
)

const (
	detailFrameDropApplied = "applied: access unit not encoded or sent"
	detailSuppressed       = "decided; suppressed by protect_keyframes"
	detailKeyframeWithheld = "keyframe withheld (not encoded)"
	detailPacketLossFmt    = "withheld %d of %d packets"
	detailPacketSparedFmt  = "withheld %d of %d packets; spared %d keyframe packets"
	detailTimestampFmt     = "timestamp shifted by %d ticks"
	detailHeldFmt          = "held (%d of %d)"
	detailReleasedFmt      = "released %d frames"
	detailDelayedFmt       = "delayed %d ms"
)

// faultSwap is a pending replacement of a pump's fault engine.
type faultSwap struct {
	eng     *fault.Engine
	applied func(frameIndex int)
}

// packetWriter is the narrow slice of ServerStream the pump needs.
//
// Writing through an interface keeps pump tests hermetic: they assert exact
// sequence numbers and timestamps with no socket, and no server to start.
type packetWriter interface {
	WritePacketRTP(*description.Media, *rtp.Packet) error
}

// rtpEncoder is the narrow slice of a gortsplib RTP encoder the pump needs.
//
// Both rtph264.Encoder and rtph265.Encoder satisfy it structurally with
// identical signatures, which is why Step needs no codec branch at all:
// everything after Encode -- timestamp arithmetic, the fault seam, the loop
// seam, sequence-number derivation -- is codec-independent. Keeping the branch
// at construction rather than in Step is what makes this change unable to
// regress any determinism guarantee.
type rtpEncoder interface {
	Encode(au [][]byte) ([]*rtp.Packet, error)
}

// PumpConfig configures a Pump.
type PumpConfig struct {
	CameraID string
	Media    *media.Media
	Medi     *description.Media
	Writer   packetWriter
	Seed     seed.Seed
	Faults   []fault.Spec
	Obs      *obs.Recorder
	// AfterFrame, if set, is called at the end of every Step that completes,
	// dropped frames and frames burst_delivery only holds (nothing written)
	// included, so a consumer counts frames by pump step rather
	// than by wall clock. An errored Step does not call it, so it does not count
	// toward a teardown's afterFrames.
	AfterFrame func()
}

// Pump paces one camera's media onto its stream.
//
// It is not safe for concurrent use: one goroutine drives one pump. Step is the
// unit of work, so a deterministic test can call it directly and needs no clock.
type Pump struct {
	id     string
	medi   *description.Media
	w      packetWriter
	enc    rtpEncoder
	aus    []media.AccessUnit
	obs    *obs.Recorder
	faults *fault.Engine

	afterFrame func()

	faultSeed seed.Seed

	// swapMu guards pending. Everything else on a Pump is owned by the one
	// goroutine that calls Step; this queue is the only way another goroutine
	// reaches in, and it is drained between frames.
	swapMu  sync.Mutex
	pending []faultSwap

	// held is burst_delivery's buffer: one entry per held frame, each already
	// stamped and loss-filtered, so a flush only writes. A hold is at most
	// HoldFrames (validated <= 300) frames long and its last frame is not
	// buffered, so memory is bounded by 299 frames' worth of packets per camera.
	//
	// Frames still here when the pump stops are discarded: they were never
	// emitted, so never counted, and nothing writes them after run returns.
	held [][]*rtp.Packet

	initialSeq uint16
	frameDur   int64

	idx          int
	frameCounter int
	loops        uint64
	// tsOffset accumulates across loops so timestamps continue rather than
	// restart. It is int64 and converted on use: RTP timestamps are 32-bit and
	// wrapping is correct behaviour, not an error.
	tsOffset int64
}

// newEncoder builds the seeded RTP encoder for a codec.
func newEncoder(c media.Codec, payloadType uint8, ssrc *uint32, initialSeq *uint16) (rtpEncoder, error) {
	switch c {
	case media.CodecH264:
		enc := &rtph264.Encoder{
			PayloadType:           payloadType,
			PacketizationMode:     1,
			SSRC:                  ssrc,
			InitialSequenceNumber: initialSeq,
		}
		if err := enc.Init(); err != nil {
			return nil, fmt.Errorf("rtsp: initializing H264 RTP encoder: %w", err)
		}
		return enc, nil
	case media.CodecH265:
		// MaxDONDiff is left at its zero value deliberately: Init rejects any
		// other value ("not supported (yet)"), and zero is correct anyway for a
		// single-layer synthetic source with no decoding-order reordering.
		enc := &rtph265.Encoder{
			PayloadType:           payloadType,
			SSRC:                  ssrc,
			InitialSequenceNumber: initialSeq,
		}
		if err := enc.Init(); err != nil {
			return nil, fmt.Errorf("rtsp: initializing H265 RTP encoder: %w", err)
		}
		return enc, nil
	default:
		return nil, fmt.Errorf("rtsp: no RTP encoder for codec %q", c)
	}
}

// NewPump validates cfg and returns a Pump positioned at the first access unit.
func NewPump(cfg PumpConfig) (*Pump, error) {
	switch {
	case cfg.CameraID == "":
		return nil, errors.New("rtsp: pump has no camera ID")
	case cfg.Media == nil:
		return nil, errors.New("rtsp: pump has no media")
	case cfg.Medi == nil:
		return nil, errors.New("rtsp: pump has no media description")
	case cfg.Writer == nil:
		return nil, errors.New("rtsp: pump has no writer")
	case cfg.Obs == nil:
		return nil, errors.New("rtsp: pump has no recorder")
	case len(cfg.Media.AUs) == 0:
		return nil, errors.New("rtsp: pump media has no access units")
	}

	faultSeed := cfg.Seed.Stream("fault")
	eng, err := fault.New(faultSeed, cfg.Faults)
	if err != nil {
		return nil, err
	}

	// Derive SSRC and the initial sequence number from the seed.
	//
	// The sequence number survives to the client and is therefore reproducible.
	// The SSRC does not: ServerStream overwrites it with a per-format value of
	// its own. It is set anyway so that a writer which does not overwrite it --
	// including the test recorder -- still behaves deterministically, and so the
	// intent is legible if upstream ever stops overwriting.
	r := cfg.Seed.Stream("media").Rand()
	ssrc := uint32(r.Uint64())
	initialSeq := uint16(r.Uint64())

	enc, err := newEncoder(cfg.Media.Codec, 96, &ssrc, &initialSeq)
	if err != nil {
		return nil, err
	}

	return &Pump{
		id:         cfg.CameraID,
		medi:       cfg.Medi,
		w:          cfg.Writer,
		enc:        enc,
		aus:        cfg.Media.AUs,
		obs:        cfg.Obs,
		afterFrame: cfg.AfterFrame,
		faults:     eng,
		faultSeed:  faultSeed,
		initialSeq: initialSeq,
		frameDur:   cfg.Media.FrameDuration(),
	}, nil
}

// FrameCounter returns how many access units have been written. It is monotonic
// across loops, which is why fault decisions key on it.
func (p *Pump) FrameCounter() int { return p.frameCounter }

// Loops returns how many complete passes over the source have finished.
func (p *Pump) Loops() uint64 { return p.loops }

// InitialSequenceNumber returns the seed-derived first RTP sequence number.
func (p *Pump) InitialSequenceNumber() uint16 { return p.initialSeq }

// SetFaults replaces this camera's fault set. The specs are validated now; the
// swap itself happens at the start of the next Step, so no frame is
// half-affected. applied, if non-nil, is called from the stepping goroutine
// with the frame index the new set takes effect on. Several calls between two
// frames apply in order.
func (p *Pump) SetFaults(specs []fault.Spec, applied func(frameIndex int)) error {
	eng, err := fault.New(p.faultSeed, specs)
	if err != nil {
		return err
	}
	p.swapMu.Lock()
	p.pending = append(p.pending, faultSwap{eng: eng, applied: applied})
	p.swapMu.Unlock()
	return nil
}

// applyPendingFaults installs queued fault sets and reports whether any was
// installed.
func (p *Pump) applyPendingFaults() bool {
	p.swapMu.Lock()
	swaps := p.pending
	p.pending = nil
	p.swapMu.Unlock()
	for _, s := range swaps {
		p.faults = s.eng
		if s.applied != nil {
			s.applied(p.frameCounter)
		}
	}
	return len(swaps) > 0
}

// writeFrame writes one frame's packets.
func (p *Pump) writeFrame(pkts []*rtp.Packet) error {
	for _, pkt := range pkts {
		if err := p.w.WritePacketRTP(p.medi, pkt); err != nil {
			return fmt.Errorf("rtsp: writing RTP for %q: %w", p.id, err)
		}
	}
	return nil
}

// plannedFrame is one Step's decisions, made and not yet carried out.
type plannedFrame struct {
	// frames are the access units to write, each already stamped and
	// loss-filtered: none when the access unit was dropped or is held, one
	// normally, and a whole burst (held frames, then this one) on a flush.
	frames [][]*rtp.Packet
	// delay is jitter's delay for this frame; zero when none is configured or
	// there is nothing to write. It is relative to the tick being processed.
	delay time.Duration
	// idx is the frame counter the frame was planned at, which events are
	// attributed to.
	idx int
	// released, when non-zero, is the size of the burst this plan flushes.
	released int
}

// emit writes the planned frames in order, counting each as served now that it
// is emitted.
func (p *Pump) emit(pf plannedFrame) error {
	for _, pkts := range pf.frames {
		if err := p.writeFrame(pkts); err != nil {
			return err
		}
		p.obs.Frame(p.id)
	}
	if pf.released > 0 {
		p.obs.Fault(p.id, pf.idx, string(fault.KindBurstDelivery), fmt.Sprintf(detailReleasedFmt, pf.released))
	}
	return nil
}

// takeHeld empties the hold buffer and returns its frames. Emptying first means
// a write error cannot cause a frame to be written twice.
func (p *Pump) takeHeld() [][]*rtp.Packet {
	frames := p.held
	p.held = nil
	return frames
}

// Step writes exactly one access unit, or holds it for burst_delivery. It plans
// the frame and emits it at once: jitter's delay is a pacing concern of run, so
// a caller driving Step directly never waits. AfterFrame is called once the
// frame is handled, held and dropped frames included, and not on error.
func (p *Pump) Step() error {
	pf, err := p.plan()
	if err != nil {
		return err
	}
	if err := p.emit(pf); err != nil {
		return err
	}
	p.done()
	return nil
}

// done reports a completed frame to AfterFrame.
func (p *Pump) done() {
	if p.afterFrame != nil {
		p.afterFrame()
	}
}

// plan makes every decision for one access unit, in this order: pending fault
// swap, frame faults (drop, starvation), encode, packet loss, timestamp faults,
// burst hold or flush, then jitter. Per-frame draws are made unconditionally,
// for dropped and held frames too, so each sequence depends only on how many
// frames have passed. It advances the pump to the next access unit and writes
// nothing, except a hold released by a fault-set swap.
func (p *Pump) plan() (plannedFrame, error) {
	if p.applyPendingFaults() && len(p.held) > 0 {
		// A new fault set starts with no hold running, so a hold in progress
		// cannot continue. Release its frames now, before this frame, rather than
		// lose them or leave them for some later unrelated burst. They are written
		// at once, undelayed, as the old set's last act.
		frames := p.takeHeld()
		if err := p.emit(plannedFrame{frames: frames, idx: p.frameCounter, released: len(frames)}); err != nil {
			return plannedFrame{}, err
		}
	}
	au := p.aus[p.idx]

	// Timestamp faults are consulted once per Step, before the drop check, so a
	// frame that is removed still advances their counters.
	shift := p.faults.TimestampShift(p.frameCounter, p.frameDur)

	// burst_delivery likewise decides for every frame, dropped or not, so its
	// draws stay frame-clocked.
	hold := p.faults.DecideHold(p.frameCounter)

	// The fault seam. frame_drop and keyframe_starvation remove the frame before
	// it is encoded, so they consume no packet_loss draws.
	drop := false
	for _, d := range p.faults.DecideFrame(p.frameCounter, au.RandomAccess) {
		kind := string(d.Kind)
		switch {
		case d.Suppressed:
			p.obs.Fault(p.id, d.FrameIndex, kind, detailSuppressed)
			p.obs.FaultTally(p.id, kind, 0, 1)
		case d.Kind == fault.KindFrameDrop:
			p.obs.Fault(p.id, d.FrameIndex, kind, detailFrameDropApplied)
			p.obs.FaultTally(p.id, kind, 1, 0)
			drop = true
		case d.Kind == fault.KindKeyframeStarvation:
			p.obs.Fault(p.id, d.FrameIndex, kind, detailKeyframeWithheld)
			p.obs.FaultTally(p.id, kind, 1, 0)
			drop = true
		}
	}

	var out []*rtp.Packet
	if !drop {
		pkts, err := p.enc.Encode(au.NALUs)
		if err != nil {
			return plannedFrame{}, fmt.Errorf("rtsp: encoding access unit %d of %q: %w", p.idx, p.id, err)
		}

		// packet_loss: the frame is encoded (so the packetizer assigns
		// sequence numbers) and individual packets are then not written,
		// which is what leaves a real gap.
		skip := make([]bool, len(pkts))
		withheld, spared := 0, 0
		for _, d := range p.faults.DecidePackets(p.frameCounter, len(pkts), au.RandomAccess) {
			if d.Suppressed {
				spared++
			} else if !skip[d.Index] {
				skip[d.Index] = true
				withheld++
			}
		}
		if withheld > 0 || spared > 0 {
			detail := fmt.Sprintf(detailPacketLossFmt, withheld, len(pkts))
			if spared > 0 {
				detail = fmt.Sprintf(detailPacketSparedFmt, withheld, len(pkts), spared)
			}
			p.obs.Fault(p.id, p.frameCounter, string(fault.KindPacketLoss), detail)
			p.obs.FaultTally(p.id, string(fault.KindPacketLoss), uint64(withheld), uint64(spared))
		}

		// uint32 of a negative int64 wraps, which is what a camera whose clock
		// is behind zero would send.
		ts := uint32(p.tsOffset + au.DTS + shift)
		if shift != 0 {
			detail := fmt.Sprintf(detailTimestampFmt, shift)
			for _, k := range p.faults.TimestampKinds() {
				p.obs.Fault(p.id, p.frameCounter, string(k), detail)
				p.obs.FaultTally(p.id, string(k), 1, 0)
			}
		}
		for i, pkt := range pkts {
			if skip[i] {
				continue
			}
			pkt.Timestamp = ts
			out = append(out, pkt)
		}
	}

	// burst_delivery: held frames are already stamped and loss-filtered, so only
	// their delivery moves. obs.Frame counts a frame when it is emitted, which
	// for a held frame is the flush. A dropped frame has nothing to hold, but its
	// decision still advances the hold.
	pf := plannedFrame{idx: p.frameCounter}
	switch hold {
	case fault.HoldHeld:
		if !drop {
			p.held = append(p.held, out)
			p.obs.Fault(p.id, p.frameCounter, string(fault.KindBurstDelivery),
				fmt.Sprintf(detailHeldFmt, len(p.held), p.faults.HoldFrames()))
			p.obs.FaultTally(p.id, string(fault.KindBurstDelivery), 1, 0)
		}
	case fault.HoldFlush:
		pf.frames = p.takeHeld()
		if !drop {
			pf.frames = append(pf.frames, out)
		}
		pf.released = len(pf.frames)
	default:
		if !drop {
			pf.frames = [][]*rtp.Packet{out}
		}
	}

	// jitter is drawn for every frame. A frame with nothing to write (dropped or
	// held) discards its draw: a held frame leaves with its burst, whose last
	// frame carries that frame's own delay.
	if d := p.faults.FrameDelay(p.frameCounter); len(pf.frames) > 0 {
		pf.delay = d
	}

	p.frameCounter++
	p.idx++

	if p.idx == len(p.aus) {
		// Loop seam: carry the timestamp forward by one frame past the last
		// access unit, so looping the fixture is indistinguishable from a
		// continuous source. Restarting at zero here would fabricate the
		// timestamp-discontinuity fault that is supposed to be opt-in.
		p.tsOffset += au.DTS + p.frameDur
		p.idx = 0
		p.loops++
		p.obs.Loop(p.id)
	}
	return pf, nil
}

// interval returns the wall-clock spacing between access units.
func (p *Pump) interval() time.Duration {
	const clockRate = 90000
	if p.frameDur <= 0 {
		return time.Second / 30
	}
	return time.Duration(p.frameDur) * time.Second / clockRate
}

// run drives the pump until ctx is cancelled.
//
// A write error is logged by the caller and does not stop the pump: a reader
// disconnecting mid-write must not take the camera down.
//
// Each tick plans a frame and, if jitter delayed it, waits until tickTime+delay
// before emitting; tickTime is the time of the tick being processed, not the
// moment the wait began. The loop is sequential, so a frame is never emitted
// earlier than the previous one: order is kept, and a late frame makes the ones
// behind it bunch up (the ticker holds one pending tick). AfterFrame fires
// after the delayed emit.
func (p *Pump) run(ctx context.Context, ck clock.Clock, onErr func(error)) {
	tk := ck.NewTicker(p.interval())
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case tickTime := <-tk.C():
			pf, err := p.plan()
			if err != nil {
				if onErr != nil {
					onErr(err)
				}
				continue
			}
			if pf.delay > 0 {
				p.obs.Fault(p.id, pf.idx, string(fault.KindJitter), fmt.Sprintf(detailDelayedFmt, pf.delay.Milliseconds()))
				p.obs.FaultTally(p.id, string(fault.KindJitter), 1, 0)
				select {
				case <-ck.After(max(0, tickTime.Add(pf.delay).Sub(ck.Now()))):
				case <-ctx.Done():
					return
				}
			}
			if err := p.emit(pf); err != nil {
				if onErr != nil {
					onErr(err)
				}
				continue
			}
			p.done()
		}
	}
}
