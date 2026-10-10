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
	"bytes"
	"context"
	"fmt"
	"image/jpeg"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	onvifserver "github.com/0x524a/onvif-go/server"

	"github.com/0x524a/camfarm/internal/media"
	"github.com/0x524a/camfarm/internal/screenshot"
	"github.com/0x524a/camfarm/internal/seed"
)

// profileToken is the single media profile every camera in this slice
// exposes. Multi-profile cameras are out of scope for slice 1.
const profileToken = "profile1"

// snapshotTimeout bounds one snapshot capture, so a source that never yields a
// random access point or stalls its RTSP handshake cannot pin the handler.
const snapshotTimeout = 15 * time.Second

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

	// Discovery, when non-nil, answers WS-Discovery probes for every camera.
	// Nil leaves discovery off.
	Discovery *DiscoveryConfig
}

type camera struct {
	id      string
	srv     *onvifserver.Server
	rtspURL string
	uuid    string      // seed-derived endpoint UUID, see EndpointUUID
	digest  *digestAuth // nil means open

	discoRef string // endpoint ref registered with the responder, "" if none
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

	// responder is the running WS-Discovery responder; nil when discovery is
	// disabled or the server is not started.
	responder *responderState

	// snapshotTimeout bounds each snapshot capture; tests shorten it.
	snapshotTimeout time.Duration
}

// New validates cfg and builds one onvif-go *server.Server per camera.
func New(cfg Config) (*Server, error) {
	if len(cfg.Cameras) == 0 {
		return nil, fmt.Errorf("onvif: no cameras configured")
	}
	if cfg.Host == "" {
		cfg.Host = "127.0.0.1"
	}
	if cfg.Discovery != nil {
		if err := cfg.Discovery.validate(); err != nil {
			return nil, err
		}
	}
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	s := &Server{cfg: cfg, log: log, snapshotTimeout: snapshotTimeout, cams: make(map[string]*camera, len(cfg.Cameras))}

	for _, cc := range cfg.Cameras {
		if err := validateCameraConfig(cc, s.cams); err != nil {
			return nil, err
		}
		cam, err := buildCamera(cc)
		if err != nil {
			return nil, err
		}
		s.cams[cc.ID] = cam
	}

	s.mux = http.NewServeMux()
	s.mux.HandleFunc("/onvif/", s.handle)
	return s, nil
}

// validateCameraConfig checks one camera's config against the server's
// existing cameras. Shared by New and AddCamera so the two paths cannot
// enforce different rules.
func validateCameraConfig(cc CameraConfig, existing map[string]*camera) error {
	if cc.ID == "" {
		return fmt.Errorf("onvif: camera with an empty ID")
	}
	if cc.Media == nil {
		return fmt.Errorf("onvif: camera %q has no media", cc.ID)
	}
	if _, dup := existing[cc.ID]; dup {
		return fmt.Errorf("onvif: duplicate camera ID %q", cc.ID)
	}
	return nil
}

// buildCamera constructs the onvif-go server and digest state for one
// camera. Shared by New and AddCamera.
func buildCamera(cc CameraConfig) (*camera, error) {
	uuid := EndpointUUID(cc.Seed)
	srv, err := onvifserver.New(&onvifserver.Config{
		EndpointUUID: uuid,
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
				Snapshot: onvifserver.SnapshotConfig{
					Enabled:    true,
					Resolution: onvifserver.Resolution{Width: cc.Media.Width, Height: cc.Media.Height},
				},
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("onvif: building server for %q: %w", cc.ID, err)
	}
	if err := srv.UpdateStreamURI(profileToken, cc.RTSPURL); err != nil {
		return nil, fmt.Errorf("onvif: wiring stream URI for %q: %w", cc.ID, err)
	}

	cam := &camera{id: cc.ID, srv: srv, rtspURL: cc.RTSPURL, uuid: uuid}
	if cc.Username != "" && cc.Password != "" {
		cam.digest = newDigestAuth(cc.Username, cc.Password, cc.Seed.Stream("onvif-auth"))
	}
	return cam, nil
}

// AddCamera registers a new camera's ONVIF endpoint. Safe to call whether or
// not Start has been called yet: unlike internal/rtsp, nothing here depends
// on a live listener, only on the in-memory dispatch map.
func (s *Server) AddCamera(cc CameraConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := validateCameraConfig(cc, s.cams); err != nil {
		return err
	}
	cam, err := buildCamera(cc)
	if err != nil {
		return err
	}
	s.cams[cc.ID] = cam
	if s.responder != nil {
		if err := s.registerLocked(s.responder.resp, cam); err != nil {
			// Unregister is a no-op if Register never took effect, and sends
			// Bye if it did, so no phantom device outlives the failure.
			s.responder.resp.Unregister(cam.discoRef)
			delete(s.cams, cc.ID)
			return err
		}
	}

	s.log.Info("camera added", "camera", cc.ID)
	return nil
}

// RemoveCamera removes a camera's ONVIF endpoint. There is no goroutine or
// stream to tear down on this side, so this is just the dispatch removal.
func (s *Server) RemoveCamera(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	cam, ok := s.cams[id]
	if !ok {
		return fmt.Errorf("onvif: unknown camera %q", id)
	}
	if s.responder != nil && cam.discoRef != "" {
		s.responder.resp.Unregister(cam.discoRef)
	}
	delete(s.cams, id)

	s.log.Info("camera removed", "camera", id)
	return nil
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
	if s.cfg.Discovery != nil {
		// Started only now that the ONVIF listener is bound, so the XAddrs
		// advertised in Hello and ProbeMatches carry the real port.
		st, err := s.startResponder()
		if err != nil {
			s.ln = nil
			s.mu.Unlock()
			_ = ln.Close()
			return err
		}
		s.responder = st
	}
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
	st := s.responder
	s.http = nil
	s.ln = nil
	s.responder = nil
	s.mu.Unlock()

	if st != nil {
		st.stop(s)
	}
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

func (s *Server) deviceURL(id string) string   { return s.Endpoint(id) + "/device" }
func (s *Server) mediaURL(id string) string    { return s.Endpoint(id) + "/media" }
func (s *Server) snapshotURL(id string) string { return s.Endpoint(id) + "/snapshot" }

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
	if !ok || (service != "device" && service != "media" && service != "snapshot") {
		http.NotFound(w, r)
		return
	}
	cam, ok := s.cameraByID(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if service == "snapshot" {
		s.handleSnapshot(w, r, cam)
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
	resp, herr := h(cam.srv, payload)
	if herr != nil {
		writeFault(w, "Server", herr.Error())
		return
	}

	rewriteXAddr(resp, s.deviceURL(id), s.mediaURL(id))
	rewriteSnapshotURI(resp, s.snapshotURL(id))
	writeResponse(w, resp)
}

// handleSnapshot serves one JPEG frame of the camera's live RTSP stream. It is
// the HTTP endpoint GetSnapshotUri advertises, gated behind the same Digest
// auth as the SOAP services.
func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request, cam *camera) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if cam.digest != nil && !cam.digest.verify(r) {
		cam.digest.challenge(w)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.snapshotTimeout)
	defer cancel()
	img, err := screenshot.Capture(ctx, cam.rtspURL)
	if err != nil {
		s.log.Warn("snapshot capture failed", "camera", cam.id, "err", err)
		http.Error(w, "snapshot capture failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		s.log.Warn("snapshot encode failed", "camera", cam.id, "err", err)
		http.Error(w, "snapshot encode failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	_, _ = w.Write(buf.Bytes())
}
