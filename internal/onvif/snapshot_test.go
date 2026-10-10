package onvif

import (
	"bytes"
	"image/jpeg"
	"io"
	"net/http"
	"strings"
	"testing"

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
