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

	faultSeed seed.Seed

	// swapMu guards pending. Everything else on a Pump is owned by the one
	// goroutine that calls Step; this queue is the only way another goroutine
	// reaches in, and it is drained between frames.
	swapMu  sync.Mutex
	pending []faultSwap

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

func (p *Pump) applyPendingFaults() {
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
}

// Step writes exactly one access unit.
func (p *Pump) Step() error {
	p.applyPendingFaults()
	au := p.aus[p.idx]

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

	if !drop {
		pkts, err := p.enc.Encode(au.NALUs)
		if err != nil {
			return fmt.Errorf("rtsp: encoding access unit %d of %q: %w", p.idx, p.id, err)
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

		ts := uint32(p.tsOffset + au.DTS)
		for i, pkt := range pkts {
			if skip[i] {
				continue
			}
			pkt.Timestamp = ts
			if err := p.w.WritePacketRTP(p.medi, pkt); err != nil {
				return fmt.Errorf("rtsp: writing RTP for %q: %w", p.id, err)
			}
		}
		p.obs.Frame(p.id)
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
	return nil
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
func (p *Pump) run(ctx context.Context, ck clock.Clock, onErr func(error)) {
	tk := ck.NewTicker(p.interval())
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C():
			if err := p.Step(); err != nil && onErr != nil {
				onErr(err)
			}
		}
	}
}
