package camfarm

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestONVIFEndpointServesGetStreamUri(t *testing.T) {
	f := StartT(t, Spec{Seed: 1, Cameras: []CameraSpec{{ID: "front-door"}}})

	cam, err := f.Camera("front-door")
	if err != nil {
		t.Fatalf("Camera: %v", err)
	}
	endpoint := cam.ONVIFEndpoint()
	if endpoint == "" {
		t.Fatal("ONVIFEndpoint is empty")
	}

	body := `<?xml version="1.0"?><soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/"><soap:Body>` +
		`<GetStreamUri xmlns="http://www.onvif.org/ver10/media/wsdl"><ProfileToken>profile1</ProfileToken></GetStreamUri>` +
		`</soap:Body></soap:Envelope>`
	resp, err := http.Post(endpoint+"/media", "application/soap+xml", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestCloseStopsONVIFServer(t *testing.T) {
	f, err := Start(nil, Spec{Seed: 1, Cameras: []CameraSpec{{ID: "a"}}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	cam, _ := f.Camera("a")
	endpoint := cam.ONVIFEndpoint()

	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := http.Get(endpoint + "/device"); err == nil {
		t.Fatal("ONVIF server still answering after Close")
	}
}

func soapPost(t *testing.T, url, ns, action, innerXML string) (*http.Response, []byte) {
	t.Helper()
	body := `<?xml version="1.0"?><soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/"><soap:Body>` +
		`<` + action + ` xmlns="` + ns + `">` + innerXML + `</` + action + `>` +
		`</soap:Body></soap:Envelope>`
	resp, err := http.Post(url, "application/soap+xml", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading response: %v", err)
	}
	return resp, out
}

func TestEveryDeviceAndMediaOperation(t *testing.T) {
	f := StartT(t, Spec{Seed: 7, Cameras: []CameraSpec{{ID: "cam"}}})
	cam, err := f.Camera("cam")
	if err != nil {
		t.Fatalf("Camera: %v", err)
	}
	endpoint := cam.ONVIFEndpoint()
	const deviceNS = "http://www.onvif.org/ver10/device/wsdl"
	const mediaNS = "http://www.onvif.org/ver10/media/wsdl"

	cases := []struct {
		name, path, ns, action, inner, want string
	}{
		{"GetDeviceInformation", "/device", deviceNS, "GetDeviceInformation", "", "camfarm"},
		{"GetCapabilities", "/device", deviceNS, "GetCapabilities", "", endpoint + "/device"},
		{"GetSystemDateAndTime", "/device", deviceNS, "GetSystemDateAndTime", "", "UTCDateTime"},
		{"GetServices", "/device", deviceNS, "GetServices", "", endpoint + "/media"},
		{"SystemReboot", "/device", deviceNS, "SystemReboot", "", "SystemRebootResponse"},
		{"GetProfiles", "/media", mediaNS, "GetProfiles", "", "profile1"},
		{"GetVideoSources", "/media", mediaNS, "GetVideoSources", "", "GetVideoSourcesResponse"},
		// Snapshot is disabled in this camera's profile config (server.go's
		// Snapshot: SnapshotConfig{Enabled: false}), and this project has no
		// endpoint behind the URL a snapshot response would advertise, so a
		// fault is the correct simulated behavior here, not a placeholder.
		{"GetSnapshotUri", "/media", mediaNS, "GetSnapshotUri", "<ProfileToken>profile1</ProfileToken>", "snapshot not supported for profile"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := soapPost(t, endpoint+tc.path, tc.ns, tc.action, tc.inner)
			if resp.StatusCode != 200 {
				t.Fatalf("status = %d, want 200\n%s", resp.StatusCode, body)
			}
			if !strings.Contains(string(body), tc.want) {
				t.Errorf("body = %s, want it to contain %q", body, tc.want)
			}
		})
	}
}

func TestGetStreamUriReturnsCamfarmsRealURL(t *testing.T) {
	f := StartT(t, Spec{Seed: 7, Cameras: []CameraSpec{{ID: "cam"}}})
	cam, err := f.Camera("cam")
	if err != nil {
		t.Fatalf("Camera: %v", err)
	}

	resp, body := soapPost(t, cam.ONVIFEndpoint()+"/media",
		"http://www.onvif.org/ver10/media/wsdl", "GetStreamUri", "<ProfileToken>profile1</ProfileToken>")
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200\n%s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), cam.RTSPURL()) {
		t.Errorf("body = %s, want it to contain the real RTSP URL %q", body, cam.RTSPURL())
	}
}
