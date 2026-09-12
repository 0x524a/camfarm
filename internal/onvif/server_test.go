package onvif

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/0x524a/camfarm/internal/media"
	"github.com/0x524a/camfarm/internal/seed"
)

func testMedia() *media.Media {
	return &media.Media{Codec: media.CodecH264, Width: 320, Height: 240, FPS: 30}
}

func startTestServer(t *testing.T, cams ...CameraConfig) *Server {
	t.Helper()
	srv, err := New(Config{Host: "127.0.0.1", Cameras: cams})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(srv.Close)
	return srv
}

func soapRequest(t *testing.T, url, action string) *http.Response {
	t.Helper()
	body := `<?xml version="1.0"?><soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/"><soap:Body>` +
		`<` + action + ` xmlns="http://www.onvif.org/ver10/device/wsdl"/>` +
		`</soap:Body></soap:Envelope>`
	resp, err := http.Post(url, soapContentType, strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return resp
}

func TestServerAnswersGetDeviceInformation(t *testing.T) {
	srv := startTestServer(t, CameraConfig{ID: "cam", Media: testMedia(), RTSPURL: "rtsp://127.0.0.1:9/cam", Seed: seed.Seed(1)})

	resp := soapRequest(t, srv.Endpoint("cam")+"/device", "GetDeviceInformation")
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	out, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(out, []byte("camfarm")) {
		t.Errorf("body = %q, want manufacturer camfarm", out)
	}
}

func TestServerUnknownCameraReturns404(t *testing.T) {
	srv := startTestServer(t, CameraConfig{ID: "cam", Media: testMedia(), RTSPURL: "rtsp://127.0.0.1:9/cam", Seed: seed.Seed(1)})

	resp, err := http.Get(srv.Endpoint("nope") + "/device")
	if err != nil {
		// Endpoint("nope") returns "" for an unknown camera, so build the URL
		// by hand against the real bound address instead.
		resp, err = http.Get("http://" + srv.Addr().String() + "/onvif/nope/device")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestServerUnknownActionReturnsFault(t *testing.T) {
	srv := startTestServer(t, CameraConfig{ID: "cam", Media: testMedia(), RTSPURL: "rtsp://127.0.0.1:9/cam", Seed: seed.Seed(1)})

	resp := soapRequest(t, srv.Endpoint("cam")+"/device", "GetPresets")
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 (a fault envelope)", resp.StatusCode)
	}
	out, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(out, []byte("soap:Fault")) {
		t.Errorf("body = %q, want a SOAP fault", out)
	}
}

func TestServerCredentialedCameraChallenges(t *testing.T) {
	srv := startTestServer(t, CameraConfig{
		ID: "cam", Media: testMedia(), RTSPURL: "rtsp://127.0.0.1:9/cam",
		Username: "admin", Password: "secret", Seed: seed.Seed(1),
	})

	resp := soapRequest(t, srv.Endpoint("cam")+"/device", "GetDeviceInformation")
	defer resp.Body.Close()

	if resp.StatusCode != 401 {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
	if resp.Header.Get("WWW-Authenticate") == "" {
		t.Error("missing WWW-Authenticate header")
	}
}

func TestServerStartTwiceFails(t *testing.T) {
	srv := startTestServer(t, CameraConfig{ID: "cam", Media: testMedia(), RTSPURL: "rtsp://127.0.0.1:9/cam", Seed: seed.Seed(1)})

	if err := srv.Start(); err == nil {
		t.Error("second Start() succeeded, want an error")
	}
}

func TestServerAddrNilAfterClose(t *testing.T) {
	srv, err := New(Config{Host: "127.0.0.1", Cameras: []CameraConfig{
		{ID: "cam", Media: testMedia(), RTSPURL: "rtsp://127.0.0.1:9/cam", Seed: seed.Seed(1)},
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if srv.Addr() == nil {
		t.Fatal("Addr() = nil after Start, want a bound address")
	}

	srv.Close()

	if addr := srv.Addr(); addr != nil {
		t.Errorf("Addr() = %v after Close, want nil", addr)
	}
	if ep := srv.Endpoint("cam"); ep != "" {
		t.Errorf("Endpoint(%q) = %q after Close, want \"\"", "cam", ep)
	}
}

func TestServerTwoCamerasIndependentEndpoints(t *testing.T) {
	srv := startTestServer(t,
		CameraConfig{ID: "a", Media: testMedia(), RTSPURL: "rtsp://127.0.0.1:9/a", Seed: seed.Seed(1)},
		CameraConfig{ID: "b", Media: testMedia(), RTSPURL: "rtsp://127.0.0.1:9/b", Seed: seed.Seed(2)},
	)

	if srv.Endpoint("a") == srv.Endpoint("b") {
		t.Fatal("two cameras got the same endpoint")
	}
	for _, id := range []string{"a", "b"} {
		resp := soapRequest(t, srv.Endpoint(id)+"/media", "GetProfiles")
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("%s: status = %d, want 200", id, resp.StatusCode)
		}
	}
}
