package rtsp

import (
	"sync"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/pion/rtp"

	"github.com/0x524a/camfarm/internal/fault"
	"github.com/0x524a/camfarm/internal/obs"
	"github.com/0x524a/camfarm/internal/seed"
)

func awaitCond(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func startFaultServer(t *testing.T, faults []fault.Spec) *Server {
	t.Helper()
	s, err := New(Config{Host: "127.0.0.1", Obs: obs.New(1000), Cameras: []CameraConfig{
		{ID: "cam-00", Media: testMedia(t), Seed: seed.Seed(21), Faults: faults},
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

// A withheld packet is a real sequence gap on the wire. Over TCP-interleaved
// transport the client sees the gap but no stall, because TCP delivers
// everything it is given; over UDP it would be a genuine loss. This test only
// claims what both transports share: gaps, in order, with nothing duplicated.
func TestPacketLossGapsAreVisibleToARealClient(t *testing.T) {
	s := startFaultServer(t, []fault.Spec{{Kind: fault.KindPacketLoss, Rate: 0.3}})
	u, err := base.ParseURL(s.URL("cam-00"))
	if err != nil {
		t.Fatal(err)
	}
	c, _ := newTestClient(u)
	if err := c.Start(); err != nil {
		t.Fatalf("client start: %v", err)
	}
	defer c.Close()
	desc, _, err := c.Describe(u)
	if err != nil {
		t.Fatalf("DESCRIBE: %v", err)
	}
	if err := c.SetupAll(desc.BaseURL, desc.Medias); err != nil {
		t.Fatalf("SETUP: %v", err)
	}
	var mu sync.Mutex
	var seqs []uint16
	c.OnPacketRTPAny(func(_ *description.Media, _ format.Format, p *rtp.Packet) {
		mu.Lock()
		seqs = append(seqs, p.SequenceNumber)
		mu.Unlock()
	})
	if _, err := c.Play(nil); err != nil {
		t.Fatalf("PLAY: %v", err)
	}
	awaitCond(t, "150 packets", func() bool { mu.Lock(); defer mu.Unlock(); return len(seqs) >= 150 })

	mu.Lock()
	defer mu.Unlock()
	gaps := 0
	for i := 1; i < len(seqs); i++ {
		d := seqs[i] - seqs[i-1] // uint16 arithmetic: correct across wraparound
		if d == 0 || d > 0x8000 {
			t.Fatalf("not strictly increasing at %d: %d then %d", i, seqs[i-1], seqs[i])
		}
		if d > 1 {
			gaps++
		}
	}
	if gaps == 0 {
		t.Fatal("no sequence gaps seen by the client despite rate 0.3")
	}
}

// countKeyframes joins the camera as a late client, plays for d, and returns
// how many access units and how many random-access units it reassembled.
func countKeyframes(t *testing.T, s *Server, d time.Duration) (units, keyframes int) {
	t.Helper()
	u, err := base.ParseURL(s.URL("cam-00"))
	if err != nil {
		t.Fatal(err)
	}
	c, _ := newTestClient(u)
	if err := c.Start(); err != nil {
		t.Fatalf("client start: %v", err)
	}
	defer c.Close()
	desc, _, err := c.Describe(u)
	if err != nil {
		t.Fatalf("DESCRIBE: %v", err)
	}
	var h264Format *format.H264
	medi := desc.FindFormat(&h264Format)
	if medi == nil {
		t.Fatal("no H264 format in the description")
	}
	dec, err := h264Format.CreateDecoder()
	if err != nil {
		t.Fatalf("CreateDecoder: %v", err)
	}
	if _, err := c.Setup(desc.BaseURL, medi, 0, 0); err != nil {
		t.Fatalf("SETUP: %v", err)
	}
	var mu sync.Mutex
	c.OnPacketRTP(medi, h264Format, func(pkt *rtp.Packet) {
		au, err := dec.Decode(pkt)
		if err != nil || au == nil {
			return
		}
		mu.Lock()
		units++
		if h264.IsRandomAccess(au) {
			keyframes++
		}
		mu.Unlock()
	})
	if _, err := c.Play(nil); err != nil {
		t.Fatalf("PLAY: %v", err)
	}
	time.Sleep(d)
	mu.Lock()
	defer mu.Unlock()
	return units, keyframes
}

func TestKeyframeStarvationLeavesALateJoinerWithoutAKeyframe(t *testing.T) {
	// Control: without the fault a late joiner gets a keyframe within a few
	// seconds. Without this the starved assertion could pass vacuously (a
	// detector that never fires).
	clean := startFaultServer(t, nil)
	if units, keys := countKeyframes(t, clean, 4*time.Second); units == 0 || keys == 0 {
		t.Fatalf("control: %d units, %d keyframes; want both non-zero", units, keys)
	}

	// keepEvery 1000 means only the very first keyframe is ever delivered,
	// and the camera has long since sent it by the time a client joins.
	starved := startFaultServer(t, []fault.Spec{{Kind: fault.KindKeyframeStarvation, KeepEvery: 1000}})
	time.Sleep(500 * time.Millisecond) // let the first (delivered) keyframe pass before the client joins
	units, keys := countKeyframes(t, starved, 4*time.Second)
	if units == 0 {
		t.Fatal("starved camera delivered no access units at all; P frames should still flow")
	}
	if keys != 0 {
		t.Fatalf("late joiner received %d keyframes from a starved camera, want 0", keys)
	}
}
