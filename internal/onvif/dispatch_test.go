package onvif

import (
	"io"
	"testing"

	onvifserver "github.com/0x524a/onvif-go/server"
)

func newTestOnvifServer(t *testing.T) *onvifserver.Server {
	t.Helper()
	srv, err := onvifserver.New(&onvifserver.Config{
		DeviceInfo: onvifserver.DeviceInfo{Manufacturer: "camfarm", Model: "synthetic"},
		Output:     io.Discard,
		Profiles: []onvifserver.ProfileConfig{{
			Token: "profile1",
			Name:  "cam",
			VideoSource: onvifserver.VideoSourceConfig{
				Token: "profile1_source", Resolution: onvifserver.Resolution{Width: 320, Height: 240}, Framerate: 30,
			},
			VideoEncoder: onvifserver.VideoEncoderConfig{
				Encoding: "H264", Resolution: onvifserver.Resolution{Width: 320, Height: 240}, Framerate: 30,
			},
		}},
	})
	if err != nil {
		t.Fatalf("onvifserver.New: %v", err)
	}
	if err := srv.UpdateStreamURI("profile1", "rtsp://127.0.0.1:9/cam"); err != nil {
		t.Fatalf("UpdateStreamURI: %v", err)
	}
	return srv
}

func TestHandlersCoverEverySlice1Action(t *testing.T) {
	want := []string{
		"GetDeviceInformation", "GetCapabilities", "GetSystemDateAndTime",
		"GetServices", "SystemReboot", "GetProfiles", "GetVideoSources",
		"GetStreamUri", "GetSnapshotUri",
	}
	for _, action := range want {
		if _, ok := handlers[action]; !ok {
			t.Errorf("handlers[%q] missing", action)
		}
	}
}

func TestHandlerGetStreamUriReturnsRealURL(t *testing.T) {
	srv := newTestOnvifServer(t)
	body := []byte(`<GetStreamUri xmlns="http://www.onvif.org/ver10/media/wsdl"><ProfileToken>profile1</ProfileToken></GetStreamUri>`)

	resp, err := handlers["GetStreamUri"](srv, body)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	got, ok := resp.(*onvifserver.GetStreamURIResponse)
	if !ok {
		t.Fatalf("response type = %T, want *GetStreamURIResponse", resp)
	}
	if got.MediaURI.URI != "rtsp://127.0.0.1:9/cam" {
		t.Errorf("URI = %q, want the real RTSP URL", got.MediaURI.URI)
	}
}

func TestRewriteXAddrOnCapabilities(t *testing.T) {
	resp := &onvifserver.GetCapabilitiesResponse{
		Capabilities: &onvifserver.Capabilities{
			Device: &onvifserver.DeviceCapabilities{XAddr: "http://wrong/device_service"},
			Media:  &onvifserver.MediaCapabilities{XAddr: "http://wrong/media_service"},
		},
	}
	rewriteXAddr(resp, "http://real/onvif/cam/device", "http://real/onvif/cam/media")

	if resp.Capabilities.Device.XAddr != "http://real/onvif/cam/device" {
		t.Errorf("Device.XAddr = %q", resp.Capabilities.Device.XAddr)
	}
	if resp.Capabilities.Media.XAddr != "http://real/onvif/cam/media" {
		t.Errorf("Media.XAddr = %q", resp.Capabilities.Media.XAddr)
	}
}

func TestRewriteXAddrOnServices(t *testing.T) {
	resp := &onvifserver.GetServicesResponse{
		Service: []onvifserver.Service{
			{Namespace: "http://www.onvif.org/ver10/device/wsdl", XAddr: "http://wrong/device_service"},
			{Namespace: "http://www.onvif.org/ver10/media/wsdl", XAddr: "http://wrong/media_service"},
		},
	}
	rewriteXAddr(resp, "http://real/onvif/cam/device", "http://real/onvif/cam/media")

	if resp.Service[0].XAddr != "http://real/onvif/cam/device" {
		t.Errorf("Service[0].XAddr = %q", resp.Service[0].XAddr)
	}
	if resp.Service[1].XAddr != "http://real/onvif/cam/media" {
		t.Errorf("Service[1].XAddr = %q", resp.Service[1].XAddr)
	}
}
