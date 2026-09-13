package rtsp

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/liberrors"
	"github.com/pion/rtp"

	"github.com/0x524a/camfarm/internal/media"
	"github.com/0x524a/camfarm/internal/obs"
	"github.com/0x524a/camfarm/internal/seed"
)

func testMedia(t *testing.T) *media.Media {
	t.Helper()
	m, err := (media.FixtureSource{}).Load()
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	return m
}

// startServer brings up n cameras named cam-00..cam-(n-1) and returns it.
func startServer(t *testing.T, n int) *Server {
	t.Helper()
	m := testMedia(t)
	root := seed.Seed(0x3f2a9c81)

	cfg := Config{Host: "127.0.0.1", Obs: obs.New(1000)}
	for i := 0; i < n; i++ {
		cfg.Cameras = append(cfg.Cameras, CameraConfig{
			ID:    cameraID(i),
			Media: m,
			Seed:  root.Camera(i),
		})
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

func cameraID(i int) string {
	return "cam-" + string(rune('0'+i/10)) + string(rune('0'+i%10))
}

// newTestClient builds a client against u, wired to record whether the
// server forced a fallback to TCP. This server advertises no UDP ports, so
// any test that performs SETUP is expected to see exactly that fallback.
// Recording it instead of leaving it as a client-side log line means that
// the day UDP support lands, an unexpected downgrade fails a test instead of
// vanishing silently into stderr.
func newTestClient(u *base.URL) (c *gortsplib.Client, switchedToTCP func() bool) {
	var sawServerForcedTCP bool
	c = &gortsplib.Client{
		Scheme: u.Scheme,
		Host:   u.Host,
		OnTransportSwitch: func(err error) {
			if _, ok := err.(liberrors.ErrClientSwitchToTCPDueToServer); ok {
				sawServerForcedTCP = true
			}
		},
	}
	return c, func() bool { return sawServerForcedTCP }
}

func TestAddrIsNilBeforeStart(t *testing.T) {
	s, err := New(Config{Host: "127.0.0.1", Obs: obs.New(10), Cameras: []CameraConfig{
		{ID: "a", Media: testMedia(t), Seed: seed.Seed(1)},
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if s.Addr() != nil {
		t.Fatal("Addr() non-nil before Start()")
	}
	if _, _, err := s.streamStatsErr("a"); err == nil {
		t.Fatal("expected ErrNotStarted before Start()")
	}
}

// Port 0 must be resolved and readable back, so a test never has to guess a
// free port.
func TestEphemeralPortIsReadBack(t *testing.T) {
	s := startServer(t, 1)
	addr := s.Addr()
	if addr == nil {
		t.Fatal("Addr() is nil after Start()")
	}
	if addr.Port == 0 {
		t.Fatal("port was not resolved")
	}
	if !strings.HasPrefix(s.URL("cam-00"), "rtsp://127.0.0.1:") {
		t.Errorf("URL = %q", s.URL("cam-00"))
	}
	if !strings.HasSuffix(s.URL("cam-00"), "/cam-00") {
		t.Errorf("URL = %q", s.URL("cam-00"))
	}
}

func TestDescribeAdvertisesTheSourceParameterSets(t *testing.T) {
	s := startServer(t, 1)
	want := testMedia(t)

	u, err := base.ParseURL(s.URL("cam-00"))
	if err != nil {
		t.Fatalf("parse: %v", err)
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
	if len(desc.Medias) != 1 {
		t.Fatalf("medias = %d, want 1", len(desc.Medias))
	}

	var forma *format.H264
	if desc.FindFormat(&forma) == nil {
		t.Fatal("no H264 format in the SDP")
	}
	if string(forma.SPS) != string(want.SPS) {
		t.Error("advertised SPS differs from the source SPS")
	}
	if string(forma.PPS) != string(want.PPS) {
		t.Error("advertised PPS differs from the source PPS")
	}
	if forma.PacketizationMode != 1 {
		t.Errorf("packetization mode = %d, want 1", forma.PacketizationMode)
	}
}

func TestUnknownCameraIsNotFound(t *testing.T) {
	s := startServer(t, 1)

	u, err := base.ParseURL(strings.Replace(s.URL("cam-00"), "/cam-00", "/nope", 1))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	c, _ := newTestClient(u)
	if err := c.Start(); err != nil {
		t.Fatalf("client start: %v", err)
	}
	defer c.Close()

	if _, _, err := c.Describe(u); err == nil {
		t.Fatal("DESCRIBE of an unknown camera succeeded")
	} else if !strings.Contains(err.Error(), "404") {
		t.Errorf("err = %v, want a 404", err)
	}
}

// Every camera must be independently addressable behind the one listener.
func TestFleetIsIndependentlyAddressable(t *testing.T) {
	const n = 25
	s := startServer(t, n)

	for i := 0; i < n; i++ {
		id := cameraID(i)
		u, err := base.ParseURL(s.URL(id))
		if err != nil {
			t.Fatalf("parse %s: %v", id, err)
		}
		c, switchedToTCP := newTestClient(u)
		if err := c.Start(); err != nil {
			t.Fatalf("client start %s: %v", id, err)
		}
		desc, _, err := c.Describe(u)
		if err != nil {
			c.Close()
			t.Fatalf("DESCRIBE %s: %v", id, err)
		}
		if err := c.SetupAll(desc.BaseURL, desc.Medias); err != nil {
			c.Close()
			t.Fatalf("SETUP %s: %v", id, err)
		}
		if !switchedToTCP() {
			c.Close()
			t.Fatalf("%s: SETUP did not fall back to TCP; server advertises no UDP ports", id)
		}
		if _, err := c.Play(nil); err != nil {
			c.Close()
			t.Fatalf("PLAY %s: %v", id, err)
		}
		c.Close()
	}
}

// gortsplib exposes no reader count, so the server tracks it. Spec section 2.9:
// a consumer needs to assert connected clients without a leaked pointer.
func TestReaderCountRisesAndFalls(t *testing.T) {
	s := startServer(t, 1)

	if got := s.Readers("cam-00"); got != 0 {
		t.Fatalf("readers = %d before any client, want 0", got)
	}

	u, err := base.ParseURL(s.URL("cam-00"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	c, switchedToTCP := newTestClient(u)
	if err := c.Start(); err != nil {
		t.Fatalf("client start: %v", err)
	}
	desc, _, err := c.Describe(u)
	if err != nil {
		t.Fatalf("DESCRIBE: %v", err)
	}
	if err := c.SetupAll(desc.BaseURL, desc.Medias); err != nil {
		t.Fatalf("SETUP: %v", err)
	}
	if !switchedToTCP() {
		t.Fatal("SETUP did not fall back to TCP; server advertises no UDP ports")
	}
	if _, err := c.Play(nil); err != nil {
		t.Fatalf("PLAY: %v", err)
	}

	if got := waitForReaders(s, "cam-00", 1); got != 1 {
		t.Fatalf("readers = %d with one client, want 1", got)
	}

	c.Close()

	if got := waitForReaders(s, "cam-00", 0); got != 0 {
		t.Fatalf("readers = %d after close, want 0", got)
	}
}

// waitForReaders polls briefly: session teardown is asynchronous on the server.
func waitForReaders(s *Server, id string, want int) int {
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := s.Readers(id)
		if got == want || time.Now().After(deadline) {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestConcurrentCloseAndDescribe races Close against a batch of in-flight
// DESCRIBE requests. lookup resolves a camera under the read lock but used to
// hand back a *camera pointer for the caller to dereference afterward, so
// OnDescribe's read of cam.stream could race the cam.stream = nil write that
// Close performs under the write lock. What is guaranteed here is only that
// nothing panics and the race detector stays quiet: a request racing a
// shutdown is allowed to fail, so no success is asserted on any Describe.
func TestConcurrentCloseAndDescribe(t *testing.T) {
	const n = 20
	s := startServer(t, 1)

	u, err := base.ParseURL(s.URL("cam-00"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	clients := make([]*gortsplib.Client, n)
	for i := range clients {
		c, _ := newTestClient(u)
		if err := c.Start(); err != nil {
			t.Fatalf("client start %d: %v", i, err)
		}
		clients[i] = c
	}
	defer func() {
		for _, c := range clients {
			c.Close()
		}
	}()

	var wg sync.WaitGroup
	wg.Add(len(clients) + 1)
	for _, c := range clients {
		c := c
		go func() {
			defer wg.Done()
			c.Describe(u) //nolint:errcheck // racing a shutdown may legitimately fail
		}()
	}
	go func() {
		defer wg.Done()
		s.Close()
	}()
	wg.Wait()
}

func TestMediaReachesAClient(t *testing.T) {
	s := startServer(t, 2)

	for _, id := range []string{"cam-00", "cam-01"} {
		u, err := base.ParseURL(s.URL(id))
		if err != nil {
			t.Fatalf("parse %s: %v", id, err)
		}
		c := &gortsplib.Client{Scheme: u.Scheme, Host: u.Host}
		if err := c.Start(); err != nil {
			t.Fatalf("client start %s: %v", id, err)
		}
		desc, _, err := c.Describe(u)
		if err != nil {
			c.Close()
			t.Fatalf("DESCRIBE %s: %v", id, err)
		}
		if err := c.SetupAll(desc.BaseURL, desc.Medias); err != nil {
			c.Close()
			t.Fatalf("SETUP %s: %v", id, err)
		}
		got := make(chan struct{}, 1)
		c.OnPacketRTPAny(func(*description.Media, format.Format, *rtp.Packet) {
			select {
			case got <- struct{}{}:
			default:
			}
		})
		if _, err := c.Play(nil); err != nil {
			c.Close()
			t.Fatalf("PLAY %s: %v", id, err)
		}
		select {
		case <-got:
		case <-time.After(10 * time.Second):
			c.Close()
			t.Fatalf("no RTP from %s within 10s", id)
		}
		c.Close()
	}

	// The stream's own counters must agree that data went out.
	if pkts, bytes := s.StreamStats("cam-00"); pkts == 0 || bytes == 0 {
		t.Errorf("StreamStats = %d packets, %d bytes; want both non-zero", pkts, bytes)
	}
}

func TestRejectsBadConfig(t *testing.T) {
	m := testMedia(t)
	cases := map[string]Config{
		"no cameras":   {Host: "127.0.0.1", Obs: obs.New(1)},
		"empty id":     {Host: "127.0.0.1", Obs: obs.New(1), Cameras: []CameraConfig{{ID: "", Media: m}}},
		"nil media":    {Host: "127.0.0.1", Obs: obs.New(1), Cameras: []CameraConfig{{ID: "a"}}},
		"duplicate id": {Host: "127.0.0.1", Obs: obs.New(1), Cameras: []CameraConfig{{ID: "a", Media: m}, {ID: "a", Media: m}}},
	}
	for name, cfg := range cases {
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: New succeeded, want an error", name)
		}
	}
}

// TestH265CameraAdvertisesVPS proves an H.265 camera reaches the wire as H.265
// with all three parameter sets in its format, which is what a client needs to
// decode. The H.264 path is unchanged and covered by the existing tests.
func TestH265CameraAdvertisesVPS(t *testing.T) {
	m, err := (media.FixtureH265Source{}).Load()
	if err != nil {
		t.Fatalf("load H265 fixture: %v", err)
	}

	forma, err := newFormat(m)
	if err != nil {
		t.Fatalf("newFormat: %v", err)
	}
	h265, ok := forma.(*format.H265)
	if !ok {
		t.Fatalf("newFormat returned %T, want *format.H265", forma)
	}
	if !bytes.Equal(h265.VPS, m.VPS) {
		t.Error("format VPS does not match the media's")
	}
	if !bytes.Equal(h265.SPS, m.SPS) || !bytes.Equal(h265.PPS, m.PPS) {
		t.Error("format SPS/PPS does not match the media's")
	}
	if got := forma.RTPMap(); !strings.Contains(got, "H265") {
		t.Errorf("RTPMap = %q, want it to name H265", got)
	}
}

func TestNewFormatH264Unchanged(t *testing.T) {
	m, err := (media.FixtureSource{}).Load()
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	forma, err := newFormat(m)
	if err != nil {
		t.Fatalf("newFormat: %v", err)
	}
	h264, ok := forma.(*format.H264)
	if !ok {
		t.Fatalf("newFormat returned %T, want *format.H264", forma)
	}
	if h264.PacketizationMode != 1 {
		t.Errorf("PacketizationMode = %d, want 1", h264.PacketizationMode)
	}
}

func TestNewFormatRejectsUnknownCodec(t *testing.T) {
	if _, err := newFormat(&media.Media{Codec: media.Codec("VP9")}); err == nil {
		t.Fatal("expected an error for an unknown codec")
	}
}

// TestAddCameraRunningServerServesNewCamera proves a camera added after Start
// is immediately dialable, and that the camera present since Start is
// unaffected by the addition.
func TestAddCameraRunningServerServesNewCamera(t *testing.T) {
	s := startServer(t, 1)
	m := testMedia(t)

	if err := s.AddCamera(CameraConfig{ID: "extra", Media: m, Seed: seed.Seed(99)}); err != nil {
		t.Fatalf("AddCamera: %v", err)
	}

	for _, id := range []string{"cam-00", "extra"} {
		u, err := base.ParseURL(s.URL(id))
		if err != nil {
			t.Fatalf("parse %s: %v", id, err)
		}
		c := &gortsplib.Client{Scheme: u.Scheme, Host: u.Host}
		if err := c.Start(); err != nil {
			t.Fatalf("client start %s: %v", id, err)
		}
		desc, _, err := c.Describe(u)
		if err != nil {
			c.Close()
			t.Fatalf("DESCRIBE %s: %v", id, err)
		}
		if err := c.SetupAll(desc.BaseURL, desc.Medias); err != nil {
			c.Close()
			t.Fatalf("SETUP %s: %v", id, err)
		}
		got := make(chan struct{}, 1)
		c.OnPacketRTPAny(func(*description.Media, format.Format, *rtp.Packet) {
			select {
			case got <- struct{}{}:
			default:
			}
		})
		if _, err := c.Play(nil); err != nil {
			c.Close()
			t.Fatalf("PLAY %s: %v", id, err)
		}
		select {
		case <-got:
		case <-time.After(10 * time.Second):
			c.Close()
			t.Fatalf("no RTP from %s within 10s", id)
		}
		c.Close()
	}
}

func TestAddCameraBeforeStartRefused(t *testing.T) {
	m := testMedia(t)
	s, err := New(Config{Host: "127.0.0.1", Obs: obs.New(10), Cameras: []CameraConfig{
		{ID: "a", Media: m, Seed: seed.Seed(1)},
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.AddCamera(CameraConfig{ID: "b", Media: m, Seed: seed.Seed(2)}); !errors.Is(err, ErrNotStarted) {
		t.Fatalf("AddCamera before Start: err = %v, want ErrNotStarted", err)
	}
}

func TestAddCameraRejectsBadConfig(t *testing.T) {
	s := startServer(t, 1)
	m := testMedia(t)
	cases := map[string]CameraConfig{
		"empty id":     {ID: "", Media: m},
		"nil media":    {ID: "new"},
		"duplicate id": {ID: "cam-00", Media: m},
	}
	for name, cc := range cases {
		if err := s.AddCamera(cc); err == nil {
			t.Errorf("%s: AddCamera succeeded, want an error", name)
		}
	}
}

// TestH265CameraMissingVPSRefused proves the guard: H.265 needs three parameter
// sets, and a camera lacking VPS must be refused at construction rather than
// producing a stream no client can decode.
func TestH265CameraMissingVPSRefused(t *testing.T) {
	m, err := (media.FixtureH265Source{}).Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	stripped := *m
	stripped.VPS = nil

	_, err = New(Config{
		Obs:     obs.New(16),
		Cameras: []CameraConfig{{ID: "cam", Media: &stripped}},
	})
	if err == nil {
		t.Fatal("expected an error for H265 media with no VPS")
	}
	if !strings.Contains(err.Error(), "VPS") {
		t.Errorf("err = %q, want it to mention VPS", err.Error())
	}
}
