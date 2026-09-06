package rtsp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph264"
	"github.com/pion/rtp"

	"github.com/0x524a/camfarm/internal/clock"
	"github.com/0x524a/camfarm/internal/fault"
	"github.com/0x524a/camfarm/internal/media"
	"github.com/0x524a/camfarm/internal/obs"
	"github.com/0x524a/camfarm/internal/seed"
)

// detailDecidedNotApplied is recorded against a fault that was decided but whose
// effect this version does not implement. It must read as deliberate, because a
// camera that is lying on purpose and a camera that is broken being
// indistinguishable would make this tool useless.
const detailDecidedNotApplied = "decided; effect not implemented in this version"

// packetWriter is the narrow slice of ServerStream the pump needs.
//
// Writing through an interface keeps pump tests hermetic: they assert exact
// sequence numbers and timestamps with no socket, and no server to start.
type packetWriter interface {
	WritePacketRTP(*description.Media, *rtp.Packet) error
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
	enc    *rtph264.Encoder
	aus    []media.AccessUnit
	obs    *obs.Recorder
	faults *fault.Engine

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

// NewPump validates cfg and returns a Pump positioned at the first access unit.
func NewPump(cfg PumpConfig) (*Pump, error) {
	switch {
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

	eng, err := fault.New(cfg.Seed.Stream("fault"), cfg.Faults)
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

	enc := &rtph264.Encoder{
		PayloadType:           96,
		PacketizationMode:     1,
		SSRC:                  &ssrc,
		InitialSequenceNumber: &initialSeq,
	}
	if err := enc.Init(); err != nil {
		return nil, fmt.Errorf("rtsp: initializing RTP encoder: %w", err)
	}

	return &Pump{
		id:         cfg.CameraID,
		medi:       cfg.Medi,
		w:          cfg.Writer,
		enc:        enc,
		aus:        cfg.Media.AUs,
		obs:        cfg.Obs,
		faults:     eng,
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

// Step writes exactly one access unit.
func (p *Pump) Step() error {
	au := p.aus[p.idx]

	// The fault seam. Decisions are recorded; none of them changes what is
	// written in this version.
	for _, d := range p.faults.DecideFrame(p.frameCounter) {
		p.obs.Fault(p.id, d.FrameIndex, string(d.Kind), detailDecidedNotApplied)
	}

	pkts, err := p.enc.Encode(au.NALUs)
	if err != nil {
		return fmt.Errorf("rtsp: encoding access unit %d of %q: %w", p.idx, p.id, err)
	}

	ts := uint32(p.tsOffset + au.DTS)
	for _, pkt := range pkts {
		pkt.Timestamp = ts
		if err := p.w.WritePacketRTP(p.medi, pkt); err != nil {
			return fmt.Errorf("rtsp: writing RTP for %q: %w", p.id, err)
		}
	}

	p.obs.Frame(p.id)
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
