// Package onvif serves the ONVIF Device and Media control plane for a
// camfarm fleet.
//
// One onvif-go *server.Server per camera holds that camera's profile and
// stream state; camfarm owns the net.Listener, the http.ServeMux, and every
// byte of the SOAP transport (envelope.go, auth.go, dispatch.go) — onvif-go's
// own Start(ctx) and soap.Handler.ServeHTTP are never called. See
// docs/superpowers/specs/2026-09-09-onvif-control-plane-design.md.
package onvif

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"

	onvifserver "github.com/0x524a/onvif-go/server"

	"github.com/0x524a/camfarm/internal/media"
	"github.com/0x524a/camfarm/internal/seed"
)

// profileToken is the single media profile every camera in this slice
// exposes. Multi-profile cameras are out of scope for slice 1.
const profileToken = "profile1"

// CameraConfig describes one camera's ONVIF endpoint.
type CameraConfig struct {
	ID       string
	Media    *media.Media
	RTSPURL  string
	Username string
	Password string
	Seed     seed.Seed
}

// Config configures the ONVIF transport for a fleet.
type Config struct {
	// Host defaults to 127.0.0.1.
	Host string
	// Port of zero requests an ephemeral port, readable back via Addr.
	Port int
	// Log receives server events. Nil means silent.
	Log *slog.Logger

	Cameras []CameraConfig
}

type camera struct {
	id     string
	srv    *onvifserver.Server
	digest *digestAuth // nil means open
}

// Server serves the ONVIF control plane for a fleet, behind one HTTP
// listener shared by every camera.
type Server struct {
	cfg Config
	log *slog.Logger
	mux *http.ServeMux

	mu   sync.RWMutex
	ln   net.Listener
	http *http.Server
	cams map[string]*camera
}

// New validates cfg and builds one onvif-go *server.Server per camera.
func New(cfg Config) (*Server, error) {
	if len(cfg.Cameras) == 0 {
		return nil, fmt.Errorf("onvif: no cameras configured")
	}
	if cfg.Host == "" {
		cfg.Host = "127.0.0.1"
	}
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	s := &Server{cfg: cfg, log: log, cams: make(map[string]*camera, len(cfg.Cameras))}

	for _, cc := range cfg.Cameras {
		if cc.ID == "" {
			return nil, fmt.Errorf("onvif: camera with an empty ID")
		}
		if _, dup := s.cams[cc.ID]; dup {
			return nil, fmt.Errorf("onvif: duplicate camera ID %q", cc.ID)
		}
		if cc.Media == nil {
			return nil, fmt.Errorf("onvif: camera %q has no media", cc.ID)
		}

		srv, err := onvifserver.New(&onvifserver.Config{
			DeviceInfo: onvifserver.DeviceInfo{
				Manufacturer:    "camfarm",
				Model:           "synthetic",
				FirmwareVersion: "0.0.0",
				SerialNumber:    cc.ID,
				HardwareID:      cc.ID,
			},
			SupportPTZ:     false,
			SupportImaging: false,
			SupportEvents:  false,
			Output:         io.Discard,
			Profiles: []onvifserver.ProfileConfig{
				{
					Token: profileToken,
					Name:  cc.ID,
					VideoSource: onvifserver.VideoSourceConfig{
						Token:      profileToken + "_source",
						Name:       cc.ID,
						Resolution: onvifserver.Resolution{Width: cc.Media.Width, Height: cc.Media.Height},
						Framerate:  int(cc.Media.FPS),
						Bounds:     onvifserver.Bounds{Width: cc.Media.Width, Height: cc.Media.Height},
					},
					VideoEncoder: onvifserver.VideoEncoderConfig{
						Encoding:   string(cc.Media.Codec),
						Resolution: onvifserver.Resolution{Width: cc.Media.Width, Height: cc.Media.Height},
						Framerate:  int(cc.Media.FPS),
					},
					Snapshot: onvifserver.SnapshotConfig{Enabled: false},
				},
			},
		})
		if err != nil {
			return nil, fmt.Errorf("onvif: building server for %q: %w", cc.ID, err)
		}
		if err := srv.UpdateStreamURI(profileToken, cc.RTSPURL); err != nil {
			return nil, fmt.Errorf("onvif: wiring stream URI for %q: %w", cc.ID, err)
		}

		cam := &camera{id: cc.ID, srv: srv}
		if cc.Username != "" && cc.Password != "" {
			cam.digest = newDigestAuth(cc.Username, cc.Password, cc.Seed.Stream("onvif-auth"))
		}
		s.cams[cc.ID] = cam
	}

	s.mux = http.NewServeMux()
	s.mux.HandleFunc("/onvif/", s.handle)
	return s, nil
}

// Start binds the listener and begins serving.
func (s *Server) Start() error {
	s.mu.Lock()
	if s.ln != nil || s.http != nil {
		s.mu.Unlock()
		return fmt.Errorf("onvif: already started")
	}

	ln, err := net.Listen("tcp", net.JoinHostPort(s.cfg.Host, strconv.Itoa(s.cfg.Port)))
	if err != nil {
		s.mu.Unlock()
		return fmt.Errorf("onvif: listen: %w", err)
	}
	httpSrv := &http.Server{Handler: s.mux}

	s.ln = ln
	s.http = httpSrv
	s.mu.Unlock()

	go func() {
		if err := httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			s.log.Warn("onvif server stopped", "err", err)
		}
	}()

	s.log.Info("onvif server started", "addr", ln.Addr().String(), "cameras", len(s.cams))
	return nil
}

// Close stops the server. Safe to call more than once.
func (s *Server) Close() {
	s.mu.Lock()
	httpSrv := s.http
	s.http = nil
	s.ln = nil
	s.mu.Unlock()

	if httpSrv != nil {
		_ = httpSrv.Close()
	}
}

// Addr returns the bound address, or nil before a successful Start or after
// Close.
func (s *Server) Addr() *net.TCPAddr {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.ln == nil {
		return nil
	}
	addr, ok := s.ln.Addr().(*net.TCPAddr)
	if !ok {
		return nil
	}
	return addr
}

// Endpoint returns the base ONVIF URL for a camera (e.g.
// "http://127.0.0.1:PORT/onvif/cam"), or "" if the camera is unknown, before
// a successful Start, or after Close.
func (s *Server) Endpoint(cameraID string) string {
	addr := s.Addr()
	if addr == nil {
		return ""
	}
	s.mu.RLock()
	_, ok := s.cams[cameraID]
	s.mu.RUnlock()
	if !ok {
		return ""
	}
	return fmt.Sprintf("http://%s/onvif/%s", addr.String(), cameraID)
}

func (s *Server) deviceURL(id string) string { return s.Endpoint(id) + "/device" }
func (s *Server) mediaURL(id string) string  { return s.Endpoint(id) + "/media" }

func (s *Server) cameraByID(id string) (*camera, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cam, ok := s.cams[id]
	return cam, ok
}

// parsePath parses "/onvif/{cameraID}/{service}" into its parts.
func parsePath(p string) (cameraID, service string, ok bool) {
	p = strings.TrimPrefix(p, "/onvif/")
	parts := strings.SplitN(p, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// handle routes one HTTP request to its camera, gates it behind Digest auth
// when the camera is credentialed, and dispatches the SOAP action inside.
func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	id, service, ok := parsePath(r.URL.Path)
	if !ok || (service != "device" && service != "media") {
		http.NotFound(w, r)
		return
	}
	cam, ok := s.cameraByID(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if cam.digest != nil && !cam.digest.verify(r) {
		cam.digest.challenge(w)
		return
	}

	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeFault(w, "Client", "failed to read request body")
		return
	}

	actionName, payload, err := parseAction(raw)
	if err != nil {
		writeFault(w, "Client", "invalid SOAP envelope")
		return
	}

	h, ok := handlers[actionName]
	if !ok {
		writeFault(w, "Client", "Action not supported")
		return
	}

	resp, err := h(cam.srv, payload)
	if err != nil {
		writeFault(w, "Server", err.Error())
		return
	}

	rewriteXAddr(resp, s.deviceURL(id), s.mediaURL(id))
	writeResponse(w, resp)
}
