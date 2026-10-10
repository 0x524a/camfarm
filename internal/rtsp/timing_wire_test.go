package rtsp

import (
	"math"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/ntp"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"

	"github.com/0x524a/camfarm/internal/fault"
	"github.com/0x524a/camfarm/internal/obs"
	"github.com/0x524a/camfarm/internal/seed"
)

// These tests drive a real gortsplib client over real TCP against a server on
// the real clock, so every timing bound is a loose bound, chosen to catch the
// fault being absent or inverted, not to measure it precisely.
//
// The shipped fixture is 15 fps with a keyframe every 15 frames, so the nominal
// frame interval is ~66.7 ms and one frame is 6000 RTP ticks at 90 kHz.
const (
	wireFrameTicks   = 6000
	wireFrameNominal = 1000 * time.Millisecond / 15
)

// wirePacket is one RTP packet as the client saw it.
type wirePacket struct {
	at  time.Time
	ts  uint32
	seq uint16
}

// wireCollector accumulates the packets a real client reads.
type wireCollector struct {
	mu   sync.Mutex
	pkts []wirePacket
}

func (w *wireCollector) add(p *rtp.Packet) {
	w.mu.Lock()
	w.pkts = append(w.pkts, wirePacket{at: time.Now(), ts: p.Timestamp, seq: p.SequenceNumber})
	w.mu.Unlock()
}

func (w *wireCollector) snapshot() []wirePacket {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]wirePacket(nil), w.pkts...)
}

// framesOf collapses packets to one entry per access unit, in arrival order: the
// access unit's first packet. An access unit is a run of packets sharing an RTP
// timestamp.
func framesOf(pkts []wirePacket) []wirePacket {
	var out []wirePacket
	for _, p := range pkts {
		if len(out) == 0 || out[len(out)-1].ts != p.ts {
			out = append(out, p)
		}
	}
	return out
}

func (w *wireCollector) frameCount() int { return len(framesOf(w.snapshot())) }

// playWire connects a real client to the camera, calls onPacket for every RTP
// packet and onRTCP (if non-nil) for every RTCP packet, and starts playback.
// The client is closed at test end.
func playWire(t *testing.T, s *Server, id string, onPacket func(c *gortsplib.Client, m *description.Media, p *rtp.Packet), onRTCP func(rtcp.Packet)) {
	t.Helper()
	u, err := base.ParseURL(s.URL(id))
	if err != nil {
		t.Fatal(err)
	}
	c, _ := newTestClient(u)
	if err := c.Start(); err != nil {
		t.Fatalf("client start: %v", err)
	}
	t.Cleanup(c.Close)
	desc, _, err := c.Describe(u)
	if err != nil {
		t.Fatalf("DESCRIBE: %v", err)
	}
	if err := c.SetupAll(desc.BaseURL, desc.Medias); err != nil {
		t.Fatalf("SETUP: %v", err)
	}
	c.OnPacketRTPAny(func(m *description.Media, _ format.Format, p *rtp.Packet) { onPacket(c, m, p) })
	if onRTCP != nil {
		c.OnPacketRTCPAny(func(_ *description.Media, p rtcp.Packet) { onRTCP(p) })
	}
	if _, err := c.Play(nil); err != nil {
		t.Fatalf("PLAY: %v", err)
	}
}

// collectFrames plays the camera until at least n access units were seen.
func collectFrames(t *testing.T, s *Server, id string, n int) []wirePacket {
	t.Helper()
	w := &wireCollector{}
	playWire(t, s, id, func(_ *gortsplib.Client, _ *description.Media, p *rtp.Packet) { w.add(p) }, nil)
	awaitCond(t, "frames", func() bool { return w.frameCount() >= n })
	return w.snapshot()
}

func gapsMs(frames []wirePacket) []float64 {
	var g []float64
	for i := 1; i < len(frames); i++ {
		g = append(g, float64(frames[i].at.Sub(frames[i-1].at))/float64(time.Millisecond))
	}
	return g
}

func stddev(xs []float64) float64 {
	var sum float64
	for _, x := range xs {
		sum += x
	}
	mean := sum / float64(len(xs))
	var sq float64
	for _, x := range xs {
		sq += (x - mean) * (x - mean)
	}
	return math.Sqrt(sq / float64(len(xs)))
}

// assertContiguousSeq fails if the packets' sequence numbers are not
// consecutive (mod 2^16): none of the timing faults may lose a packet.
func assertContiguousSeq(t *testing.T, pkts []wirePacket) {
	t.Helper()
	for i := 1; i < len(pkts); i++ {
		if pkts[i].seq != pkts[i-1].seq+1 {
			t.Fatalf("sequence not contiguous at packet %d: %d then %d", i, pkts[i-1].seq, pkts[i].seq)
		}
	}
}

// Jitter models a network or encoder that delivers frames at uneven spacing
// while keeping them in order, which is what sizes a jitter buffer. A real
// client must see the inter-arrival spread widen, and must still see RTP
// timestamps strictly increasing.
func TestTimingJitterSpreadsInterArrivalTimes(t *testing.T) {
	const frames = 40
	clean, _ := startFaultServer(t, nil)
	jit, _ := startFaultServer(t, []fault.Spec{{Kind: fault.KindJitter, MaxMs: 200}})

	cleanFrames := framesOf(collectFrames(t, clean, "cam-00", frames))[:frames]
	jitPkts := collectFrames(t, jit, "cam-00", frames)
	jitFrames := framesOf(jitPkts)[:frames]

	cleanSD := stddev(gapsMs(cleanFrames))
	jitSD := stddev(gapsMs(jitFrames))
	t.Logf("gap stddev: clean %.2f ms, jittered %.2f ms", cleanSD, jitSD)

	// The 5 ms margin over the clean run and the 8 ms floor are deliberately far
	// below the ~80 ms a uniform 0-200 ms delay produces, so scheduler noise on a
	// loaded CI runner cannot flip the result, while a jitter that does nothing
	// (stddev equal to the clean run's) still fails.
	if jitSD < cleanSD+5 {
		t.Fatalf("jittered stddev %.2f ms is not at least 5 ms above clean %.2f ms", jitSD, cleanSD)
	}
	if jitSD <= 8 {
		t.Fatalf("jittered stddev %.2f ms, want > 8 ms", jitSD)
	}
	for i := 1; i < len(jitFrames); i++ {
		if int32(jitFrames[i].ts-jitFrames[i-1].ts) <= 0 {
			t.Fatalf("jitter reordered frames: timestamp %d then %d", jitFrames[i-1].ts, jitFrames[i].ts)
		}
	}
	assertContiguousSeq(t, jitPkts)
}

// holdSeed finds a root seed whose burst_delivery draws start exactly one hold
// of h frames at frame start, and no other hold within the first horizon
// frames, so the burst is the only disturbance the client sees. start is late
// enough that a client joining a moment after server start has not missed it.
func holdSeed(t *testing.T, start, h, horizon int, rate float64) seed.Seed {
	t.Helper()
	for n := 1; n < 200000; n++ {
		s := seed.Seed(n)
		e, err := fault.New(s.Stream("fault"), []fault.Spec{{Kind: fault.KindBurstDelivery, Rate: rate, HoldFrames: h}})
		if err != nil {
			t.Fatal(err)
		}
		ok := true
		for k := 0; k < horizon && ok; k++ {
			d := e.DecideHold(k)
			switch {
			case k < start, k >= start+h:
				ok = d == fault.HoldNone
			case k < start+h-1:
				ok = d == fault.HoldHeld
			default:
				ok = d == fault.HoldFlush
			}
		}
		if ok {
			return s
		}
	}
	t.Fatalf("no seed holds exactly frames %d-%d within %d frames at rate %v", start, start+h-1, horizon, rate)
	return 0
}

// Burst delivery models an uplink or encoder that buffers and then releases
// frames back-to-back, which overflows an NVR's queue. The client must see
// holdFrames frames arrive together, with honest timestamps and no lost packet,
// and then see normal spacing resume.
func TestTimingBurstDeliveryArrivesTogether(t *testing.T) {
	const (
		holdFrames = 4
		startFrame = 20 // 1.3 s in: after any realistic client join
		horizon    = 45
	)
	rootSeed := holdSeed(t, startFrame, holdFrames, horizon, 0.05)
	rec := obs.New(1000)
	s, err := New(Config{Host: "127.0.0.1", Obs: rec, Cameras: []CameraConfig{{
		ID: "cam-00", Media: testMedia(t), Seed: rootSeed,
		Faults: []fault.Spec{{Kind: fault.KindBurstDelivery, Rate: 0.05, HoldFrames: holdFrames}},
	}}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(s.Close)

	pkts := collectFrames(t, s, "cam-00", 40)
	frames := framesOf(pkts)

	// Find the run of holdFrames frames that arrived within 5 ms of each other.
	// 5 ms is the brief's bound: held frames are written in one loop with no
	// pacing, so on loopback they land within well under a millisecond.
	burstAt := -1
	var spread time.Duration
	for i := 0; i+holdFrames <= len(frames); i++ {
		if sp := frames[i+holdFrames-1].at.Sub(frames[i].at); sp < 5*time.Millisecond {
			burstAt, spread = i, sp
			break
		}
	}
	if burstAt < 0 {
		t.Fatalf("no run of %d frames arrived within 5 ms of each other", holdFrames)
	}
	t.Logf("burst of %d frames starts at client frame %d, arrival spread %v", holdFrames, burstAt, spread)
	if burstAt == 0 {
		t.Fatal("burst found at the very first frame; cannot tell it from the stream start, and nothing precedes it to compare against")
	}

	// Timestamps honest: held frames keep their capture-time stamps, so every
	// frame is exactly one frame apart even though four arrived together.
	for i := 1; i < len(frames); i++ {
		if d := int32(frames[i].ts - frames[i-1].ts); d != wireFrameTicks {
			t.Fatalf("frame %d timestamp step %d ticks, want %d", i, d, wireFrameTicks)
		}
	}
	assertContiguousSeq(t, pkts)

	// Only the cadence after the burst is asserted. The first frame after the
	// burst waits one tick (its own), so skip 3 frames and then average the
	// following ones: the cadence must be back at the nominal spacing. The 20 ms
	// band on a 66.7 ms interval absorbs timer and scheduler noise.
	from := burstAt + holdFrames + 3
	if from+8 > len(frames) {
		t.Fatalf("only %d frames; need %d to check the cadence after the burst", len(frames), from+8)
	}
	avg := frames[from+7].at.Sub(frames[from].at) / 7
	t.Logf("average gap after the burst: %v (nominal %v)", avg, wireFrameNominal)
	if avg < wireFrameNominal-20*time.Millisecond || avg > wireFrameNominal+20*time.Millisecond {
		t.Fatalf("average gap after the burst %v, want about %v", avg, wireFrameNominal)
	}
}

// pts_dts_mismatch models B-frame-style reordering: with reorderDepth 1 the
// timestamps a client reads are a pair-wise swap of the nominal ones, so the
// stream carries decode-order frames whose presentation times are out of
// order. The sequence numbers stay contiguous because nothing is lost.
func TestTimingReorderedTimestampsReachTheClient(t *testing.T) {
	s, _ := startFaultServer(t, []fault.Spec{{Kind: fault.KindPTSDTSMismatch, ReorderDepth: 1}})
	pkts := collectFrames(t, s, "cam-00", 30)
	frames := framesOf(pkts)
	assertContiguousSeq(t, pkts)

	// In frame-interval units relative to the first frame's nominal slot, window
	// W=2 sends 1,0,3,2,5,4,...: steps alternate -1 and +3. Which of the two
	// comes first depends on whether the client joined on an even or an odd
	// frame, so require the alternation, not a fixed phase.
	var steps []int32
	for i := 1; i < len(frames); i++ {
		d := int32(frames[i].ts - frames[i-1].ts)
		if d%wireFrameTicks != 0 {
			t.Fatalf("step %d is %d ticks, not a whole number of %d-tick frames", i, d, wireFrameTicks)
		}
		steps = append(steps, d/wireFrameTicks)
	}
	for i, st := range steps {
		if st != -1 && st != 3 {
			t.Fatalf("step %d = %d frames, want -1 or +3 (steps %v)", i, st, steps)
		}
		if i > 0 && st == steps[i-1] {
			t.Fatalf("steps %d and %d are both %d; reordering must alternate (steps %v)", i-1, i, st, steps)
		}
	}
	// Every window of two frames is a permutation of its own two nominal slots.
	// Pairing starts at the first frame the client saw if that was a window start
	// (first step -1), else one frame later.
	first := 0
	if steps[0] == 3 {
		first = 1
	}
	for i := first; i+1 < len(frames); i += 2 {
		lo, hi := frames[i].ts, frames[i+1].ts
		if hi-lo != ^uint32(wireFrameTicks-1) { // -wireFrameTicks as uint32
			t.Fatalf("window at frame %d is not a swap: %d then %d", i, lo, hi)
		}
	}
}

// medianMs returns the median of xs.
func medianMs(xs []float64) float64 {
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	return s[len(s)/2]
}

// srPoint is one RTCP sender report as the client saw it.
type srPoint struct {
	ntp time.Time
	rtp uint32
}

// rtpMinusWall is the sender report's RTP timestamp minus the RTP ticks that
// its own NTP time would map to on a clock that started at the Unix epoch. For
// two cameras that start together and stamp the same frames, it differs between
// them only by their timestamp offset.
func (p srPoint) rtpMinusWall() uint32 {
	return p.rtp - uint32(uint64(p.ntp.UnixNano()/1000)*9/100)
}

// pts_delayed models a camera whose RTP timestamps lag its capture clock (a
// stale or mis-set encoder clock). Two things follow from how gortsplib's
// server builds a sender report (pkg/rtpsender, v5.6.5), and this test pins
// both rather than assuming them:
//
//  1. The sender stores the RTP timestamp of the packet it was last given
//     (already shifted) together with the NTP time of that write, and each SR is
//     that pair advanced by elapsed wall time. The SR therefore carries the
//     shifted RTP clock: its RTP timestamp sits delayMs (45000 ticks at 90 kHz)
//     behind a clean camera's at the same NTP time.
//  2. Because the SR pairs the shifted timestamp with the true write time, a
//     client that maps packet -> NTP through it (Client.PacketNTP) gets the true
//     send time back. The constant delay is therefore NOT visible as a
//     disagreement between packet timestamps and SR NTP, contrary to what spec
//     2.9 assumed; a constant offset is absorbed into the RTP/NTP pair, and only
//     a drift (a rate change) would show up in the mapping.
//
// The 10 s sender report period of gortsplib does not matter here: the sender
// emits one SR as soon as its first packet is written.
func TestTimingPTSDelayedShowsInTheSenderReport(t *testing.T) {
	const delayMs = 500
	s, err := New(Config{Host: "127.0.0.1", Obs: obs.New(1000), Cameras: []CameraConfig{
		{ID: "cam-00", Media: testMedia(t), Seed: seed.Seed(21)},
		{ID: "cam-01", Media: testMedia(t), Seed: seed.Seed(21),
			Faults: []fault.Spec{{Kind: fault.KindPTSDelayed, DelayMs: delayMs}}},
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(s.Close)

	type observed struct {
		mu  sync.Mutex
		lag []float64 // arrival - PacketNTP, ms
		srs []srPoint
	}
	watch := func(id string) *observed {
		o := &observed{}
		playWire(t, s, id,
			func(c *gortsplib.Client, m *description.Media, p *rtp.Packet) {
				// Before the first sender report arrives there is no mapping.
				at, ok := c.PacketNTP(m, p)
				if !ok {
					return
				}
				o.mu.Lock()
				o.lag = append(o.lag, float64(time.Since(at))/float64(time.Millisecond))
				o.mu.Unlock()
			},
			func(pkt rtcp.Packet) {
				if sr, ok := pkt.(*rtcp.SenderReport); ok {
					o.mu.Lock()
					o.srs = append(o.srs, srPoint{ntp: ntp.Decode(sr.NTPTime), rtp: sr.RTPTime})
					o.mu.Unlock()
				}
			})
		return o
	}
	clean, delayed := watch("cam-00"), watch("cam-01")
	enough := func(o *observed) func() bool {
		return func() bool { o.mu.Lock(); defer o.mu.Unlock(); return len(o.lag) >= 20 && len(o.srs) >= 1 }
	}
	awaitCond(t, "a sender report and 20 mapped packets on the clean camera", enough(clean))
	awaitCond(t, "a sender report and 20 mapped packets on the delayed camera", enough(delayed))

	clean.mu.Lock()
	delayed.mu.Lock()
	cm, dm := medianMs(clean.lag), medianMs(delayed.lag)
	rtpDelta := int32(delayed.srs[0].rtpMinusWall() - clean.srs[0].rtpMinusWall())
	delayed.mu.Unlock()
	clean.mu.Unlock()
	ntpDelta := dm - cm
	t.Logf("SR RTP offset, delayed minus clean: %d ticks (%.1f ms); PacketNTP lag, delayed minus clean: %.1f ms",
		rtpDelta, float64(rtpDelta)/90, ntpDelta)

	// (1) The SR's RTP timestamp is delayMs behind the clean camera's. The
	// +/-150 ms band covers the two cameras starting a few ms apart and the
	// different instants their SRs were written; a missing or wrongly-signed
	// shift (0 or +45000) fails.
	if want := int32(-delayMs * 90); rtpDelta < want-150*90 || rtpDelta > want+150*90 {
		t.Fatalf("SR RTP time differs from the clean camera's by %d ticks, want %d +/- %d", rtpDelta, want, 150*90)
	}
	// (2) The same shift is invisible through the SR mapping: the client's
	// NTP-derived send time agrees with the true arrival time for both cameras.
	// The delayed camera must not look delayMs early. Same +/-150 ms band.
	if ntpDelta < -150 || ntpDelta > 150 {
		t.Fatalf("PacketNTP lag differs from the clean camera's by %.1f ms; the SR was expected to absorb the constant delay", ntpDelta)
	}
}
