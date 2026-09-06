package rtsp

import (
	"testing"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/pion/rtp"

	"github.com/0x524a/camfarm/internal/fault"
	"github.com/0x524a/camfarm/internal/obs"
	"github.com/0x524a/camfarm/internal/seed"
)

// recorder captures what a pump wrote, so pump tests need no socket.
type recorder struct {
	pkts []rtp.Packet
}

func (r *recorder) WritePacketRTP(_ *description.Media, pkt *rtp.Packet) error {
	r.pkts = append(r.pkts, *pkt)
	return nil
}

func newTestPump(t *testing.T, s seed.Seed, faults []fault.Spec, rec *obs.Recorder) (*Pump, *recorder) {
	t.Helper()
	w := &recorder{}
	p, err := NewPump(PumpConfig{
		CameraID: "cam",
		Media:    testMedia(t),
		Medi:     &description.Media{Type: description.MediaTypeVideo},
		Writer:   w,
		Seed:     s,
		Faults:   faults,
		Obs:      rec,
	})
	if err != nil {
		t.Fatalf("NewPump: %v", err)
	}
	return p, w
}

func TestStepWritesOneAccessUnit(t *testing.T) {
	rec := obs.New(10)
	p, w := newTestPump(t, seed.Seed(1), nil, rec)

	if err := p.Step(); err != nil {
		t.Fatalf("Step: %v", err)
	}
	if len(w.pkts) == 0 {
		t.Fatal("Step wrote no packets")
	}
	if got := p.FrameCounter(); got != 1 {
		t.Errorf("FrameCounter = %d, want 1", got)
	}
	if got := rec.Counters("cam").FramesServed; got != 1 {
		t.Errorf("FramesServed = %d, want 1", got)
	}
	// One access unit shares one timestamp across however many packets it
	// fragments into.
	ts := w.pkts[0].Timestamp
	for i, pkt := range w.pkts {
		if pkt.Timestamp != ts {
			t.Fatalf("packet %d timestamp %d differs from %d within one access unit", i, pkt.Timestamp, ts)
		}
	}
	// The final packet of an access unit carries the marker bit.
	if !w.pkts[len(w.pkts)-1].Marker {
		t.Error("last packet of the access unit has no marker bit")
	}
}

// Spec section 8.3 as corrected: the initial sequence number IS reproducible.
func TestSequenceNumbersAreSeedDeterministic(t *testing.T) {
	collect := func() []uint16 {
		p, w := newTestPump(t, seed.Seed(0x3f2a9c81).Camera(2), nil, obs.New(10))
		for i := 0; i < 10; i++ {
			if err := p.Step(); err != nil {
				t.Fatalf("Step: %v", err)
			}
		}
		out := make([]uint16, len(w.pkts))
		for i, pkt := range w.pkts {
			out[i] = pkt.SequenceNumber
		}
		return out
	}

	a, b := collect(), collect()
	if len(a) != len(b) {
		t.Fatalf("packet counts differ: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("sequence number %d differs: %d vs %d", i, a[i], b[i])
		}
	}

	// Different cameras must not start at the same sequence number.
	root := seed.Seed(0x3f2a9c81)
	p0, _ := newTestPump(t, root.Camera(0), nil, obs.New(1))
	p1, _ := newTestPump(t, root.Camera(1), nil, obs.New(1))
	if p0.InitialSequenceNumber() == p1.InitialSequenceNumber() {
		t.Error("two cameras share an initial sequence number")
	}
}

// Spec section 10.4: on loop the pump continues timestamps, so a looping fixture
// does not accidentally resemble the timestamp-discontinuity fault.
func TestLoopContinuesTimestamps(t *testing.T) {
	m := testMedia(t)
	rec := obs.New(10)
	p, w := newTestPump(t, seed.Seed(1), nil, rec)

	// Two full passes plus one frame, so the seam is crossed and passed.
	for i := 0; i < 2*len(m.AUs)+1; i++ {
		if err := p.Step(); err != nil {
			t.Fatalf("Step %d: %v", i, err)
		}
	}

	if got := p.Loops(); got != 2 {
		t.Errorf("Loops = %d, want 2", got)
	}
	if got := rec.Counters("cam").LoopsCompleted; got != 2 {
		t.Errorf("LoopsCompleted = %d, want 2", got)
	}

	// Collapse packets to one timestamp per access unit, in order.
	var ts []uint32
	for _, pkt := range w.pkts {
		if len(ts) == 0 || ts[len(ts)-1] != pkt.Timestamp {
			ts = append(ts, pkt.Timestamp)
		}
	}
	if len(ts) != 2*len(m.AUs)+1 {
		t.Fatalf("distinct timestamps = %d, want %d", len(ts), 2*len(m.AUs)+1)
	}

	// Timestamps must never go backwards.
	for i := 1; i < len(ts); i++ {
		if ts[i] <= ts[i-1] {
			t.Fatalf("timestamp went backwards at %d: %d then %d", i, ts[i-1], ts[i])
		}
	}

	// Across the seam the step must be exactly one frame duration -- that is what
	// "no discontinuity" means here.
	seam := len(m.AUs)
	if got := int64(ts[seam]) - int64(ts[seam-1]); got != m.FrameDuration() {
		t.Errorf("seam delta = %d, want %d (one frame duration)", got, m.FrameDuration())
	}
}

// The seam is real: a configured fault is decided and recorded on every frame,
// while deliberately changing nothing on the wire in this version.
func TestFaultDecisionsAreRecordedButNotApplied(t *testing.T) {
	rec := obs.New(1000)
	// Rate 1 fires on every frame, which makes the assertion unambiguous.
	p, w := newTestPump(t, seed.Seed(5), []fault.Spec{{Kind: fault.KindFrameDrop, Rate: 1}}, rec)

	const frames = 5
	for i := 0; i < frames; i++ {
		if err := p.Step(); err != nil {
			t.Fatalf("Step: %v", err)
		}
	}

	if got := rec.Counters("cam").FaultsFired; got != frames {
		t.Fatalf("FaultsFired = %d, want %d", got, frames)
	}
	// Effect deliberately absent: every frame was still written.
	if got := rec.Counters("cam").FramesServed; got != frames {
		t.Fatalf("FramesServed = %d, want %d -- a fault must not yet drop anything", got, frames)
	}
	if len(w.pkts) == 0 {
		t.Fatal("no packets written")
	}
	ev := rec.Events()
	if len(ev) != frames {
		t.Fatalf("events = %d, want %d", len(ev), frames)
	}
	if ev[0].Kind != string(fault.KindFrameDrop) {
		t.Errorf("event kind = %q", ev[0].Kind)
	}
	if ev[0].FrameIndex != 0 || ev[frames-1].FrameIndex != frames-1 {
		t.Errorf("frame indices = %d..%d, want 0..%d", ev[0].FrameIndex, ev[frames-1].FrameIndex, frames-1)
	}
}

func TestNewPumpRejectsBadConfig(t *testing.T) {
	m := testMedia(t)
	medi := &description.Media{Type: description.MediaTypeVideo}
	good := PumpConfig{CameraID: "c", Media: m, Medi: medi, Writer: &recorder{}, Obs: obs.New(1)}

	bad := map[string]func(PumpConfig) PumpConfig{
		"no media":  func(c PumpConfig) PumpConfig { c.Media = nil; return c },
		"no writer": func(c PumpConfig) PumpConfig { c.Writer = nil; return c },
		"no obs":    func(c PumpConfig) PumpConfig { c.Obs = nil; return c },
		"no medi":   func(c PumpConfig) PumpConfig { c.Medi = nil; return c },
		"bad fault": func(c PumpConfig) PumpConfig {
			c.Faults = []fault.Spec{{Kind: "nope", Rate: 1}}
			return c
		},
	}
	for name, mutate := range bad {
		if _, err := NewPump(mutate(good)); err == nil {
			t.Errorf("%s: NewPump succeeded, want an error", name)
		}
	}
}
