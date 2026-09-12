package camfarm

import (
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
