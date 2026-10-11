package dashboard

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/0x524a/camfarm/internal/library"
)

// multipartSlack is how much framing a multipart body may add beyond the file.
const multipartSlack = 1 << 20

// uploadIdleTimeout is how long an upload may go without delivering a byte
// before it is cut off. It is a variable so tests can shorten it.
var uploadIdleTimeout = 30 * time.Second

// idleReader pushes the connection's read deadline out before every read, so a
// slow upload that keeps making progress survives the server's ReadTimeout while
// a stalled one is cut off after uploadIdleTimeout.
type idleReader struct {
	io.ReadCloser
	rc *http.ResponseController
}

func (r idleReader) Read(p []byte) (int, error) {
	_ = r.rc.SetReadDeadline(time.Now().Add(uploadIdleTimeout))
	return r.ReadCloser.Read(p)
}

type fileDTO struct {
	library.Entry
	InUseBy []string `json:"inUseBy"`
}

type libraryDTO struct {
	Roots         []library.RootInfo `json:"roots"`
	UploadEnabled bool               `json:"uploadEnabled"`
	AllowUIPaths  bool               `json:"allowUIPaths"`
	MaxUpload     int64              `json:"maxUpload"`
	Scanning      bool               `json:"scanning"`
	Files         []fileDTO          `json:"files"`
}

// needLibrary replies 404 and returns false when the library is not enabled.
func (s *server) needLibrary(w http.ResponseWriter) bool {
	if s.lib == nil {
		writeError(w, http.StatusNotFound, "the media library is not enabled")
		return false
	}
	return true
}

// inUse maps a library file path to the cameras serving it.
func (s *server) inUse() map[string][]string {
	out := map[string][]string{}
	for _, st := range s.fleet.List() {
		if p, ok := strings.CutPrefix(st.Source, "file:"); ok {
			p = canonicalPath(p)
			out[p] = append(out[p], st.ID)
		}
	}
	return out
}

// canonicalPath makes a camera's typed path comparable with a library entry's
// path, which is absolute and symlink-resolved.
func canonicalPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}

func (s *server) fileView(e library.Entry, inUse map[string][]string) fileDTO {
	users := inUse[e.Path]
	if users == nil {
		users = []string{}
	}
	return fileDTO{Entry: e, InUseBy: users}
}

func (s *server) libraryView() libraryDTO {
	inUse := s.inUse()
	entries := s.lib.List()
	files := make([]fileDTO, 0, len(entries))
	for _, e := range entries {
		files = append(files, s.fileView(e, inUse))
	}
	roots := s.lib.Roots()
	if roots == nil {
		roots = []library.RootInfo{}
	}
	return libraryDTO{
		Roots: roots, UploadEnabled: s.lib.UploadEnabled(), AllowUIPaths: s.lib.AllowUIPaths(),
		MaxUpload: s.lib.MaxUpload(), Scanning: s.lib.Scanning(), Files: files,
	}
}

// writeLibraryErr maps a library error to an HTTP status.
func writeLibraryErr(w http.ResponseWriter, err error) {
	var tooBig *http.MaxBytesError
	var netErr net.Error
	switch {
	case errors.As(err, &tooBig), errors.Is(err, library.ErrTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, err.Error())
	case errors.Is(err, os.ErrDeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		writeError(w, http.StatusRequestTimeout, "the upload stalled")
	case errors.Is(err, io.ErrUnexpectedEOF):
		writeError(w, http.StatusBadRequest, "the upload body ended early")
	case errors.Is(err, library.ErrBadName), errors.Is(err, library.ErrNotDirectory):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, library.ErrReadOnly), errors.Is(err, library.ErrUIPathsDisabled), errors.Is(err, library.ErrNotDeletable):
		writeError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, library.ErrNotFound):
		writeError(w, http.StatusNotFound, "unknown library file")
	default:
		writeErr(w, err)
	}
}

func (s *server) getLibrary(w http.ResponseWriter, _ *http.Request) {
	if !s.needLibrary(w) {
		return
	}
	writeJSON(w, http.StatusOK, s.libraryView())
}

func (s *server) scanLibrary(w http.ResponseWriter, r *http.Request) {
	if !s.needLibrary(w) {
		return
	}
	var req struct {
		Force bool   `json:"force"`
		Root  string `json:"root"`
	}
	if r.ContentLength != 0 && !decodeJSON(w, r, &req) {
		return
	}
	if err := s.lib.Scan(r.Context(), req.Root, req.Force); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			writeErr(w, err)
			return
		}
		if errors.Is(err, library.ErrNotFound) {
			writeError(w, http.StatusNotFound, "unknown media directory")
			return
		}
		writeLibraryErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.libraryView())
}

func (s *server) uploadFile(w http.ResponseWriter, r *http.Request) {
	if !s.needLibrary(w) {
		return
	}
	// The binary's ReadTimeout bounds slow request bodies; a large upload over a
	// slow link must not be cut off by it. The idle reader replaces it with a
	// per-read deadline, and the size cap bounds the total bytes.
	r.Body = idleReader{http.MaxBytesReader(w, r.Body, s.lib.MaxUpload()+multipartSlack), http.NewResponseController(w)}

	mr, err := r.MultipartReader()
	if err != nil {
		writeError(w, http.StatusBadRequest, "expected multipart/form-data with a file part: "+err.Error())
		return
	}
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, `the upload has no "file" part`)
			return
		}
		if err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				writeError(w, http.StatusRequestEntityTooLarge, "upload too large")
				return
			}
			writeError(w, http.StatusBadRequest, "malformed upload: "+err.Error())
			return
		}
		if part.FormName() != "file" || part.FileName() == "" {
			continue
		}
		e, err := s.lib.Upload(r.Context(), part.FileName(), part)
		if err != nil {
			writeLibraryErr(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, s.fileView(e, s.inUse()))
		return
	}
}

func (s *server) deleteFile(w http.ResponseWriter, r *http.Request) {
	if !s.needLibrary(w) {
		return
	}
	id := r.PathValue("id")
	e, ok := s.lib.Resolve(id)
	if !ok {
		writeError(w, http.StatusNotFound, "unknown library file")
		return
	}
	if users := s.inUse()[e.Path]; len(users) > 0 {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":   "a camera is serving this file; remove the camera first",
			"cameras": users,
		})
		return
	}
	if err := s.lib.Delete(id); err != nil {
		writeLibraryErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) addRoot(w http.ResponseWriter, r *http.Request) {
	if !s.needLibrary(w) {
		return
	}
	var req struct {
		Path string `json:"path"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if !s.lib.AllowUIPaths() {
		writeLibraryErr(w, library.ErrUIPathsDisabled)
		return
	}
	if !filepath.IsAbs(req.Path) {
		writeError(w, http.StatusBadRequest, "path must be an absolute directory path")
		return
	}
	if err := s.lib.AddRoot(req.Path); err != nil {
		switch {
		case errors.Is(err, library.ErrUIPathsDisabled):
			writeLibraryErr(w, err)
		case errors.Is(err, fs.ErrNotExist), errors.Is(err, fs.ErrPermission):
			writeError(w, http.StatusBadRequest, "that path does not exist or is not readable")
		case errors.Is(err, library.ErrNotDirectory):
			writeError(w, http.StatusBadRequest, "that path is not a directory")
		default:
			writeError(w, http.StatusBadRequest, "that path cannot be used as a media directory")
		}
		return
	}
	if err := s.lib.Scan(r.Context(), "", false); err != nil {
		writeLibraryErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.libraryView())
}
