package dashboard

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/0x524a/camfarm"
	"github.com/0x524a/camfarm/internal/library"
	"github.com/0x524a/camfarm/internal/media"
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
	// ScanErrors names media directories the last scan could not read, in fixed
	// wording. Empty, never null.
	ScanErrors []string `json:"scanErrors"`
}

// sameOrigin refuses a request a browser sent on behalf of another origin.
// multipart/form-data is a CORS-safelisted content type, so a foreign page can
// POST an upload without a preflight; the JSON endpoints get a preflight from
// their content type, but every mutating library endpoint is guarded the same
// way. It runs before the handler reads any body. A request with neither
// Sec-Fetch-Site nor Origin (curl, a Go client) is not from a browser page and
// passes.
func sameOrigin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		originOK := true
		if o := r.Header.Get("Origin"); o != "" {
			u, err := url.Parse(o)
			originOK = err == nil && u.Host != "" && u.Host == r.Host
		}
		switch r.Header.Get("Sec-Fetch-Site") {
		case "cross-site":
			originOK = false
		case "same-site":
			// Another origin on the same registrable domain: only acceptable if
			// the Origin header proves it is this one.
			originOK = originOK && r.Header.Get("Origin") != ""
		}
		if !originOK {
			writeError(w, http.StatusForbidden, "cross-origin request refused")
			return
		}
		next(w, r)
	}
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

// scanFailure turns an error from Library.Scan into scanErrors text. ok is
// false when err holds anything besides unreadable media directories, which the
// caller maps to an error status as before.
func scanFailure(err error) (texts []string, ok bool) {
	roots, other := library.SplitScanError(err)
	if other != nil {
		return nil, false
	}
	for _, re := range roots {
		texts = append(texts, "media directory unavailable: "+re.Root)
	}
	return texts, true
}

func (s *server) libraryView(scanErrors ...string) libraryDTO {
	if scanErrors == nil {
		scanErrors = []string{}
	}
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
		ScanErrors: scanErrors,
	}
}

// writeLibraryErr maps a library error to an HTTP status.
func writeLibraryErr(w http.ResponseWriter, err error) {
	var tooBig *http.MaxBytesError
	var netErr net.Error
	switch {
	case errors.As(err, &tooBig), errors.Is(err, library.ErrTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, err.Error())
	case errors.Is(err, library.ErrBusy):
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusTooManyRequests, err.Error())
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
	err := s.lib.Scan(r.Context(), req.Root, req.Force)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			writeErr(w, err)
			return
		}
		if errors.Is(err, library.ErrNotFound) {
			writeError(w, http.StatusNotFound, "unknown media directory")
			return
		}
	}
	s.writeScanResult(w, err)
}

// writeScanResult answers a request that ran a scan: the library view, with any
// unreadable media directories listed in scanErrors, or an error status when
// the scan failed for another reason.
func (s *server) writeScanResult(w http.ResponseWriter, err error) {
	var texts []string
	if err != nil {
		var ok bool
		if texts, ok = scanFailure(err); !ok {
			writeLibraryErr(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, s.libraryView(texts...))
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
	// libMu makes the in-use check and the delete atomic with respect to
	// addCamera's re-check and add.
	s.libMu.Lock()
	defer s.libMu.Unlock()
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

// addCameraReq is the body of POST /cameras: a CameraSpec plus an optional
// libraryId. libraryId is a sibling of source, not part of it, because
// SourceSpec is public library API and the library is a dashboard concern.
type addCameraReq struct {
	camfarm.CameraSpec
	LibraryID string `json:"libraryId,omitempty"`
}

// specFromLibrary resolves req.LibraryID to a file source. It replies and
// returns false when the file is unusable. The file is re-checked first, so a
// report that went stale between the scan and the click is never trusted. The
// caller holds libMu.
func (s *server) specFromLibrary(w http.ResponseWriter, r *http.Request, req addCameraReq) (camfarm.CameraSpec, bool) {
	spec := req.CameraSpec
	if !s.needLibrary(w) {
		return spec, false
	}
	if spec.Source.Path != "" || (spec.Source.Kind != "" && spec.Source.Kind != camfarm.SourceFile) {
		writeError(w, http.StatusBadRequest, "libraryId cannot be combined with source.path or a source kind other than file")
		return spec, false
	}
	e, err := s.lib.Refresh(r.Context(), req.LibraryID)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			writeError(w, http.StatusGatewayTimeout, "checking the file timed out")
			return spec, false
		}
		if errors.Is(err, context.Canceled) {
			writeError(w, 499, "request cancelled")
			return spec, false
		}
		writeLibraryErr(w, err)
		return spec, false
	}
	if e.Report.Status == media.StatusInvalid {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":  "this file is invalid and cannot be used as a camera",
			"report": e.Report,
		})
		return spec, false
	}
	spec.Source = camfarm.SourceSpec{Kind: camfarm.SourceFile, Path: e.Path}
	return spec, true
}

// libraryInfo links a camera's source to its library entry, if it has one, and
// returns that entry's warnings so the camera card can show them.
func (s *server) libraryInfo(source string) (string, []media.Issue) {
	if s.lib == nil {
		return "", nil
	}
	p, ok := strings.CutPrefix(source, "file:")
	if !ok {
		return "", nil
	}
	e, ok := s.lib.ByPath(canonicalPath(p))
	if !ok {
		return "", nil
	}
	var warns []media.Issue
	for _, is := range e.Report.Issues {
		if is.Severity == media.SeverityWarning {
			warns = append(warns, is)
		}
	}
	return e.ID, warns
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
	s.writeScanResult(w, s.lib.Scan(r.Context(), "", false))
}
