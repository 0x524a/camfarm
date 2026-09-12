package onvif

import (
	onvifserver "github.com/0x524a/onvif-go/server"
)

// handlerFunc adapts one onvif-go Handle* method to the []byte body seam
// this package always uses (media.go's unmarshalBody accepts []byte
// directly; the no-parameter operations ignore body entirely).
type handlerFunc func(*onvifserver.Server, []byte) (any, error)

// handlers maps the SOAP wire action name — the ONVIF Media WSDL spells
// GetStreamUri/GetSnapshotUri with a lowercase "Uri", which is what a real
// onvif.Client sends — to the onvif-go method that answers it.
var handlers = map[string]handlerFunc{
	"GetDeviceInformation": func(s *onvifserver.Server, _ []byte) (any, error) { return s.HandleGetDeviceInformation(nil) },
	"GetCapabilities":      func(s *onvifserver.Server, _ []byte) (any, error) { return s.HandleGetCapabilities(nil) },
	"GetSystemDateAndTime": func(s *onvifserver.Server, _ []byte) (any, error) { return s.HandleGetSystemDateAndTime(nil) },
	"GetServices":          func(s *onvifserver.Server, _ []byte) (any, error) { return s.HandleGetServices(nil) },
	"SystemReboot":         func(s *onvifserver.Server, _ []byte) (any, error) { return s.HandleSystemReboot(nil) },
	"GetProfiles":          func(s *onvifserver.Server, _ []byte) (any, error) { return s.HandleGetProfiles(nil) },
	"GetVideoSources":      func(s *onvifserver.Server, _ []byte) (any, error) { return s.HandleGetVideoSources(nil) },
	"GetStreamUri":         func(s *onvifserver.Server, body []byte) (any, error) { return s.HandleGetStreamURI(body) },
	"GetSnapshotUri":       func(s *onvifserver.Server, body []byte) (any, error) { return s.HandleGetSnapshotURI(body) },
}

// rewriteXAddr rewrites onvif-go's own XAddr fields, which are computed from
// a Config.Host/Port camfarm never binds (camfarm owns the listener; see
// server.go's New), to the camera's real per-service URL under this
// fleet's ONVIF listener.
func rewriteXAddr(resp any, deviceURL, mediaURL string) {
	switch r := resp.(type) {
	case *onvifserver.GetCapabilitiesResponse:
		if r.Capabilities.Device != nil {
			r.Capabilities.Device.XAddr = deviceURL
		}
		if r.Capabilities.Media != nil {
			r.Capabilities.Media.XAddr = mediaURL
		}
	case *onvifserver.GetServicesResponse:
		for i := range r.Service {
			switch r.Service[i].Namespace {
			case "http://www.onvif.org/ver10/device/wsdl":
				r.Service[i].XAddr = deviceURL
			case "http://www.onvif.org/ver10/media/wsdl":
				r.Service[i].XAddr = mediaURL
			}
		}
	}
}
