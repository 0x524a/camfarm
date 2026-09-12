package camfarm

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
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

func digestAuthorizationHeader(t *testing.T, username, password, method, uri, nonce string) string {
	t.Helper()
	h := md5.Sum([]byte(username + ":camfarm:" + password))
	ha1 := hex.EncodeToString(h[:])
	h2 := md5.Sum([]byte(method + ":" + uri))
	ha2 := hex.EncodeToString(h2[:])
	nc, cnonce := "00000001", "clienttest"
	resp := md5.Sum([]byte(strings.Join([]string{ha1, nonce, nc, cnonce, "auth", ha2}, ":")))
	response := hex.EncodeToString(resp[:])
	return fmt.Sprintf(`Digest username="%s", realm="camfarm", nonce="%s", uri="%s", qop=auth, nc=%s, cnonce="%s", response="%s"`,
		username, nonce, uri, nc, cnonce, response)
}

func extractWWWAuthNonce(t *testing.T, header string) string {
	t.Helper()
	const key = `nonce="`
	i := strings.Index(header, key)
	if i < 0 {
		t.Fatalf("WWW-Authenticate %q has no nonce", header)
	}
	rest := header[i+len(key):]
	return rest[:strings.IndexByte(rest, '"')]
}

func TestFleetLevelDigestFlow(t *testing.T) {
	f := StartT(t, Spec{Seed: 9, Cameras: []CameraSpec{
		{ID: "cam", Auth: AuthSpec{Username: "admin", Password: "secret"}},
	}})
	cam, err := f.Camera("cam")
	if err != nil {
		t.Fatalf("Camera: %v", err)
	}
	url := cam.ONVIFEndpoint() + "/device"
	uri := strings.TrimPrefix(url, "http://"+strings.SplitN(url, "/", 4)[2])

	body := `<?xml version="1.0"?><soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/"><soap:Body>` +
		`<GetDeviceInformation xmlns="http://www.onvif.org/ver10/device/wsdl"/></soap:Body></soap:Envelope>`

	// No Authorization header: challenged.
	resp, err := http.Post(url, "application/soap+xml", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	nonce := extractWWWAuthNonce(t, resp.Header.Get("WWW-Authenticate"))

	// Correct credentials against that nonce: succeeds.
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/soap+xml")
	req.Header.Set("Authorization", digestAuthorizationHeader(t, "admin", "secret", "POST", uri, nonce))
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST with auth: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 200 {
		out, _ := io.ReadAll(resp2.Body)
		t.Fatalf("status = %d, want 200\n%s", resp2.StatusCode, out)
	}

	// Wrong password against a fresh nonce: challenged again, not 200.
	req3, _ := http.NewRequest("POST", url, strings.NewReader(body))
	req3.Header.Set("Content-Type", "application/soap+xml")
	resp3, err := http.DefaultClient.Do(req3)
	if err != nil {
		t.Fatalf("POST (fresh nonce): %v", err)
	}
	resp3.Body.Close()
	nonce2 := extractWWWAuthNonce(t, resp3.Header.Get("WWW-Authenticate"))

	req4, _ := http.NewRequest("POST", url, strings.NewReader(body))
	req4.Header.Set("Content-Type", "application/soap+xml")
	req4.Header.Set("Authorization", digestAuthorizationHeader(t, "admin", "wrong", "POST", uri, nonce2))
	resp4, err := http.DefaultClient.Do(req4)
	if err != nil {
		t.Fatalf("POST with wrong password: %v", err)
	}
	defer resp4.Body.Close()
	if resp4.StatusCode != 401 {
		t.Errorf("status = %d, want 401 for wrong password", resp4.StatusCode)
	}
}

func TestSameSeedReproducesNonceSequence(t *testing.T) {
	spec := Spec{Seed: 0x1234, Cameras: []CameraSpec{
		{ID: "cam", Auth: AuthSpec{Username: "admin", Password: "secret"}},
	}}

	firstNonce := func() string {
		f := StartT(t, spec)
		cam, _ := f.Camera("cam")
		resp, err := http.Post(cam.ONVIFEndpoint()+"/device", "application/soap+xml", strings.NewReader(""))
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		defer resp.Body.Close()
		return extractWWWAuthNonce(t, resp.Header.Get("WWW-Authenticate"))
	}

	n1 := firstNonce()
	n2 := firstNonce()
	if n1 != n2 {
		t.Errorf("nonce = %q then %q, want equal for the same seed", n1, n2)
	}
}

func TestMixedFleetGetStreamUriPerCamera(t *testing.T) {
	h265Path := filepath.Join(t.TempDir(), "h265.ts")
	if err := os.WriteFile(h265Path, h265FixtureBytes(t), 0o600); err != nil {
		t.Fatalf("write H265 fixture: %v", err)
	}

	f := StartT(t, Spec{Seed: 11, Cameras: []CameraSpec{
		{ID: "open-h264"},
		{ID: "auth-h264", Auth: AuthSpec{Username: "admin", Password: "secret"}},
		{ID: "hevc-cam", Source: SourceSpec{Kind: SourceFile, Path: h265Path}},
	}})

	for _, id := range []string{"open-h264", "auth-h264", "hevc-cam"} {
		cam, err := f.Camera(id)
		if err != nil {
			t.Fatalf("Camera(%q): %v", id, err)
		}
		if cam.ONVIFEndpoint() == "" {
			t.Errorf("%s: empty ONVIFEndpoint", id)
		}
	}

	// The open camera answers GetStreamUri directly with its own URL.
	open, _ := f.Camera("open-h264")
	resp, body := soapPost(t, open.ONVIFEndpoint()+"/media",
		"http://www.onvif.org/ver10/media/wsdl", "GetStreamUri", "<ProfileToken>profile1</ProfileToken>")
	if resp.StatusCode != 200 || !strings.Contains(string(body), open.RTSPURL()) {
		t.Errorf("open-h264: status=%d body=%s, want 200 containing %q", resp.StatusCode, body, open.RTSPURL())
	}

	// The credentialed camera refuses the same unauthenticated request.
	auth, _ := f.Camera("auth-h264")
	resp2, _ := soapPost(t, auth.ONVIFEndpoint()+"/media",
		"http://www.onvif.org/ver10/media/wsdl", "GetStreamUri", "<ProfileToken>profile1</ProfileToken>")
	if resp2.StatusCode != 401 {
		t.Errorf("auth-h264: status = %d, want 401", resp2.StatusCode)
	}
}
