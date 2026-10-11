package dashboard

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0x524a/camfarm"
	"github.com/0x524a/camfarm/internal/library"
	"github.com/0x524a/camfarm/internal/media/fixture"
)

// libServer serves a dashboard over a library whose first root is a fresh
// temp directory.
func libServer(t *testing.T, opt library.Options) (*httptest.Server, *library.Library, string) {
	t.Helper()
	opt.Roots = append([]string{t.TempDir()}, opt.Roots...)
	lib, err := library.New(opt)
	if err != nil {
		t.Fatal(err)
	}
	ts, _ := newServer(t, Options{Library: lib})
	return ts, lib, lib.Roots()[0].Path
}

func multipartBody(t *testing.T, filename string, data []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fw.Write(data)
	_ = mw.Close()
	return &buf, mw.FormDataContentType()
}

func postFile(t *testing.T, base, filename string, data []byte) *http.Response {
	t.Helper()
	body, ct := multipartBody(t, filename, data)
	resp, err := http.Post(base+"/api/v1/library/upload", ct, body)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func decodeLibrary(t *testing.T, resp *http.Response) libraryDTO {
	t.Helper()
	var v libraryDTO
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return v
}

func TestLibraryRoutesAre404WhenDisabled(t *testing.T) {
	ts, _ := newServer(t, Options{})
	for _, c := range [][2]string{
		{"GET", "/api/v1/library"}, {"POST", "/api/v1/library/scan"},
		{"POST", "/api/v1/library/roots"}, {"DELETE", "/api/v1/library/files/x"},
		{"POST", "/api/v1/library/upload"},
	} {
		resp := do(t, c[0], ts.URL+c[1], "")
		if resp.StatusCode != 404 {
			t.Errorf("%s %s = %d, want 404", c[0], c[1], resp.StatusCode)
		}
	}
}

func TestGetLibraryDescribesRootsAndFiles(t *testing.T) {
	ts, _, _ := libServer(t, library.Options{AllowUIPaths: true, MaxUpload: 1234})
	v := decodeLibrary(t, do(t, "GET", ts.URL+"/api/v1/library", ""))
	if len(v.Roots) != 1 || !v.UploadEnabled || !v.AllowUIPaths || v.MaxUpload != 1234 || v.Files == nil || len(v.Files) != 0 {
		t.Errorf("library = %+v", v)
	}
}

func TestScanListsFilesWithReports(t *testing.T) {
	ts, _, root := libServer(t, library.Options{})
	_ = os.WriteFile(filepath.Join(root, "good.ts"), fixture.Bytes(), 0o644)
	_ = os.WriteFile(filepath.Join(root, "bad.mp4"), []byte("plain text, not video"), 0o644)

	resp := do(t, "POST", ts.URL+"/api/v1/library/scan", "")
	if resp.StatusCode != 200 {
		t.Fatalf("scan = %d", resp.StatusCode)
	}
	v := decodeLibrary(t, resp)
	if len(v.Files) != 2 || v.Files[0].Name != "bad.mp4" || v.Files[0].Report.Status != "invalid" ||
		v.Files[1].Name != "good.ts" || v.Files[1].Report.Status != "valid" {
		t.Fatalf("files = %+v", v.Files)
	}
	if v.Files[0].InUseBy == nil {
		t.Error("inUseBy must encode as [], not null")
	}

	if r := do(t, "POST", ts.URL+"/api/v1/library/scan", `{"force":true}`); r.StatusCode != 200 {
		t.Errorf("forced scan = %d", r.StatusCode)
	}
	if r := do(t, "POST", ts.URL+"/api/v1/library/scan", `{"root":"/not/a/root"}`); r.StatusCode != 404 {
		t.Errorf("unknown root = %d, want 404", r.StatusCode)
	}
	if r := do(t, "POST", ts.URL+"/api/v1/library/scan", `{"nope":1}`); r.StatusCode != 400 {
		t.Errorf("unknown field = %d, want 400", r.StatusCode)
	}
}

func TestUploadEndpoint(t *testing.T) {
	ts, _, root := libServer(t, library.Options{MaxUpload: 100000})

	resp := postFile(t, ts.URL, "clip.ts", fixture.Bytes())
	if resp.StatusCode != 201 {
		t.Fatalf("upload = %d", resp.StatusCode)
	}
	var f fileDTO
	_ = json.NewDecoder(resp.Body).Decode(&f)
	if f.Name != "uploads/clip.ts" || f.Report.Status != "valid" {
		t.Errorf("file = %+v", f)
	}

	resp = postFile(t, ts.URL, "junk.mp4", []byte("not a video"))
	_ = json.NewDecoder(resp.Body).Decode(&f)
	if resp.StatusCode != 201 || f.Report.Status != "invalid" {
		t.Errorf("invalid upload: %d %+v (an invalid file is kept, with its report)", resp.StatusCode, f.Report)
	}

	if r := postFile(t, ts.URL, "../x.txt", fixture.Bytes()); r.StatusCode != 400 {
		t.Errorf("bad name = %d, want 400", r.StatusCode)
	}

	before, _ := os.ReadDir(root)
	if r := postFile(t, ts.URL, "big.ts", make([]byte, 200000)); r.StatusCode != 413 {
		t.Errorf("over the limit = %d, want 413", r.StatusCode)
	}
	after, _ := os.ReadDir(root)
	if len(after) != len(before) {
		t.Errorf("an over-limit upload left files behind: %d -> %d", len(before), len(after))
	}

	// Not multipart, and multipart without a file part.
	if r := do(t, "POST", ts.URL+"/api/v1/library/upload", `{}`); r.StatusCode != 400 {
		t.Errorf("json body = %d, want 400", r.StatusCode)
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("other", "x")
	_ = mw.Close()
	r, err := http.Post(ts.URL+"/api/v1/library/upload", mw.FormDataContentType(), &buf)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != 400 {
		t.Errorf("no file part = %d, want 400", r.StatusCode)
	}
}

func TestUploadForbiddenWithoutAWritableRoot(t *testing.T) {
	lib, err := library.New(library.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ts, _ := newServer(t, Options{Library: lib})
	if r := postFile(t, ts.URL, "a.ts", fixture.Bytes()); r.StatusCode != 403 {
		t.Errorf("upload with no writable root = %d, want 403", r.StatusCode)
	}
}

// The binary sets ReadTimeout to bound slow request bodies; a large upload over
// a slow link must not be cut off by it.
func TestSlowUploadSurvivesTheServerReadTimeout(t *testing.T) {
	lib, err := library.New(library.Options{Roots: []string{t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	f := camfarm.StartT(t, camfarm.Spec{Seed: 1, Cameras: []camfarm.CameraSpec{{ID: "c"}}})
	srv := httptest.NewUnstartedServer(Handler(f, Options{Library: lib}))
	srv.Config.ReadTimeout = 300 * time.Millisecond
	srv.Start()
	t.Cleanup(srv.Close)

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		fw, _ := mw.CreateFormFile("file", "slow.ts")
		data := fixture.Bytes()
		_, _ = fw.Write(data[:len(data)/2])
		time.Sleep(900 * time.Millisecond) // three times the ReadTimeout
		_, _ = fw.Write(data[len(data)/2:])
		_ = mw.Close()
		_ = pw.Close()
	}()
	req, _ := http.NewRequest("POST", srv.URL+"/api/v1/library/upload", pr)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("slow upload: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("slow upload = %d %s", resp.StatusCode, b)
	}
}

func TestDeleteEndpoint(t *testing.T) {
	ts, lib, _ := libServer(t, library.Options{})
	var f fileDTO
	_ = json.NewDecoder(postFile(t, ts.URL, "gone.ts", fixture.Bytes()).Body).Decode(&f)

	if r := do(t, "DELETE", ts.URL+"/api/v1/library/files/"+f.ID, ""); r.StatusCode != 204 {
		t.Fatalf("delete = %d", r.StatusCode)
	}
	if _, ok := lib.Resolve(f.ID); ok {
		t.Error("entry survived the delete")
	}
	if r := do(t, "DELETE", ts.URL+"/api/v1/library/files/"+f.ID, ""); r.StatusCode != 404 {
		t.Errorf("second delete = %d, want 404", r.StatusCode)
	}
}

// TestDeleteRefusesAScannedOriginalInTheWritableRoot: the writable root holds
// the operator's own files; only uploads/ is deletable.
func TestDeleteRefusesAScannedOriginalInTheWritableRoot(t *testing.T) {
	ts, _, root := libServer(t, library.Options{})
	id := stage(t, ts.URL, root, "original.ts", fixture.Bytes())
	if r := do(t, "DELETE", ts.URL+"/api/v1/library/files/"+id, ""); r.StatusCode != 403 {
		t.Errorf("delete of a scanned original = %d, want 403", r.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(root, "original.ts")); err != nil {
		t.Errorf("the original was removed: %v", err)
	}
}

func TestDeleteRefusesFilesOutsideTheUploadRoot(t *testing.T) {
	other := t.TempDir()
	_ = os.WriteFile(filepath.Join(other, "keep.ts"), fixture.Bytes(), 0o644)
	ts, lib, _ := libServer(t, library.Options{Roots: []string{other}})
	do(t, "POST", ts.URL+"/api/v1/library/scan", "")
	var id string
	for _, e := range lib.List() {
		id = e.ID
	}
	if r := do(t, "DELETE", ts.URL+"/api/v1/library/files/"+id, ""); r.StatusCode != 403 {
		t.Errorf("delete outside the upload root = %d, want 403", r.StatusCode)
	}
}

func TestAddRootEndpoint(t *testing.T) {
	extra := t.TempDir()
	_ = os.WriteFile(filepath.Join(extra, "e.ts"), fixture.Bytes(), 0o644)

	off, _, _ := libServer(t, library.Options{})
	if r := do(t, "POST", off.URL+"/api/v1/library/roots", `{"path":"`+extra+`"}`); r.StatusCode != 403 {
		t.Errorf("without --allow-ui-paths = %d, want 403", r.StatusCode)
	}

	on, _, _ := libServer(t, library.Options{AllowUIPaths: true})
	resp := do(t, "POST", on.URL+"/api/v1/library/roots", `{"path":"`+extra+`"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("add root = %d", resp.StatusCode)
	}
	v := decodeLibrary(t, resp)
	if len(v.Roots) != 2 || !v.Roots[1].UI || len(v.Files) != 1 || v.Files[0].Name != "e.ts" {
		t.Errorf("library = %+v", v)
	}
	if r := do(t, "POST", on.URL+"/api/v1/library/roots", `{"path":"`+filepath.Join(extra, "nope")+`"}`); r.StatusCode != 400 {
		t.Errorf("nonexistent directory = %d, want 400", r.StatusCode)
	}
	if r := do(t, "POST", on.URL+"/api/v1/library/roots", `{"path":"`+filepath.Join(extra, "e.ts")+`"}`); r.StatusCode != 400 {
		t.Errorf("a file = %d, want 400", r.StatusCode)
	}
}

func TestAddRootRejectsEmptyAndRelativePaths(t *testing.T) {
	ts, _, _ := libServer(t, library.Options{AllowUIPaths: true})
	for _, body := range []string{`{}`, `{"path":""}`, `{"path":"relative/dir"}`, `{"path":"."}`} {
		if r := do(t, "POST", ts.URL+"/api/v1/library/roots", body); r.StatusCode != 400 {
			t.Errorf("%s = %d, want 400", body, r.StatusCode)
		}
	}
	if v := decodeLibrary(t, do(t, "GET", ts.URL+"/api/v1/library", "")); len(v.Roots) != 1 {
		t.Errorf("roots after rejected adds = %+v, want the original one", v.Roots)
	}
}

func TestEmptyLibraryEncodesRootsAsAnArray(t *testing.T) {
	lib, err := library.New(library.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ts, _ := newServer(t, Options{Library: lib})
	b, _ := io.ReadAll(do(t, "GET", ts.URL+"/api/v1/library", "").Body)
	if !strings.Contains(string(b), `"roots":[]`) || !strings.Contains(string(b), `"files":[]`) {
		t.Errorf("body = %s", b)
	}
}

func TestLibraryErrorsDoNotEchoTypedPaths(t *testing.T) {
	ts, _, _ := libServer(t, library.Options{AllowUIPaths: true})
	missing := filepath.Join(t.TempDir(), "typed-missing-dir")
	file := filepath.Join(t.TempDir(), "typed-file.ts")
	_ = os.WriteFile(file, fixture.Bytes(), 0o644)
	for _, c := range []struct {
		url, body string
		want      int
	}{
		{"/api/v1/library/roots", `{"path":"` + missing + `"}`, 400},
		{"/api/v1/library/roots", `{"path":"` + file + `"}`, 400},
		{"/api/v1/library/scan", `{"root":"` + missing + `"}`, 404},
	} {
		resp := do(t, "POST", ts.URL+c.url, c.body)
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != c.want || strings.Contains(string(b), "typed-") {
			t.Errorf("%s %s = %d %s", c.url, c.body, resp.StatusCode, b)
		}
	}
}

// rawUpload sends an upload request whose body is cut after sent bytes,
// declaring the full length, and returns the response (or nil if none arrived
// before the deadline).
func rawUpload(t *testing.T, addr string, cut func(body []byte) int, then func(*net.TCPConn)) *http.Response {
	t.Helper()
	body, ct := multipartBody(t, "cut.ts", fixture.Bytes())
	conn, err := net.Dial("tcp", strings.TrimPrefix(addr, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	tc := conn.(*net.TCPConn)
	_ = tc.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(tc, "POST /api/v1/library/upload HTTP/1.1\r\nHost: x\r\nContent-Type: %s\r\nContent-Length: %d\r\n\r\n", ct, body.Len())
	_, _ = tc.Write(body.Bytes()[:cut(body.Bytes())])
	then(tc)
	resp, err := http.ReadResponse(bufio.NewReader(tc), nil)
	if err != nil {
		t.Fatalf("no response: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func noPartFiles(t *testing.T, root string) {
	t.Helper()
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".upload-") {
			t.Errorf("leftover part file %s", e.Name())
		}
	}
}

func TestTruncatedUploadBodyIs400(t *testing.T) {
	ts, _, root := libServer(t, library.Options{})
	resp := rawUpload(t, ts.URL, func(b []byte) int { return len(b) / 2 },
		func(tc *net.TCPConn) { _ = tc.CloseWrite() })
	if resp.StatusCode != 400 {
		t.Errorf("truncated body = %d, want 400", resp.StatusCode)
	}
	noPartFiles(t, root)
}

func TestStalledUploadIsCutOff(t *testing.T) {
	old := uploadIdleTimeout
	uploadIdleTimeout = 300 * time.Millisecond
	t.Cleanup(func() { uploadIdleTimeout = old })

	root := t.TempDir()
	lib, err := library.New(library.Options{Roots: []string{root}})
	if err != nil {
		t.Fatal(err)
	}
	ts, _ := newServer(t, Options{Library: lib})
	resp := rawUpload(t, ts.URL, func(b []byte) int { return len(b) / 2 }, func(*net.TCPConn) {})
	if resp.StatusCode != 408 {
		t.Errorf("stalled upload = %d, want 408", resp.StatusCode)
	}
	noPartFiles(t, root)
}

func TestInUseMatchesCameraPathsThatAreNotCanonical(t *testing.T) {
	ts, _, root := libServer(t, library.Options{})
	var f fileDTO
	_ = json.NewDecoder(postFile(t, ts.URL, "clip.ts", fixture.Bytes()).Body).Decode(&f)
	_ = os.Mkdir(filepath.Join(root, "sub"), 0o755)
	link := filepath.Join(t.TempDir(), "rootlink")
	if err := os.Symlink(root, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	for i, typed := range []string{
		root + "/./sub/../uploads/clip.ts",
		filepath.Join(link, "uploads", "clip.ts"),
	} {
		id := fmt.Sprintf("cam%d", i)
		body := `{"id":"` + id + `","source":{"kind":"file","path":"` + typed + `"}}`
		if r := do(t, "POST", ts.URL+"/api/v1/cameras", body); r.StatusCode != 201 {
			t.Fatalf("add camera via %s = %d", typed, r.StatusCode)
		}
	}
	v := decodeLibrary(t, do(t, "GET", ts.URL+"/api/v1/library", ""))
	if len(v.Files) != 1 || len(v.Files[0].InUseBy) != 2 {
		t.Fatalf("inUseBy = %+v, want both cameras", v.Files)
	}
	if r := do(t, "DELETE", ts.URL+"/api/v1/library/files/"+f.ID, ""); r.StatusCode != 409 {
		t.Errorf("delete of an in-use file = %d, want 409", r.StatusCode)
	}
}

func TestLibraryErrorsNeverEchoUnrelatedPaths(t *testing.T) {
	ts, _, _ := libServer(t, library.Options{AllowUIPaths: true})
	resp := do(t, "POST", ts.URL+"/api/v1/library/roots", `{"path":"/etc/shadow"}`)
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 400 || strings.Contains(string(b), "root:") {
		t.Errorf("status %d body %s", resp.StatusCode, b)
	}
}
