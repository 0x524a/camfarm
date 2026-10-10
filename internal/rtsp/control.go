package rtsp

import (
	"fmt"
	"net"
	"slices"
	"sort"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"

	"github.com/0x524a/camfarm/internal/fault"
	"github.com/0x524a/camfarm/internal/media"
)

// Journal clocks reported through SetFaults' applied callback.
const (
	clockFrame   = "frame"
	clockRequest = "request"
)

// splitFaults partitions specs into per-frame (pump) and per-request (control)
// kinds, preserving order within each.
func splitFaults(specs []fault.Spec) (media, control []fault.Spec) {
	for _, sp := range specs {
		if fault.IsControl(sp.Kind) {
			control = append(control, sp)
		} else {
			media = append(media, sp)
		}
	}
	return media, control
}

func mediaFaults(specs []fault.Spec) []fault.Spec {
	m, _ := splitFaults(specs)
	return m
}

// newControl builds a camera's control engine from its initial fault set. It
// shares the pump's seed derivation; the two engines never collide because
// each stream is further split by kind name.
func newControl(cc CameraConfig) (*fault.ControlEngine, error) {
	ctl := fault.NewControlEngine(cc.Seed.Stream("fault"))
	_, control := splitFaults(cc.Faults)
	if err := ctl.Set(control); err != nil {
		return nil, err
	}
	return ctl, nil
}

// sameSpecs reports whether two spec lists install the same faults, ignoring
// order (Camera.Inject reorders unrelated kinds).
func sameSpecs(a, b []fault.Spec) bool {
	if len(a) != len(b) {
		return false
	}
	sorted := func(in []fault.Spec) []fault.Spec {
		out := append([]fault.Spec(nil), in...)
		sort.Slice(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
		return out
	}
	return slices.Equal(sorted(a), sorted(b))
}

// SetFaults replaces a camera's fault set on a running server. Per-frame kinds
// go to the pump and take effect at its next frame boundary; per-request kinds
// go to the camera's control engine and take effect on the next request.
//
// Only a class whose spec list actually changed is touched: an unchanged class
// is neither swapped nor notified, so injecting one fault does not restart the
// draw sequence of an unrelated active one (ControlEngine.Set and a pump swap
// both restart a class's streams).
//
// applied, if non-nil, is called once per changed class: with ("request",
// requests decided so far) synchronously, before SetFaults returns, because a
// control change is effective immediately; and with ("frame", frame index) from
// the pump goroutine at the frame boundary. When both change, request is
// therefore reported first, which is the order the effects land in.
func (s *Server) SetFaults(id string, specs []fault.Spec, applied func(index int, clock string)) error {
	s.mu.RLock()
	started := s.srv != nil
	cam, ok := s.cams[id]
	s.mu.RUnlock()
	if !started {
		return ErrNotStarted
	}
	if !ok || cam.pump == nil {
		return fmt.Errorf("rtsp: unknown camera %q", id)
	}

	media, control := splitFaults(specs)
	// Validate the control half on a scratch engine first, so a bad control spec
	// cannot leave the pump swapped and the engine not.
	if err := fault.NewControlEngine(0).Set(control); err != nil {
		return err
	}

	cam.setMu.Lock()
	defer cam.setMu.Unlock()
	mediaChanged := !sameSpecs(media, cam.media)
	controlChanged := !sameSpecs(control, cam.control)

	if mediaChanged {
		var pumpApplied func(int)
		if applied != nil {
			pumpApplied = func(frame int) { applied(frame, clockFrame) }
		}
		if err := cam.pump.SetFaults(media, pumpApplied); err != nil {
			return err
		}
		cam.media = media
	}
	if controlChanged {
		if err := cam.ctl.Set(control); err != nil {
			return err // unreachable: validated above
		}
		cam.control = control
		if applied != nil {
			applied(cam.ctl.Requests(), clockRequest)
		}
	}
	return nil
}

// answerError turns a fired rtsp_error into the response to send, recording it.
// It returns nil when no error fired. Callers must not hold s.mu.
func (s *Server) answerError(m fault.Method, camID string, d fault.ControlDecision) *base.Response {
	if d.Error == nil {
		return nil
	}
	s.cfg.Obs.Fault(camID, d.Request, string(fault.KindRTSPError),
		fmt.Sprintf("answered %d to %s", d.Error.Status, m))
	s.cfg.Obs.FaultTally(camID, string(fault.KindRTSPError), 1, 0)
	code := base.StatusCode(d.Error.Status)
	res := &base.Response{StatusCode: code}
	// The catalogue allows any 4xx or 5xx; gortsplib only names the ones it
	// knows, and an empty reason phrase is not something a real camera sends.
	if _, known := base.StatusMessages[code]; !known {
		if d.Error.Status >= 500 {
			res.StatusMessage = "Server Error"
		} else {
			res.StatusMessage = "Client Error"
		}
	}
	return res
}

// armed is a session a teardown fault will end once remaining more frames have
// been stepped for its camera.
type armed struct {
	camID     string
	sess      *gortsplib.ServerSession
	conn      *gortsplib.ServerConn
	remaining int
	mode      string
}

// arm registers a freshly playing session for teardown.
func (s *Server) arm(camID string, sess *gortsplib.ServerSession, conn *gortsplib.ServerConn, a *fault.Arming) {
	s.armMu.Lock()
	s.armed[sess] = &armed{camID: camID, sess: sess, conn: conn, remaining: a.AfterFrames, mode: a.Mode}
	s.armMu.Unlock()
}

// disarm forgets a session; called when the session closes.
func (s *Server) disarm(sess *gortsplib.ServerSession) {
	s.armMu.Lock()
	delete(s.armed, sess)
	s.armMu.Unlock()
}

// afterFrame is the pump hook: it counts one stepped frame against every armed
// session of the camera and ends those that reach zero. Counting is by pump
// step, never wall clock, so a seed and a frame count fix the outcome.
func (s *Server) afterFrame(camID string) {
	var due []*armed
	s.armMu.Lock()
	for sess, a := range s.armed {
		if a.camID != camID {
			continue
		}
		a.remaining--
		if a.remaining <= 0 {
			due = append(due, a)
			delete(s.armed, sess)
		}
	}
	s.armMu.Unlock()

	// armMu is released before closing: closing triggers OnSessionClose, which
	// takes it.
	for _, a := range due {
		s.cfg.Obs.Fault(camID, 0, string(fault.KindTeardown), "session torn down ("+a.mode+")")
		s.cfg.Obs.FaultTally(camID, string(fault.KindTeardown), 1, 0)
		s.tearDown(a)
	}
}

// tearDown ends a session. Every call is safe on a session or connection that
// is already gone.
func (s *Server) tearDown(a *armed) {
	nc := a.conn.NetConn()
	if a.mode == "reset" {
		if tc, ok := nc.(*net.TCPConn); ok {
			_ = tc.SetLinger(0) // close sends RST rather than FIN
		}
		_ = nc.Close()
		return
	}
	a.sess.Close()
}

// lieInfo is an sdp_lie decided for a connection's DESCRIBE.
type lieInfo struct {
	mode  string
	camID string
	codec media.Codec
}

// noteLie records the lie for the response about to be written on conn, and
// tallies it. Requests on one connection are answered in order, so the next
// response OnResponse sees for conn is this DESCRIBE's.
func (s *Server) noteLie(conn *gortsplib.ServerConn, camID string, d fault.ControlDecision) {
	s.mu.RLock()
	cam, ok := s.cams[camID]
	s.mu.RUnlock()
	if !ok {
		return
	}
	s.lieMu.Lock()
	s.pendingLie[conn] = lieInfo{mode: d.Lie.Mode, camID: camID, codec: cam.cfg.Media.Codec}
	s.lieMu.Unlock()
	s.cfg.Obs.Fault(camID, d.Request, string(fault.KindSDPLie), "DESCRIBE answered with a lying "+d.Lie.Mode)
	s.cfg.Obs.FaultTally(camID, string(fault.KindSDPLie), 1, 0)
}

// dropLie forgets a connection's pending lie.
func (s *Server) dropLie(conn *gortsplib.ServerConn) {
	s.lieMu.Lock()
	delete(s.pendingLie, conn)
	s.lieMu.Unlock()
}

// applyLie takes and clears conn's pending lie, applying it only to a 200
// response: a DESCRIBE that went on to fail must not leave a lie behind for a
// later response.
func (s *Server) applyLie(conn *gortsplib.ServerConn, res *base.Response) {
	s.lieMu.Lock()
	li, ok := s.pendingLie[conn]
	delete(s.pendingLie, conn)
	s.lieMu.Unlock()
	if !ok || res.StatusCode != base.StatusOK {
		return
	}
	if li.mode == "contentBase" {
		if cb := res.Header["Content-Base"]; len(cb) > 0 {
			res.Header["Content-Base"] = base.HeaderValue{lieContentBase(cb[0])}
		}
		return
	}
	out, err := rewriteSDP(res.Body, li.mode, li.codec)
	if err != nil {
		// The honest SDP goes out: a lie that cannot be built is not a half-lie.
		s.log.Warn("sdp_lie not applied", "camera", li.camID, "err", err)
		return
	}
	res.Body = out
}
