package rtsp

import (
	"reflect"
	"testing"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/pion/rtp"

	"github.com/0x524a/camfarm/internal/fault"
	"github.com/0x524a/camfarm/internal/media"
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
		if ev.Kind == string(fault.KindFrameDrop) {
			dropped[ev.FrameIndex] = true
		}
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

// SetFaults may be called from any goroutine while another steps the pump. Under
// -race this pins that the queue is synchronised, and every swap is reported
// exactly once, in order, at a non-decreasing frame index.
func TestSetFaultsConcurrentWithStepIsRaceFree(t *testing.T) {
	p, _ := newTestPump(t, seed.Seed(6), nil, obs.New(1000))
	const swaps = 50
	var applied []int
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < swaps; i++ {
			rate := float64(i%2) * 0.5
			if err := p.SetFaults([]fault.Spec{{Kind: fault.KindFrameDrop, Rate: rate}}, func(f int) { applied = append(applied, f) }); err != nil {
				t.Errorf("SetFaults: %v", err)
				return
			}
		}
	}()
	for running := true; running; {
		select {
		case <-done:
			running = false
		default:
		}
		if err := p.Step(); err != nil {
			t.Fatalf("Step: %v", err)
		}
	}
	if err := p.Step(); err != nil { // applies anything queued after the last loop Step
		t.Fatalf("Step: %v", err)
	}
	if len(applied) != swaps {
		t.Fatalf("%d swaps applied, want %d", len(applied), swaps)
	}
	for i := 1; i < len(applied); i++ {
		if applied[i] < applied[i-1] {
			t.Fatalf("swap frame indices go backwards: %v", applied)
		}
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
		if ev.Kind == string(fault.KindFrameDrop) {
			dropped[ev.FrameIndex] = true
		}
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

// perFrame runs a fault-free pump and records, for each of the first n access
// units, how many packets it produced and whether it is random access. Loss
// tests compare against it.
func perFrame(t *testing.T, n int) (counts []int, random []bool) {
	t.Helper()
	m := testMedia(t)
	p, w := newTestPump(t, seed.Seed(1), nil, obs.New(10))
	for i := 0; i < n; i++ {
		before := len(w.pkts)
		if err := p.Step(); err != nil {
			t.Fatalf("Step: %v", err)
		}
		counts = append(counts, len(w.pkts)-before)
		random = append(random, m.AUs[i%len(m.AUs)].RandomAccess)
	}
	return counts, random
}

func TestKeyframeStarvationWithholdsAllButEveryNthKeyframe(t *testing.T) {
	m := testMedia(t)
	rec := obs.New(1000)
	p, w := newTestPump(t, seed.Seed(3), []fault.Spec{{Kind: fault.KindKeyframeStarvation, KeepEvery: 2}}, rec)

	steps := len(m.AUs) * 3
	wantWithheld := map[int]bool{}
	k := 0
	for i := 0; i < steps; i++ {
		if m.AUs[i%len(m.AUs)].RandomAccess {
			if k%2 != 0 {
				wantWithheld[i] = true
			}
			k++
		}
	}
	if len(wantWithheld) == 0 {
		t.Fatal("fixture has too few keyframes for this test")
	}
	for i := 0; i < steps; i++ {
		if err := p.Step(); err != nil {
			t.Fatalf("Step: %v", err)
		}
	}
	gotWithheld := map[int]bool{}
	for _, ev := range rec.Events() {
		if ev.Kind != string(fault.KindKeyframeStarvation) || ev.Detail != detailKeyframeWithheld {
			t.Fatalf("unexpected event %+v", ev)
		}
		gotWithheld[ev.FrameIndex] = true
	}
	if len(gotWithheld) != len(wantWithheld) {
		t.Fatalf("withheld frames = %v, want %v", gotWithheld, wantWithheld)
	}
	for i := range wantWithheld {
		if !gotWithheld[i] {
			t.Fatalf("frame %d should have been withheld; got %v", i, gotWithheld)
		}
	}
	c := rec.Counters("cam")
	if got := c.FramesServed; got != uint64(steps-len(wantWithheld)) {
		t.Errorf("FramesServed = %d, want %d", got, steps-len(wantWithheld))
	}
	if got := c.ByKind["keyframe_starvation"].Applied; got != uint64(len(wantWithheld)) {
		t.Errorf("tally = %d, want %d", got, len(wantWithheld))
	}
	// Withheld frames are never encoded, so sequence numbers stay contiguous.
	for i := 1; i < len(w.pkts); i++ {
		if w.pkts[i].SequenceNumber != w.pkts[i-1].SequenceNumber+1 {
			t.Fatalf("sequence gap at packet %d", i)
		}
	}
}

func TestPacketLossLeavesSequenceGapsAndHonestTimestamps(t *testing.T) {
	const steps = 60
	counts, _ := perFrame(t, steps)
	total := 0
	for _, c := range counts {
		total += c
	}

	// Fault-free run of the same fixture and seed: the reference timestamps.
	cleanP, cleanW := newTestPump(t, seed.Seed(0x1055), nil, obs.New(1000))
	for i := 0; i < steps; i++ {
		if err := cleanP.Step(); err != nil {
			t.Fatalf("clean Step: %v", err)
		}
	}
	cleanTS := map[uint16]uint32{}
	for _, pkt := range cleanW.pkts {
		cleanTS[pkt.SequenceNumber] = pkt.Timestamp
	}

	rec := obs.New(1000)
	p, w := newTestPump(t, seed.Seed(0x1055), []fault.Spec{{Kind: fault.KindPacketLoss, Rate: 0.3}}, rec)
	for i := 0; i < steps; i++ {
		if err := p.Step(); err != nil {
			t.Fatalf("Step: %v", err)
		}
	}
	for i, pkt := range w.pkts {
		want, ok := cleanTS[pkt.SequenceNumber]
		if !ok || pkt.Timestamp != want {
			t.Fatalf("packet %d (seq %d): timestamp %d, fault-free run has %d (present=%v)", i, pkt.SequenceNumber, pkt.Timestamp, want, ok)
		}
	}
	tally := rec.Counters("cam").ByKind["packet_loss"]
	if tally.Applied == 0 || int(tally.Applied) == total {
		t.Fatalf("withheld %d of %d packets; need a mix", tally.Applied, total)
	}
	if got := uint64(len(w.pkts)) + tally.Applied; got != uint64(total) {
		t.Fatalf("written %d + withheld %d != produced %d", len(w.pkts), tally.Applied, total)
	}
	gaps := 0
	for i := 1; i < len(w.pkts); i++ {
		d := w.pkts[i].SequenceNumber - w.pkts[i-1].SequenceNumber
		if d == 0 {
			t.Fatalf("duplicate sequence number at packet %d", i)
		}
		if d > 1 {
			gaps++
		}
		if w.pkts[i].Timestamp < w.pkts[i-1].Timestamp {
			t.Fatalf("timestamp went backwards at packet %d", i)
		}
	}
	if gaps == 0 {
		t.Fatal("no sequence gaps despite withheld packets")
	}
	// The frame still counts as served: it was encoded and offered.
	if got := rec.Counters("cam").FramesServed; got != steps {
		t.Errorf("FramesServed = %d, want %d", got, steps)
	}
}

func TestPacketLossIsSeedDeterministicThroughThePump(t *testing.T) {
	run := func() []uint16 {
		p, w := newTestPump(t, seed.Seed(77), []fault.Spec{{Kind: fault.KindPacketLoss, Rate: 0.4}}, obs.New(1000))
		for i := 0; i < 90; i++ {
			if err := p.Step(); err != nil {
				t.Fatalf("Step: %v", err)
			}
		}
		var seqs []uint16
		for _, pkt := range w.pkts {
			seqs = append(seqs, pkt.SequenceNumber)
		}
		return seqs
	}
	a, b := run(), run()
	if len(a) == 0 || len(a) != len(b) {
		t.Fatalf("runs differ in length: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("packet %d: sequence %d vs %d", i, a[i], b[i])
		}
	}
}

func TestPacketLossProtectKeyframesSparesRandomAccessFrames(t *testing.T) {
	const steps = 60
	counts, random := perFrame(t, steps)
	wantWritten, wantSpared := 0, 0
	for i, c := range counts {
		if random[i] {
			wantWritten += c
			wantSpared += c
		}
	}
	if wantSpared == 0 {
		t.Fatal("fixture produced no keyframe packets")
	}

	rec := obs.New(1000)
	p, w := newTestPump(t, seed.Seed(4), []fault.Spec{{Kind: fault.KindPacketLoss, Rate: 1, ProtectKeyframes: true}}, rec)
	for i := 0; i < steps; i++ {
		if err := p.Step(); err != nil {
			t.Fatalf("Step: %v", err)
		}
	}
	if len(w.pkts) != wantWritten {
		t.Fatalf("wrote %d packets, want exactly the keyframe packets (%d)", len(w.pkts), wantWritten)
	}
	tally := rec.Counters("cam").ByKind["packet_loss"]
	if tally.Suppressed != uint64(wantSpared) {
		t.Errorf("suppressed = %d, want %d", tally.Suppressed, wantSpared)
	}
}

func TestFrameDropTallyCountsAppliedAndSuppressed(t *testing.T) {
	rec := obs.New(1000)
	p, _ := newTestPump(t, seed.Seed(5), []fault.Spec{{Kind: fault.KindFrameDrop, Rate: 1, ProtectKeyframes: true}}, rec)
	steps := len(testMedia(t).AUs)
	for i := 0; i < steps; i++ {
		if err := p.Step(); err != nil {
			t.Fatalf("Step: %v", err)
		}
	}
	tally := rec.Counters("cam").ByKind["frame_drop"]
	if tally.Applied == 0 || tally.Suppressed == 0 || tally.Applied+tally.Suppressed != uint64(steps) {
		t.Fatalf("tally = %+v over %d frames; want applied+suppressed == frames, both non-zero", tally, steps)
	}
}

func TestAfterFrameIsCalledOncePerStepIncludingDroppedFrames(t *testing.T) {
	for _, faults := range [][]fault.Spec{nil, {{Kind: fault.KindFrameDrop, Rate: 1}}} {
		calls := 0
		w := &recorder{}
		p, err := NewPump(PumpConfig{
			CameraID: "cam", Media: testMedia(t), Medi: &description.Media{Type: description.MediaTypeVideo},
			Writer: w, Seed: seed.Seed(1), Faults: faults, Obs: obs.New(10),
			AfterFrame: func() { calls++ },
		})
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 7; i++ {
			if err := p.Step(); err != nil {
				t.Fatal(err)
			}
		}
		if calls != 7 {
			t.Fatalf("faults %v: AfterFrame called %d times over 7 steps", faults, calls)
		}
	}
}

// pumpWithMedia is newTestPump over a caller-supplied source.
func pumpWithMedia(t *testing.T, m *media.Media, faults []fault.Spec, rec *obs.Recorder) (*Pump, *recorder) {
	t.Helper()
	w := &recorder{}
	p, err := NewPump(PumpConfig{
		CameraID: "cam",
		Media:    m,
		Medi:     &description.Media{Type: description.MediaTypeVideo},
		Writer:   w,
		Seed:     seed.Seed(1),
		Faults:   faults,
		Obs:      rec,
	})
	if err != nil {
		t.Fatalf("NewPump: %v", err)
	}
	return p, w
}

// frameTimestamps steps n times and returns the timestamp of the first packet
// each step wrote (a step that wrote nothing contributes nothing).
func frameTimestamps(t *testing.T, p *Pump, w *recorder, n int) []uint32 {
	t.Helper()
	var out []uint32
	for i := 0; i < n; i++ {
		before := len(w.pkts)
		if err := p.Step(); err != nil {
			t.Fatalf("Step: %v", err)
		}
		if len(w.pkts) > before {
			out = append(out, w.pkts[before].Timestamp)
		}
	}
	return out
}

// regularMedia returns the fixture with DTS and PTS rebased to a fixed frame
// duration, so the nominal timestamps are known exactly.
func regularMedia(t *testing.T, dur int64, fps float64) *media.Media {
	t.Helper()
	src := testMedia(t)
	m := *src
	m.FPS = fps
	m.AUs = append([]media.AccessUnit(nil), src.AUs...)
	for i := range m.AUs {
		m.AUs[i].DTS = int64(i) * dur
		m.AUs[i].PTS = int64(i) * dur
	}
	return &m
}

func TestReorderStampsOutOfOrderTimestamps(t *testing.T) {
	spec := []fault.Spec{{Kind: fault.KindPTSDTSMismatch, ReorderDepth: 1}}
	cases := []struct {
		name string
		dur  int64
		fps  float64
		want []uint32
	}{
		{"30 fps", 3000, 30, []uint32{3000, 0, 9000, 6000}},
		{"25 fps", 3600, 25, []uint32{3600, 0, 10800, 7200}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := obs.New(100)
			p, w := pumpWithMedia(t, regularMedia(t, c.dur, c.fps), spec, rec)
			if p.frameDur != c.dur {
				t.Fatalf("frameDur = %d, want %d", p.frameDur, c.dur)
			}
			got := frameTimestamps(t, p, w, 4)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("timestamps = %v, want %v", got, c.want)
			}
			// Sequence numbers stay contiguous: only the timestamps lie.
			for i := 1; i < len(w.pkts); i++ {
				if w.pkts[i].SequenceNumber != w.pkts[i-1].SequenceNumber+1 {
					t.Fatalf("sequence gap at packet %d", i)
				}
			}
			if ev := rec.Events(); len(ev) != 4 || ev[0].Kind != "pts_dts_mismatch" {
				t.Fatalf("events = %+v, want one pts_dts_mismatch per frame", ev)
			}
			if k := rec.Counters("cam").ByKind["pts_dts_mismatch"]; k.Applied != 4 {
				t.Errorf("tally = %+v, want 4 applied", k)
			}
		})
	}
}

// A negative sum wraps in uint32; that is the required behaviour.
func TestDelayedFirstFrameWrapsBelowZero(t *testing.T) {
	p, w := pumpWithMedia(t, regularMedia(t, 3000, 30), []fault.Spec{{Kind: fault.KindPTSDelayed, DelayMs: 10}}, obs.New(10))
	got := frameTimestamps(t, p, w, 1)
	if len(got) != 1 || got[0] != 4294966396 {
		t.Fatalf("timestamps = %v, want [4294966396]", got)
	}
}

// R4: a frame removed by frame_drop still advances the timestamp-fault
// counters, so lag is frame-clocked rather than delivery-clocked.
func TestDroppedFrameStillAdvancesDriftCounter(t *testing.T) {
	m := regularMedia(t, 3000, 30)
	drift := fault.Spec{Kind: fault.KindPTSDelayed, DriftPpm: 100000}

	pd, wd := pumpWithMedia(t, m, []fault.Spec{{Kind: fault.KindFrameDrop, Rate: 0.5}, drift}, obs.New(1000))
	delivered := map[int]uint32{}
	for i := 0; i < len(m.AUs); i++ {
		before := len(wd.pkts)
		if err := pd.Step(); err != nil {
			t.Fatalf("Step: %v", err)
		}
		if len(wd.pkts) > before {
			delivered[i] = wd.pkts[before].Timestamp
		}
	}
	if len(delivered) == 0 || len(delivered) == len(m.AUs) {
		t.Fatalf("seed gave %d delivered of %d; need a mix", len(delivered), len(m.AUs))
	}

	pu, wu := pumpWithMedia(t, m, []fault.Spec{drift}, obs.New(1000))
	for i := 0; i < len(m.AUs); i++ {
		before := len(wu.pkts)
		if err := pu.Step(); err != nil {
			t.Fatalf("Step: %v", err)
		}
		if ts, ok := delivered[i]; ok && ts != wu.pkts[before].Timestamp {
			t.Fatalf("frame %d: timestamp %d with drops, %d without", i, ts, wu.pkts[before].Timestamp)
		}
	}
}
