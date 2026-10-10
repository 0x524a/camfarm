package rtsp

import (
	"fmt"

	"github.com/bluenviron/gortsplib/v5/pkg/base"

	"github.com/0x524a/camfarm/internal/fault"
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

// SetFaults replaces a camera's fault set on a running server. Per-frame kinds
// go to the pump and take effect at its next frame boundary; per-request kinds
// go to the camera's control engine and take effect on the next request.
//
// applied, if non-nil, is called once per class present, in the order frame
// then request: with the frame index for "frame" and the number of requests
// decided so far for "request". A clear reports every class it removed (or
// "frame" if there was nothing to remove). When a set has both classes the
// request notification follows the frame one, from the pump goroutine; a set
// with control kinds only is reported synchronously.
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
	wantFrame, wantRequest := len(media) > 0, len(control) > 0
	if len(specs) == 0 {
		wantFrame, wantRequest = cam.hadMedia, cam.hadControl
		if !wantFrame && !wantRequest {
			wantFrame = true
		}
	}

	var pumpApplied func(int)
	if applied != nil && wantFrame {
		pumpApplied = func(frame int) {
			applied(frame, clockFrame)
			if wantRequest {
				applied(cam.ctl.Requests(), clockRequest)
			}
		}
	}
	if err := cam.pump.SetFaults(media, pumpApplied); err != nil {
		return err
	}
	if err := cam.ctl.Set(control); err != nil {
		return err // unreachable: validated above
	}
	cam.hadMedia, cam.hadControl = len(media) > 0, len(control) > 0
	if applied != nil && wantRequest && !wantFrame {
		applied(cam.ctl.Requests(), clockRequest)
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
	return &base.Response{StatusCode: base.StatusCode(d.Error.Status)}
}
