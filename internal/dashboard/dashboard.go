// Package dashboard serves an HTTP API and an embedded web page that drive a
// camfarm fleet. It is a thin layer over the public camfarm API.
package dashboard

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"strconv"
	"time"

	"github.com/0x524a/camfarm"
	"github.com/0x524a/camfarm/internal/fault"
	"github.com/0x524a/camfarm/internal/library"
)

// Options configures the dashboard.
type Options struct {
	// SnapshotTimeout bounds one capture; zero means defaultSnapshotTimeout (10s).
	SnapshotTimeout time.Duration
	// MaxConcurrentSnapshots caps captures in flight across cameras; zero means 4.
	MaxConcurrentSnapshots int
	// capture is the capture seam; nil means Camera.Screenshot. Tests only.
	capture func(context.Context, *camfarm.Camera) (image.Image, error)
	// Library, when non-nil, enables the media library endpoints.
	Library *library.Library
}

//go:embed web
var webFS embed.FS

const maxBody = 1 << 20

type server struct {
	fleet *camfarm.Fleet
	snap  *snapshotter
	lib   *library.Library
}

// Handler returns the dashboard for a fleet.
func Handler(f *camfarm.Fleet, opt Options) http.Handler {
	s := &server{fleet: f, snap: newSnapshotter(opt), lib: opt.Library}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/fleet", s.getFleet)
	mux.HandleFunc("GET /api/v1/cameras", s.listCameras)
	mux.HandleFunc("POST /api/v1/cameras", s.addCamera)
	mux.HandleFunc("GET /api/v1/cameras/{id}", s.getCamera)
	mux.HandleFunc("DELETE /api/v1/cameras/{id}", s.removeCamera)
	mux.HandleFunc("GET /api/v1/cameras/{id}/snapshot.jpg", s.snapshot)
	mux.HandleFunc("POST /api/v1/cameras/{id}/faults", s.injectFault)
	mux.HandleFunc("DELETE /api/v1/cameras/{id}/faults", s.clearFaults)
	mux.HandleFunc("GET /api/v1/faults", s.getFaultCatalogue)
	mux.HandleFunc("GET /api/v1/events", s.events)
	mux.HandleFunc("GET /api/v1/library", s.getLibrary)
	mux.HandleFunc("POST /api/v1/library/scan", s.scanLibrary)
	mux.HandleFunc("POST /api/v1/library/upload", s.uploadFile)
	mux.HandleFunc("DELETE /api/v1/library/files/{id}", s.deleteFile)
	mux.HandleFunc("POST /api/v1/library/roots", s.addRoot)

	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		panic(err) // the embed directive guarantees the directory exists
	}
	mux.Handle("GET /", http.FileServerFS(sub))
	return mux
}

type cameraDTO struct {
	ID            string              `json:"id"`
	RTSPURL       string              `json:"rtspUrl"`
	ONVIFEndpoint string              `json:"onvifEndpoint"`
	ONVIFUUID     string              `json:"onvifUuid"`
	Source        string              `json:"source"`
	Codec         string              `json:"codec"`
	Width         int                 `json:"width"`
	Height        int                 `json:"height"`
	FPS           float64             `json:"fps"`
	Readers       int                 `json:"readers"`
	Seed          string              `json:"seed"` // decimal string: a uint64 exceeds JS 2^53
	Username      string              `json:"username,omitempty"`
	Password      string              `json:"password,omitempty"`
	Faults        []camfarm.FaultSpec `json:"faults"`
	Stats         statsDTO            `json:"stats"`
}

// statsDTO mirrors camfarm.Stats with the seed as a decimal string, since a
// uint64 seed above 2^53 is silently rounded by JavaScript's JSON.parse. The
// other counters stay far below that bound.
type statsDTO struct {
	Seed               string
	FramesServed       uint64
	LoopsCompleted     uint64
	FaultsFired        uint64
	FaultCounts        map[string]camfarm.FaultCount
	OutboundRTPPackets uint64
	OutboundBytes      uint64
	Readers            int
}

func (s *server) dto(cam *camfarm.Camera, st camfarm.Status) cameraDTO {
	u, p, _ := cam.Auth()
	faults := cam.Faults()
	stats := cam.Stats()
	if faults == nil {
		faults = []camfarm.FaultSpec{}
	}
	return cameraDTO{
		ID: st.ID, RTSPURL: st.RTSPURL, ONVIFEndpoint: cam.ONVIFEndpoint(), ONVIFUUID: cam.ONVIFUUID(),
		Source: st.Source, Codec: st.Codec, Width: st.Width, Height: st.Height, FPS: st.FPS,
		Readers: st.Readers, Seed: strconv.FormatUint(stats.Seed, 10), Username: u, Password: p,
		Faults: faults,
		Stats: statsDTO{
			Seed: strconv.FormatUint(stats.Seed, 10), FramesServed: stats.FramesServed,
			LoopsCompleted: stats.LoopsCompleted, FaultsFired: stats.FaultsFired, FaultCounts: stats.FaultCounts,
			OutboundRTPPackets: stats.OutboundRTPPackets, OutboundBytes: stats.OutboundBytes,
			Readers: stats.Readers,
		},
	}
}

// getFaultCatalogue serves the fault catalogue that drives validation and the
// page's fault form, so the page cannot drift from the server.
func (s *server) getFaultCatalogue(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, fault.Catalogue())
}

func (s *server) listCameras(w http.ResponseWriter, _ *http.Request) {
	statuses := s.fleet.List()
	out := make([]cameraDTO, 0, len(statuses))
	for _, st := range statuses {
		cam, err := s.fleet.Camera(st.ID)
		if err != nil {
			continue // removed between List and Camera
		}
		out = append(out, s.dto(cam, st))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) statusOf(id string) (*camfarm.Camera, camfarm.Status, error) {
	cam, err := s.fleet.Camera(id)
	if err != nil {
		return nil, camfarm.Status{}, err
	}
	for _, st := range s.fleet.List() {
		if st.ID == id {
			return cam, st, nil
		}
	}
	return nil, camfarm.Status{}, fmt.Errorf("%w: %q", camfarm.ErrUnknownCamera, id)
}

func (s *server) getCamera(w http.ResponseWriter, r *http.Request) {
	cam, st, err := s.statusOf(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.dto(cam, st))
}

func (s *server) addCamera(w http.ResponseWriter, r *http.Request) {
	var spec camfarm.CameraSpec
	if !decodeJSON(w, r, &spec) {
		return
	}
	cam, err := s.fleet.AddCamera(spec)
	if err != nil {
		writeErr(w, err)
		return
	}
	_, st, err := s.statusOf(cam.ID())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, s.dto(cam, st))
}

func (s *server) removeCamera(w http.ResponseWriter, r *http.Request) {
	if err := s.fleet.RemoveCamera(r.PathValue("id")); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) snapshot(w http.ResponseWriter, r *http.Request) {
	cam, err := s.fleet.Camera(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	b, err := s.snap.get(r.Context(), cam)
	if err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(b)
}

func (s *server) injectFault(w http.ResponseWriter, r *http.Request) {
	cam, err := s.fleet.Camera(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	var f camfarm.FaultSpec
	if !decodeJSON(w, r, &f) {
		return
	}
	if err := cam.Inject(f); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, cam.Faults())
}

func (s *server) clearFaults(w http.ResponseWriter, r *http.Request) {
	cam, err := s.fleet.Camera(r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := cam.Clear(); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// decodeJSON reads a JSON body into v, replying with the right status itself
// and returning false on failure.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return bodyFailed(w, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("unexpected data after the JSON value")
		}
		return bodyFailed(w, err)
	}
	return true
}

// bodyFailed replies to a body that could not be read as one JSON value.
func bodyFailed(w http.ResponseWriter, err error) bool {
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return false
	}
	writeError(w, http.StatusBadRequest, "malformed JSON body: "+err.Error())
	return false
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// writeErr maps a library error to an HTTP status.
func writeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, camfarm.ErrDuplicateCamera):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, camfarm.ErrUnknownCamera):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, camfarm.ErrUnsupported), errors.Is(err, camfarm.ErrInvalidSpec):
		writeError(w, http.StatusUnprocessableEntity, err.Error())
	case errors.Is(err, camfarm.ErrNotReady):
		writeError(w, http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusGatewayTimeout, "snapshot timed out")
	case errors.Is(err, context.Canceled):
		writeError(w, 499, "request cancelled")
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}
