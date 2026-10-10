package rtsp

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/pion/rtp"

	"github.com/0x524a/camfarm/internal/fault"
)

// playingClient is a real TCP client that has reached PLAY on a camera.
type playingClient struct {
	c    *gortsplib.Client
	done chan error
}

// Done yields the error the client's session ended with.
func (p *playingClient) Done() <-chan error { return p.done }

// playClient connects, plays and counts RTP packets through onPacket.
func playClient(t *testing.T, url string, onPacket func()) *playingClient {
	t.Helper()
	u, err := base.ParseURL(url)
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
	c.OnPacketRTPAny(func(_ *description.Media, _ format.Format, _ *rtp.Packet) { onPacket() })
	if _, err := c.Play(nil); err != nil {
		t.Fatalf("PLAY: %v", err)
	}
	pc := &playingClient{c: c, done: make(chan error, 1)}
	go func() { pc.done <- c.Wait() }()
	return pc
}

func TestTeardownEndsTheSessionAfterNFrames(t *testing.T) {
	for _, mode := range []string{"close", "reset"} {
		t.Run(mode, func(t *testing.T) {
			srv, rec := startFaultServer(t, nil)
			if err := srv.SetFaults("cam-00", []fault.Spec{{Kind: fault.KindTeardown, AfterFrames: 5, Mode: mode, Rate: 1}}, nil); err != nil {
				t.Fatal(err)
			}
			var packets atomic.Int64
			c := playClient(t, srv.URL("cam-00"), func() { packets.Add(1) })
			select {
			case err := <-c.Done():
				if err == nil {
					t.Fatal("session ended without error")
				}
				if mode == "reset" && !strings.Contains(err.Error(), "reset") && !strings.Contains(err.Error(), "EOF") {
					t.Fatalf("reset mode error = %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("session not torn down")
			}
			if n := packets.Load(); n < 5 || n > 40 { // generous: frames can span packets
				t.Fatalf("got %d packets before teardown", n)
			}
			if got := rec.Counters("cam-00").ByKind["teardown"].Applied; got != 1 {
				t.Fatalf("teardown tally = %d, want 1", got)
			}
		})
	}
}

func TestTeardownRateZeroNeverArms(t *testing.T) {
	srv, rec := startFaultServer(t, nil)
	if err := srv.SetFaults("cam-00", []fault.Spec{{Kind: fault.KindTeardown, AfterFrames: 5, Rate: 1e-9}}, nil); err != nil {
		t.Fatal(err)
	}
	c := playClient(t, srv.URL("cam-00"), func() {})
	before := rec.Counters("cam-00").FramesServed
	awaitCond(t, "60 frames", func() bool { return rec.Counters("cam-00").FramesServed >= before+60 })
	select {
	case err := <-c.Done():
		t.Fatalf("session ended: %v", err)
	default:
	}
	if got := rec.Counters("cam-00").ByKind["teardown"].Applied; got != 0 {
		t.Fatalf("teardown tally = %d, want 0", got)
	}
}

func TestTeardownRacingRemoveCameraDoesNotPanicOrHang(t *testing.T) {
	srv, _ := startFaultServer(t, nil)
	if err := srv.SetFaults("cam-00", []fault.Spec{{Kind: fault.KindTeardown, AfterFrames: 3, Mode: "reset", Rate: 1}}, nil); err != nil {
		t.Fatal(err)
	}
	var packets atomic.Int64
	_ = playClient(t, srv.URL("cam-00"), func() { packets.Add(1) })
	awaitCond(t, "first packets", func() bool { return packets.Load() > 0 })

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.RemoveCamera("cam-00")
		srv.Close()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("RemoveCamera/Close hung")
	}
}
