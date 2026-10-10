package rtsp

import (
	"bytes"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/format"

	"github.com/0x524a/camfarm/internal/obs"
	"github.com/0x524a/camfarm/internal/seed"
)

func startSpikeServer(t *testing.T, install func(s *Server)) *Server {
	t.Helper()
	s, err := New(Config{Host: "127.0.0.1", Obs: obs.New(1000), Cameras: []CameraConfig{
		{ID: "cam-00", Media: testMedia(t), Seed: seed.Seed(21)},
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	install(s)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

// Pins that the finaliser sees the SDP body, Content-Type and Content-Base
// already set by gortsplib, and that rewriting them reaches a real client.
func TestPinOnResponseCanRewriteDescribe(t *testing.T) {
	var mu sync.Mutex
	var sawBody, sawBase string
	s := startSpikeServer(t, func(s *Server) {
		s.testOnResponse = func(_ *gortsplib.ServerConn, res *base.Response) {
			if len(res.Header["Content-Type"]) == 0 || res.Header["Content-Type"][0] != "application/sdp" {
				return
			}
			mu.Lock()
			sawBody, sawBase = string(res.Body), res.Header["Content-Base"][0]
			mu.Unlock()
			res.Body = bytes.ReplaceAll(res.Body, []byte("H264/90000"), []byte("H265/90000"))
			res.Header["Content-Base"] = base.HeaderValue{"rtsp://wrong.invalid/"}
		}
	})
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
	mu.Lock()
	defer mu.Unlock()
	t.Logf("OnResponse saw Content-Base %q", sawBase)
	if !strings.Contains(sawBody, "H264/90000") {
		t.Fatalf("OnResponse did not see the SDP body: %q", sawBody)
	}
	if !strings.HasSuffix(sawBase, "/cam-00/") {
		t.Fatalf("OnResponse did not see the path in Content-Base: %q", sawBase)
	}
	if len(desc.Medias) == 0 || len(desc.Medias[0].Formats) == 0 {
		t.Fatal("no media in description")
	}
	if _, ok := desc.Medias[0].Formats[0].(*format.H265); !ok {
		t.Fatalf("first format is %T, want *format.H265", desc.Medias[0].Formats[0])
	}
	if desc.BaseURL.Host != "wrong.invalid" {
		t.Fatalf("Content-Base not rewritten: %v", desc.BaseURL)
	}
}

// Pins that closing the underlying TCP conn with linger 0 reaches a real
// client as a connection reset.
func TestPinNetConnResetReachesClient(t *testing.T) {
	s := startSpikeServer(t, func(s *Server) {
		s.testOnPlay = func(ctx *gortsplib.ServerHandlerOnPlayCtx) {
			nc := ctx.Conn.NetConn()
			time.AfterFunc(100*time.Millisecond, func() {
				if tc, ok := nc.(*net.TCPConn); ok {
					_ = tc.SetLinger(0)
				}
				_ = nc.Close()
			})
		}
	})
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
	if _, err := c.Play(nil); err != nil {
		t.Fatalf("PLAY: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	select {
	case err := <-done:
		t.Logf("client Wait error: %v", err)
		if err == nil || !strings.Contains(err.Error(), "reset") {
			t.Fatalf("want a connection reset, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("client never saw the connection end")
	}
}
