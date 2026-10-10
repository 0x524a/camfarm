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
				// RST must be distinguishable from a clean close.
				isReset := strings.Contains(strings.ToLower(err.Error()), "reset")
				if mode == "reset" && !isReset {
					t.Fatalf("reset mode error = %v, want a connection reset", err)
				}
				if mode == "close" && isReset {
					t.Fatalf("close mode error = %v, must not be a reset", err)
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

// Racing RemoveCamera and Close against a teardown, with frames flowing. armed
// removes while the teardown is still pending (large AfterFrames); firing lets
// the teardown trip concurrently with the removal (small AfterFrames, removal
// not waiting for the client).
func TestTeardownRacingRemoveCameraDoesNotPanicOrHang(t *testing.T) {
	for _, c := range []struct {
		name        string
		afterFrames int
		mode        string
	}{
		{"armed-reset", 100000, "reset"},
		{"armed-close", 100000, "close"},
		{"firing-reset", 3, "reset"},
		{"firing-close", 3, "close"},
	} {
		t.Run(c.name, func(t *testing.T) {
			racingRemove(t, c.afterFrames, c.mode)
		})
	}
}

func racingRemove(t *testing.T, afterFrames int, mode string) {
	srv, rec := startFaultServer(t, nil)
	if err := srv.SetFaults("cam-00", []fault.Spec{{Kind: fault.KindTeardown, AfterFrames: afterFrames, Mode: mode, Rate: 1}}, nil); err != nil {
		t.Fatal(err)
	}
	var packets atomic.Int64
	_ = playClient(t, srv.URL("cam-00"), func() { packets.Add(1) })
	if afterFrames > 100 {
		// Frames are demonstrably flowing while the teardown stays pending.
		awaitCond(t, "frames flowing", func() bool { return packets.Load() > 3 })
		if got := rec.Counters("cam-00").ByKind["teardown"].Applied; got != 0 {
			t.Fatalf("teardown fired early: %d", got)
		}
	}

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
