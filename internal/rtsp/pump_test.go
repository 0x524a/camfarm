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

// A catalogued fault whose effect does not exist yet is decided and recorded,
// and changes nothing on the wire.
func TestUnimplementedFaultIsRecordedButNotApplied(t *testing.T) {
	t.Skip("no unimplemented kind is frame-decided any more (keyframe_starvation now needs KeepEvery); the slice A plan replaces this test")
	rec := obs.New(1000)
	p, w := newTestPump(t, seed.Seed(5), []fault.Spec{{Kind: fault.KindTeardownMidSession, Rate: 1}}, rec)

	const frames = 5
	for i := 0; i < frames; i++ {
		if err := p.Step(); err != nil {
			t.Fatalf("Step: %v", err)
		}
	}
	if got := rec.Counters("cam").FaultsFired; got != frames {
		t.Fatalf("FaultsFired = %d, want %d", got, frames)
	}
	if got := rec.Counters("cam").FramesServed; got != frames {
		t.Fatalf("FramesServed = %d, want %d -- an unimplemented fault must not drop anything", got, frames)
	}
	if len(w.pkts) == 0 {
		t.Fatal("no packets written")
	}
	if ev := rec.Events(); ev[0].Detail != detailDecidedNotApplied {
		t.Errorf("detail = %q, want %q", ev[0].Detail, detailDecidedNotApplied)
	}
}

func TestFrameDropAtRateOneWritesNothingButAdvances(t *testing.T) {
	rec := obs.New(1000)
	p, w := newTestPump(t, seed.Seed(5), []fault.Spec{{Kind: fault.KindFrameDrop, Rate: 1}}, rec)

	const frames = 6
	for i := 0; i < frames; i++ {
		if err := p.Step(); err != nil {
			t.Fatalf("Step: %v", err)
		}
	}
	if len(w.pkts) != 0 {
		t.Fatalf("wrote %d packets, want 0 when every frame is dropped", len(w.pkts))
	}
	if got := rec.Counters("cam").FramesServed; got != 0 {
		t.Errorf("FramesServed = %d, want 0", got)
	}
	if got := rec.Counters("cam").FaultsFired; got != frames {
		t.Errorf("FaultsFired = %d, want %d", got, frames)
	}
	if got := p.FrameCounter(); got != frames {
		t.Errorf("FrameCounter = %d, want %d (a drop must not stall the position)", got, frames)
	}
	if ev := rec.Events(); ev[0].Detail != detailFrameDropApplied {
		t.Errorf("detail = %q, want %q", ev[0].Detail, detailFrameDropApplied)
	}
}

// A drop is encoder-side: RTP sequence numbers stay contiguous and the missing
// frame shows up only as a timestamp jump.
func TestFrameDropKeepsSequenceContiguousAndJumpsTimestamps(t *testing.T) {
	m := testMedia(t)
	rec := obs.New(1000)
	p, w := newTestPump(t, seed.Seed(0xD20F), []fault.Spec{{Kind: fault.KindFrameDrop, Rate: 0.5}}, rec)

	steps := len(m.AUs) // stay inside one loop so timestamps are the raw DTS
	for i := 0; i < steps; i++ {
		if err := p.Step(); err != nil {
			t.Fatalf("Step: %v", err)
		}
	}

	dropped := map[int]bool{}
	for _, ev := range rec.Events() {
		dropped[ev.FrameIndex] = true
	}
	if len(dropped) == 0 || len(dropped) == steps {
		t.Fatalf("seed gave %d drops of %d frames; pick a seed that mixes both", len(dropped), steps)
	}

	for i := 1; i < len(w.pkts); i++ {
		if w.pkts[i].SequenceNumber != w.pkts[i-1].SequenceNumber+1 {
			t.Fatalf("sequence gap at packet %d: %d then %d", i, w.pkts[i-1].SequenceNumber, w.pkts[i].SequenceNumber)
		}
	}

	want := map[uint32]bool{}
	for i := 0; i < steps; i++ {
		if !dropped[i] {
			want[uint32(m.AUs[i].DTS)] = true
		}
	}
	got := map[uint32]bool{}
	for _, pkt := range w.pkts {
		got[pkt.Timestamp] = true
	}
	if len(got) != len(want) {
		t.Fatalf("distinct timestamps = %d, want %d", len(got), len(want))
	}
	for ts := range want {
		if !got[ts] {
			t.Errorf("missing timestamp %d of a frame that should have been written", ts)
		}
	}
}

func TestFrameDropIsSeedDeterministic(t *testing.T) {
	run := func() []int {
		rec := obs.New(1000)
		p, _ := newTestPump(t, seed.Seed(77), []fault.Spec{{Kind: fault.KindFrameDrop, Rate: 0.4}}, rec)
		for i := 0; i < 90; i++ {
			if err := p.Step(); err != nil {
				t.Fatalf("Step: %v", err)
			}
		}
		var idx []int
		for _, ev := range rec.Events() {
			idx = append(idx, ev.FrameIndex)
		}
		return idx
	}
	a, b := run(), run()
	if len(a) == 0 {
		t.Fatal("no drops at rate 0.4 over 90 frames")
	}
	if len(a) != len(b) {
		t.Fatalf("runs differ: %d vs %d drops", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("drop %d differs: frame %d vs %d", i, a[i], b[i])
		}
	}
}

func TestProtectKeyframesKeepsRandomAccessFrames(t *testing.T) {
	m := testMedia(t)
	rec := obs.New(1000)
	p, w := newTestPump(t, seed.Seed(3), []fault.Spec{{Kind: fault.KindFrameDrop, Rate: 1, ProtectKeyframes: true}}, rec)

	steps := len(m.AUs)
	for i := 0; i < steps; i++ {
		if err := p.Step(); err != nil {
			t.Fatalf("Step: %v", err)
		}
	}

	wantTS := map[uint32]bool{}
	for _, au := range m.AUs {
		if au.RandomAccess {
			wantTS[uint32(au.DTS)] = true
		}
	}
	if len(wantTS) == 0 {
		t.Fatal("fixture has no random-access units")
	}
	gotTS := map[uint32]bool{}
	for _, pkt := range w.pkts {
		gotTS[pkt.Timestamp] = true
	}
	if len(gotTS) != len(wantTS) {
		t.Fatalf("written frames = %d, want only the %d keyframes", len(gotTS), len(wantTS))
	}
	for ts := range gotTS {
		if !wantTS[ts] {
			t.Errorf("a non-keyframe (timestamp %d) was written", ts)
		}
	}
	suppressed := 0
	for _, ev := range rec.Events() {
		if ev.Detail == detailSuppressed {
			suppressed++
		}
	}
	if suppressed != len(wantTS) {
		t.Errorf("suppressed events = %d, want %d", suppressed, len(wantTS))
	}
}

func TestSetFaultsSwapsInAtTheNextFrameBoundary(t *testing.T) {
	rec := obs.New(1000)
	p, w := newTestPump(t, seed.Seed(5), nil, rec)

	for i := 0; i < 2; i++ {
		if err := p.Step(); err != nil {
			t.Fatalf("Step: %v", err)
		}
	}
	written := len(w.pkts)
	if written == 0 {
		t.Fatal("no packets before the swap")
	}

	var appliedAt []int
	if err := p.SetFaults([]fault.Spec{{Kind: fault.KindFrameDrop, Rate: 1}}, func(f int) { appliedAt = append(appliedAt, f) }); err != nil {
		t.Fatalf("SetFaults: %v", err)
	}
	if len(appliedAt) != 0 {
		t.Fatal("SetFaults applied immediately; it must wait for the next Step")
	}
	if err := p.Step(); err != nil {
		t.Fatalf("Step: %v", err)
	}
	if len(appliedAt) != 1 || appliedAt[0] != 2 {
		t.Fatalf("applied callbacks = %v, want [2]", appliedAt)
	}
	if len(w.pkts) != written {
		t.Fatal("the frame after the swap was written; it should have been dropped")
	}

	if err := p.SetFaults(nil, func(f int) { appliedAt = append(appliedAt, f) }); err != nil {
		t.Fatalf("SetFaults(nil): %v", err)
	}
	if err := p.Step(); err != nil {
		t.Fatalf("Step: %v", err)
	}
	if len(appliedAt) != 2 || appliedAt[1] != 3 {
		t.Fatalf("applied callbacks = %v, want [2 3]", appliedAt)
	}
	if len(w.pkts) == written {
		t.Fatal("clearing the faults did not resume writing")
	}
}

func TestSetFaultsRejectsBadSpecs(t *testing.T) {
	p, _ := newTestPump(t, seed.Seed(5), nil, obs.New(10))
	if err := p.SetFaults([]fault.Spec{{Kind: "nope", Rate: 1}}, nil); err == nil {
		t.Error("unknown kind accepted")
	}
	if err := p.SetFaults([]fault.Spec{{Kind: fault.KindFrameDrop, Rate: 2}}, nil); err == nil {
		t.Error("rate 2 accepted")
	}
}

// Across a dropped frame the written timestamp advances by exactly
// (dropped+1) frame durations.
func TestFrameDropTimestampDeltaIsExactFrameIntervals(t *testing.T) {
	m := testMedia(t)
	rec := obs.New(1000)
	p, w := newTestPump(t, seed.Seed(0xD20F), []fault.Spec{{Kind: fault.KindFrameDrop, Rate: 0.5}}, rec)

	steps := len(m.AUs)
	for i := 0; i < steps; i++ {
		if err := p.Step(); err != nil {
			t.Fatalf("Step: %v", err)
		}
	}
	dropped := map[int]bool{}
	for _, ev := range rec.Events() {
		dropped[ev.FrameIndex] = true
	}
	if len(dropped) == 0 || len(dropped) == steps {
		t.Fatalf("seed gave %d drops of %d frames; pick a seed that mixes both", len(dropped), steps)
	}

	// Distinct written timestamps in order.
	var tss []uint32
	for _, pkt := range w.pkts {
		if len(tss) == 0 || tss[len(tss)-1] != pkt.Timestamp {
			tss = append(tss, pkt.Timestamp)
		}
	}
	// Expected: for each written frame i, gap to the previous written frame is
	// (i - prevWritten) * FrameDuration.
	var written []int
	for i := 0; i < steps; i++ {
		if !dropped[i] {
			written = append(written, i)
		}
	}
	if len(tss) != len(written) {
		t.Fatalf("distinct timestamps = %d, want %d", len(tss), len(written))
	}
	sawGap := false
	for k := 1; k < len(written); k++ {
		n := written[k] - written[k-1]
		if n > 1 {
			sawGap = true
		}
		want := uint32(int64(n) * m.FrameDuration())
		if got := tss[k] - tss[k-1]; got != want {
			t.Errorf("delta between frames %d and %d = %d, want %d", written[k-1], written[k], got, want)
		}
	}
	if !sawGap {
		t.Fatal("no dropped frame sat between two written frames")
	}
}

// Two same-seed pumps swapped to the same faults at the same frame index drop
// identical frames.
func TestSetFaultsMidRunIsSeedDeterministic(t *testing.T) {
	run := func() []int {
		rec := obs.New(1000)
		p, _ := newTestPump(t, seed.Seed(91), nil, rec)
		for i := 0; i < 80; i++ {
			if i == 20 {
				if err := p.SetFaults([]fault.Spec{{Kind: fault.KindFrameDrop, Rate: 0.4}}, nil); err != nil {
					t.Fatalf("SetFaults: %v", err)
				}
			}
			if err := p.Step(); err != nil {
				t.Fatalf("Step: %v", err)
			}
		}
		var idx []int
		for _, ev := range rec.Events() {
			idx = append(idx, ev.FrameIndex)
		}
		return idx
	}
	a, b := run(), run()
	if len(a) == 0 {
		t.Fatal("no drops after the swap")
	}
	if a[0] < 20 {
		t.Fatalf("drop at frame %d before the swap", a[0])
	}
	if len(a) != len(b) {
		t.Fatalf("runs differ: %d vs %d drops", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("drop %d differs: frame %d vs %d", i, a[i], b[i])
		}
	}
}

func TestNewPumpRejectsBadConfig(t *testing.T) {
	m := testMedia(t)
	medi := &description.Media{Type: description.MediaTypeVideo}
	good := PumpConfig{CameraID: "c", Media: m, Medi: medi, Writer: &recorder{}, Obs: obs.New(1)}

	bad := map[string]func(PumpConfig) PumpConfig{
		"no camera id": func(c PumpConfig) PumpConfig { c.CameraID = ""; return c },
		"no media":     func(c PumpConfig) PumpConfig { c.Media = nil; return c },
		"no writer":    func(c PumpConfig) PumpConfig { c.Writer = nil; return c },
		"no obs":       func(c PumpConfig) PumpConfig { c.Obs = nil; return c },
		"no medi":      func(c PumpConfig) PumpConfig { c.Medi = nil; return c },
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
