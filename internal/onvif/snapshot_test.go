package onvif

import (
	"bytes"
	"image/jpeg"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0x524a/camfarm/internal/media"
	"github.com/0x524a/camfarm/internal/obs"
	"github.com/0x524a/camfarm/internal/rtsp"
	"github.com/0x524a/camfarm/internal/seed"
)

// startRealRTSP serves the bundled fixture over a real RTSP server and returns
// its media and the camera's URL, so snapshot tests exercise the real wire.
func startRealRTSP(t *testing.T) (*media.Media, string) {
	t.Helper()
	m, err := (media.FixtureSource{}).Load()
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	rs, err := rtsp.New(rtsp.Config{
		Host: "127.0.0.1", Obs: obs.New(1000),
		Cameras: []rtsp.CameraConfig{{ID: "cam", Media: m, Seed: seed.Seed(3)}},
	})
	if err != nil {
		t.Fatalf("rtsp.New: %v", err)
	}
	if err := rs.Start(); err != nil {
		t.Fatalf("rtsp.Start: %v", err)
	}
	t.Cleanup(rs.Close)
	return m, rs.URL("cam")
}

func TestSnapshotEndpointServesJPEG(t *testing.T) {
	m, url := startRealRTSP(t)
	srv := startTestServer(t, CameraConfig{ID: "cam", Media: m, RTSPURL: url, Seed: seed.Seed(1)})

	resp, err := http.Get(srv.Endpoint("cam") + "/snapshot")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("Content-Type = %q, want image/jpeg", ct)
	}
	img, err := jpeg.Decode(resp.Body)
	if err != nil {
		t.Fatalf("body is not a JPEG: %v", err)
	}
	if b := img.Bounds(); b.Dx() != 320 || b.Dy() != 240 {
		t.Errorf("bounds = %v, want 320x240", b)
	}
}

func TestSnapshotUnknownCameraIs404(t *testing.T) {
	srv := startTestServer(t, CameraConfig{ID: "cam", Media: testMedia(), RTSPURL: "rtsp://127.0.0.1:9/cam", Seed: seed.Seed(1)})
	resp, err := http.Get("http://" + srv.Addr().String() + "/onvif/nope/snapshot")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestSnapshotCaptureFailureIs500(t *testing.T) {
	// Port 9 (discard) has no RTSP server: the dial fails for real.
	srv := startTestServer(t, CameraConfig{ID: "cam", Media: testMedia(), RTSPURL: "rtsp://127.0.0.1:9/cam", Seed: seed.Seed(1)})
	resp, err := http.Get(srv.Endpoint("cam") + "/snapshot")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 500 {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "snapshot capture failed") {
		t.Errorf("body = %q, want it to mention the capture failure", body)
	}
}

// A source that accepts the TCP connection but never speaks RTSP must not pin
// the handler: the capture deadline fires, the handler answers 500, and the
// server keeps serving afterwards.
func TestSnapshotTimesOutOnStalledSource(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	var mu sync.Mutex
	var held []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, c)
			mu.Unlock()
		}
	}()
	defer func() {
		mu.Lock()
		for _, c := range held {
			c.Close()
		}
		mu.Unlock()
	}()

	m, goodURL := startRealRTSP(t)
	srv, err := New(Config{Host: "127.0.0.1", Cameras: []CameraConfig{
		{ID: "stall", Media: m, RTSPURL: "rtsp://" + ln.Addr().String() + "/cam", Seed: seed.Seed(1)},
		{ID: "good", Media: m, RTSPURL: goodURL, Seed: seed.Seed(2)},
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv.snapshotTimeout = 3 * time.Second
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(srv.Close)

	start := time.Now()
	resp, err := http.Get(srv.Endpoint("stall") + "/snapshot")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 500 {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
	if !strings.Contains(string(body), "snapshot capture failed") {
		t.Errorf("body = %q, want capture failure text", body)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("handler took %v, want well under the default %v", d, snapshotTimeout)
	}

	// The server still works for a healthy camera.
	resp2, err := http.Get(srv.Endpoint("good") + "/snapshot")
	if err != nil {
		t.Fatalf("GET good: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Errorf("good camera status = %d, want 200", resp2.StatusCode)
	}
}

func TestSnapshotRequiresDigestWhenCredentialed(t *testing.T) {
	srv := startTestServer(t, CameraConfig{
		ID: "cam", Media: testMedia(), RTSPURL: "rtsp://127.0.0.1:9/cam",
		Username: "admin", Password: "secret", Seed: seed.Seed(1),
	})
	resp, err := http.Get(srv.Endpoint("cam") + "/snapshot")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
	if resp.Header.Get("WWW-Authenticate") == "" {
		t.Error("missing WWW-Authenticate header")
	}
}

func TestSnapshotRejectsNonGET(t *testing.T) {
	srv := startTestServer(t, CameraConfig{ID: "cam", Media: testMedia(), RTSPURL: "rtsp://127.0.0.1:9/cam", Seed: seed.Seed(1)})
	resp, err := http.Post(srv.Endpoint("cam")+"/snapshot", "text/plain", strings.NewReader("x"))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 405 {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
}

func TestSnapshotDigestHandshakeServesJPEGAndConsumesNonce(t *testing.T) {
	m, url := startRealRTSP(t)
	srv := startTestServer(t, CameraConfig{
		ID: "cam", Media: m, RTSPURL: url,
		Username: "admin", Password: "secret", Seed: seed.Seed(1),
	})
	target := srv.Endpoint("cam") + "/snapshot"
	path := "/onvif/cam/snapshot"

	// First request: no credentials, expect a challenge.
	resp, err := http.Get(target)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("first status = %d, want 401", resp.StatusCode)
	}
	nonce := extractNonce(t, resp.Header.Get("WWW-Authenticate"))
	authz := digestAuthorization("admin", "secret", http.MethodGet, path, authRealm, nonce)

	get := func() *http.Response {
		req, err := http.NewRequest(http.MethodGet, target, nil)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		req.Header.Set("Authorization", authz)
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		return r
	}

	ok := get()
	defer ok.Body.Close()
	if ok.StatusCode != 200 {
		t.Fatalf("authorized status = %d, want 200", ok.StatusCode)
	}
	if ct := ok.Header.Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("Content-Type = %q, want image/jpeg", ct)
	}
	img, err := jpeg.Decode(ok.Body)
	if err != nil {
		t.Fatalf("body is not a JPEG: %v", err)
	}
	if b := img.Bounds(); b.Dx() != 320 || b.Dy() != 240 {
		t.Errorf("bounds = %v, want 320x240", b)
	}

	replay := get()
	defer replay.Body.Close()
	if replay.StatusCode != 401 {
		t.Errorf("replayed Authorization status = %d, want 401 (nonce consumed)", replay.StatusCode)
	}
}

func TestGetSnapshotUriAdvertisesTheRealEndpoint(t *testing.T) {
	srv := startTestServer(t, CameraConfig{ID: "cam", Media: testMedia(), RTSPURL: "rtsp://127.0.0.1:9/cam", Seed: seed.Seed(1)})

	body := `<?xml version="1.0"?><soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/"><soap:Body>` +
		`<GetSnapshotUri xmlns="http://www.onvif.org/ver10/media/wsdl"><ProfileToken>profile1</ProfileToken></GetSnapshotUri>` +
		`</soap:Body></soap:Envelope>`
	resp, err := http.Post(srv.Endpoint("cam")+"/media", soapContentType, strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	want := srv.Endpoint("cam") + "/snapshot"
	if !bytes.Contains(out, []byte(want)) {
		t.Errorf("response = %s, want it to carry %q", out, want)
	}
}
