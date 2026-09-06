// Package rtsp serves the fleet's synthetic streams.
//
// One gortsplib server holds N ServerStreams and dispatches by request path, so
// a fleet costs one listener and one accept loop regardless of camera count. The
// media behind those streams is parsed once and shared; per-camera state is only
// an encoder and a position in the loop.
package rtsp

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"

	"github.com/0x524a/camfarm/internal/clock"
	"github.com/0x524a/camfarm/internal/fault"
	"github.com/0x524a/camfarm/internal/media"
	"github.com/0x524a/camfarm/internal/obs"
	"github.com/0x524a/camfarm/internal/seed"
)

// ErrNotStarted is returned when the server has not been started yet.
var ErrNotStarted = errors.New("rtsp: server not started")

// CameraConfig describes one camera.
type CameraConfig struct {
	ID     string
	Media  *media.Media
	Seed   seed.Seed
	Faults []fault.Spec
}

// Config configures the server.
type Config struct {
	// Host to bind. Defaults to 127.0.0.1: a test fixture should not be
	// reachable from the network by accident.
	Host string
	// Port to bind. Zero requests an ephemeral port, readable back via Addr.
	Port int
	// Log receives server events. Nil means silent. Never stdout: a consumer
	// runs camfarm inside a process whose stdout is a protocol stream.
	Log *slog.Logger
	// Clock paces the pumps. Nil means the real clock.
	Clock clock.Clock
	// Obs records what happened. Required.
	Obs *obs.Recorder

	Cameras []CameraConfig
}

type camera struct {
	id     string
	cfg    CameraConfig
	stream *gortsplib.ServerStream
	medi   *description.Media
	forma  *format.H264
}

// Server serves a fleet of synthetic RTSP cameras.
type Server struct {
	cfg Config
	log *slog.Logger
	ck  clock.Clock

	mu      sync.RWMutex
	srv     *gortsplib.Server
	cams    map[string]*camera
	order   []string
	readers map[*gortsplib.ServerSession]string
}

// New validates cfg and returns an unstarted server.
func New(cfg Config) (*Server, error) {
	if cfg.Obs == nil {
		return nil, errors.New("rtsp: Config.Obs is required")
	}
	if len(cfg.Cameras) == 0 {
		return nil, errors.New("rtsp: no cameras configured")
	}
	if cfg.Host == "" {
		cfg.Host = "127.0.0.1"
	}

	s := &Server{
		cfg:     cfg,
		log:     cfg.Log,
		ck:      cfg.Clock,
		cams:    make(map[string]*camera, len(cfg.Cameras)),
		readers: make(map[*gortsplib.ServerSession]string),
	}
	if s.log == nil {
		s.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if s.ck == nil {
		s.ck = clock.Real{}
	}

	for _, cc := range cfg.Cameras {
		if cc.ID == "" {
			return nil, errors.New("rtsp: camera with an empty ID")
		}
		if strings.ContainsAny(cc.ID, "/?#") {
			return nil, fmt.Errorf("rtsp: camera ID %q contains a character reserved in a URL path", cc.ID)
		}
		if cc.Media == nil {
			return nil, fmt.Errorf("rtsp: camera %q has no media", cc.ID)
		}
		if len(cc.Media.SPS) == 0 || len(cc.Media.PPS) == 0 {
			return nil, fmt.Errorf("rtsp: camera %q media carries no SPS/PPS", cc.ID)
		}
		if _, dup := s.cams[cc.ID]; dup {
			return nil, fmt.Errorf("rtsp: duplicate camera ID %q", cc.ID)
		}
		s.cams[cc.ID] = &camera{id: cc.ID, cfg: cc}
		s.order = append(s.order, cc.ID)
	}
	return s, nil
}

// Start binds the listener and initializes every stream.
func (s *Server) Start() error {
	s.mu.Lock()

	if s.srv != nil {
		s.mu.Unlock()
		return errors.New("rtsp: already started")
	}

	srv := &gortsplib.Server{
		Handler:     s,
		RTSPAddress: net.JoinHostPort(s.cfg.Host, strconv.Itoa(s.cfg.Port)),
	}
	if err := srv.Start(); err != nil {
		s.mu.Unlock()
		return fmt.Errorf("rtsp: starting server: %w", err)
	}

	// ServerStream.Initialize needs a started server, and the handler must
	// already be live because Start launched the accept loop. Holding the write
	// lock for the whole sequence is what makes that safe: read handlers block
	// until every stream exists.
	var failedID string
	var initErr error
	for _, id := range s.order {
		cam := s.cams[id]
		cam.forma = &format.H264{
			PayloadTyp:        96,
			SPS:               cam.cfg.Media.SPS,
			PPS:               cam.cfg.Media.PPS,
			PacketizationMode: 1,
		}
		cam.medi = &description.Media{
			Type:    description.MediaTypeVideo,
			Formats: []format.Format{cam.forma},
		}
		cam.stream = &gortsplib.ServerStream{
			Server: srv,
			Desc:   &description.Session{Medias: []*description.Media{cam.medi}},
		}
		if err := cam.stream.Initialize(); err != nil {
			failedID, initErr = id, err
			cam.stream = nil
			break
		}
	}

	if initErr != nil {
		// Same rule as Close: release the lock before calling into gortsplib.
		// The accept loop has been live since srv.Start() above, so a client
		// could already hold a session; tearing it down here while still
		// holding the lock would deadlock against its OnSessionClose the same
		// way Close() would.
		streams := make([]*gortsplib.ServerStream, 0, len(s.order))
		for _, id := range s.order {
			if c := s.cams[id]; c.stream != nil {
				streams = append(streams, c.stream)
				c.stream = nil
			}
		}
		s.mu.Unlock()

		for _, stream := range streams {
			stream.Close()
		}
		srv.Close()
		return fmt.Errorf("rtsp: initializing stream for %q: %w", failedID, initErr)
	}

	s.srv = srv
	addr := srv.NetListener().Addr().String()
	n := len(s.order)
	s.mu.Unlock()

	s.log.Info("rtsp server started", "addr", addr, "cameras", n)
	return nil
}

// Close stops the server and every stream. Safe to call more than once.
//
// The lock is released before calling into gortsplib: Server.Close tears down
// live sessions, and each teardown invokes OnSessionClose on its own goroutine,
// which needs the same lock to drop its reader registration. Holding the lock
// across that call would deadlock Close against every session it is trying to
// close.
func (s *Server) Close() {
	s.mu.Lock()
	if s.srv == nil {
		s.mu.Unlock()
		return
	}
	srv := s.srv
	s.srv = nil
	streams := make([]*gortsplib.ServerStream, 0, len(s.order))
	for _, id := range s.order {
		if cam := s.cams[id]; cam.stream != nil {
			streams = append(streams, cam.stream)
			cam.stream = nil
		}
	}
	s.mu.Unlock()

	for _, stream := range streams {
		stream.Close()
	}
	srv.Close()
}

// Addr returns the bound address, or nil before a successful Start.
func (s *Server) Addr() *net.TCPAddr {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.srv == nil {
		return nil
	}
	// NetListener panics before Start succeeds, which is why this is guarded.
	addr, ok := s.srv.NetListener().Addr().(*net.TCPAddr)
	if !ok {
		return nil
	}
	return addr
}

// Has reports whether a camera exists.
func (s *Server) Has(id string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.cams[id]
	return ok
}

// Path returns the request path a camera answers on.
func (s *Server) Path(id string) string { return "/" + id }

// URL returns the full RTSP URL of a camera, or "" before Start.
func (s *Server) URL(id string) string {
	addr := s.Addr()
	if addr == nil {
		return ""
	}
	return "rtsp://" + addr.String() + s.Path(id)
}

// Readers returns the number of sessions currently set up against a camera.
//
// gortsplib exposes no reader count of its own, so this is tracked here.
func (s *Server) Readers(id string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, camID := range s.readers {
		if camID == id {
			n++
		}
	}
	return n
}

// StreamStats returns outbound RTP packet and byte counts for a camera.
func (s *Server) StreamStats(id string) (packets, bytes uint64) {
	p, b, _ := s.streamStatsErr(id)
	return p, b
}

// streamStatsErr is StreamStats with the error surfaced, for tests.
func (s *Server) streamStatsErr(id string) (packets, bytes uint64, err error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.srv == nil {
		return 0, 0, ErrNotStarted
	}
	cam, ok := s.cams[id]
	if !ok {
		return 0, 0, ErrNotStarted
	}
	st := cam.stream.Stats()
	return st.OutboundRTPPackets, st.OutboundBytes, nil
}

// lookup resolves a request path to a camera.
func (s *Server) lookup(path string) *camera {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cams[strings.Trim(path, "/")]
}

// --- gortsplib handlers ---
//
// gortsplib's ServerHandler is `any`; a server implements whichever of the
// optional interfaces it needs.

// OnConnClose logs connection teardown.
func (s *Server) OnConnClose(ctx *gortsplib.ServerHandlerOnConnCloseCtx) {
	s.log.Debug("connection closed", "err", ctx.Error)
}

// OnSessionClose drops the session's reader registration.
func (s *Server) OnSessionClose(ctx *gortsplib.ServerHandlerOnSessionCloseCtx) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.readers, ctx.Session)
}

// OnDescribe answers DESCRIBE by path.
func (s *Server) OnDescribe(ctx *gortsplib.ServerHandlerOnDescribeCtx) (*base.Response, *gortsplib.ServerStream, error) {
	cam := s.lookup(ctx.Path)
	if cam == nil {
		return &base.Response{StatusCode: base.StatusNotFound}, nil, nil
	}
	return &base.Response{StatusCode: base.StatusOK}, cam.stream, nil
}

// OnSetup answers SETUP by path and registers the session as a reader.
func (s *Server) OnSetup(ctx *gortsplib.ServerHandlerOnSetupCtx) (*base.Response, *gortsplib.ServerStream, error) {
	cam := s.lookup(ctx.Path)
	if cam == nil {
		return &base.Response{StatusCode: base.StatusNotFound}, nil, nil
	}
	s.mu.Lock()
	s.readers[ctx.Session] = cam.id
	s.mu.Unlock()
	return &base.Response{StatusCode: base.StatusOK}, cam.stream, nil
}

// OnPlay answers PLAY.
func (s *Server) OnPlay(ctx *gortsplib.ServerHandlerOnPlayCtx) (*base.Response, error) {
	if cam := s.lookup(ctx.Path); cam == nil {
		return &base.Response{StatusCode: base.StatusNotFound}, nil
	}
	return &base.Response{StatusCode: base.StatusOK}, nil
}

// OnResponse is the header-level fault seam.
//
// gortsplib sets Content-Base, Content-Type, CSeq, RTP-Info and the SDP body
// after a handler returns, so a handler cannot lie about them. This hook
// receives the same live response immediately before it is marshalled, which is
// the only place those headers can be rewritten. No fault rewrites anything in
// this version; the hook exists so the phase-2 faults have somewhere to live.
func (s *Server) OnResponse(_ *gortsplib.ServerConn, _ *base.Response) {}
