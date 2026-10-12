package dashboard

import (
	"bufio"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/0x524a/camfarm/internal/quality"
)

// qualitySummary is the small form of a camera's latest stream quality run
// that rides on the camera DTO, so a card can show a badge without a second
// request.
type qualitySummary struct {
	State      quality.State `json:"state"`
	Verdict    string        `json:"verdict,omitempty"`
	FinishedAt *time.Time    `json:"finishedAt,omitempty"`
}

// needQuality replies 404 and returns false when stream quality tests are not
// enabled.
func (s *server) needQuality(w http.ResponseWriter) bool {
	if s.quality == nil {
		writeError(w, http.StatusNotFound, "stream quality tests are not enabled")
		return false
	}
	return true
}

func (s *server) qualityOf(id string) *qualitySummary {
	if s.quality == nil {
		return nil
	}
	run, ok := s.quality.Get(id)
	if !ok {
		return nil
	}
	return &qualitySummary{State: run.State, Verdict: run.Verdict, FinishedAt: run.FinishedAt}
}

// startQuality begins a test of the camera's own RTSP URL. The URL is never
// read from the request, so this endpoint cannot be aimed at another host.
func (s *server) startQuality(w http.ResponseWriter, r *http.Request) {
	if !s.needQuality(w) {
		return
	}
	id := r.PathValue("id")
	cam, err := s.fleet.Camera(id)
	if err != nil {
		writeErr(w, err)
		return
	}
	var req quality.Request
	if !decodeOptionalJSON(w, r, &req) {
		return
	}
	run, err := s.quality.Start(id, cam.RTSPURL(), req)
	if err != nil {
		switch {
		case errors.Is(err, quality.ErrBadRequest):
			writeError(w, http.StatusUnprocessableEntity, err.Error())
		case errors.Is(err, quality.ErrRunning):
			writeError(w, http.StatusConflict, err.Error())
		case errors.Is(err, quality.ErrBusy):
			w.Header().Set("Retry-After", "5")
			writeError(w, http.StatusTooManyRequests, err.Error())
		case errors.Is(err, quality.ErrClosed):
			writeError(w, http.StatusServiceUnavailable, err.Error())
		default:
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusAccepted, run)
}

func (s *server) getQuality(w http.ResponseWriter, r *http.Request) {
	if !s.needQuality(w) {
		return
	}
	id := r.PathValue("id")
	if _, err := s.fleet.Camera(id); err != nil {
		writeErr(w, err)
		return
	}
	run, ok := s.quality.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "no stream quality result for this camera")
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *server) cancelQuality(w http.ResponseWriter, r *http.Request) {
	if !s.needQuality(w) {
		return
	}
	id := r.PathValue("id")
	if _, err := s.fleet.Camera(id); err != nil {
		writeErr(w, err)
		return
	}
	if !s.quality.Cancel(id) {
		writeError(w, http.StatusNotFound, "no stream quality test is running on this camera")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// decodeOptionalJSON is decodeJSON for a body that may be absent: an empty
// body leaves v at its zero value and needs no Content-Type.
func decodeOptionalJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	br := bufio.NewReader(r.Body)
	if _, err := br.Peek(1); errors.Is(err, io.EOF) {
		return true
	}
	r.Body = struct {
		io.Reader
		io.Closer
	}{br, r.Body}
	return decodeJSON(w, r, v)
}
