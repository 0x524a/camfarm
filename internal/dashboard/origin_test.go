package dashboard

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/0x524a/camfarm"
	"github.com/0x524a/camfarm/internal/library"
)

const refusedText = "cross-origin request refused"

// mutating lists the four library endpoints that change state.
func mutating(id string) []struct{ name, method, path, ctype, body string } {
	return []struct{ name, method, path, ctype, body string }{
		{"scan", "POST", "/api/v1/library/scan", "application/json", `{}`},
		{"roots", "POST", "/api/v1/library/roots", "application/json", `{"path":"/tmp"}`},
		{"delete", "DELETE", "/api/v1/library/files/" + id, "", ""},
		{"upload", "POST", "/api/v1/library/upload", "multipart/form-data; boundary=x",
			"--x\r\nContent-Disposition: form-data; name=\"file\"; filename=\"a.ts\"\r\n\r\nzz\r\n--x--\r\n"},
	}
}

func originReq(t *testing.T, base string, ep struct{ name, method, path, ctype, body string }, hdr map[string]string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(ep.method, base+ep.path, strings.NewReader(ep.body))
	if err != nil {
		t.Fatal(err)
	}
	if ep.ctype != "" {
		req.Header.Set("Content-Type", ep.ctype)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestMutatingLibraryEndpointsRefuseCrossOriginRequests(t *testing.T) {
	ts, _, root := libServer(t, library.Options{})
	id := stage(t, ts.URL, root, "clip.ts", []byte("not media"))

	refused := map[string]map[string]string{
		"cross-site":          {"Sec-Fetch-Site": "cross-site"},
		"same-site foreign":   {"Sec-Fetch-Site": "same-site", "Origin": "https://sibling.example"},
		"same-site no origin": {"Sec-Fetch-Site": "same-site"},
		"foreign origin":      {"Origin": "https://evil.example"},
		"foreign origin port": {"Origin": strings.Replace(ts.URL, "127.0.0.1:", "127.0.0.1:1", 1)},
		"null origin":         {"Origin": "null"},
		"cross-site no-cors":  {"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"},
	}
	for _, ep := range mutating(id) {
		for hname, hdr := range refused {
			t.Run(ep.name+"/"+hname, func(t *testing.T) {
				code, body := originReq(t, ts.URL, ep, hdr)
				if code != http.StatusForbidden || !strings.Contains(body, refusedText) {
					t.Errorf("got %d %s, want 403 %q", code, body, refusedText)
				}
			})
		}
	}
	if _, err := os.Stat(filepath.Join(root, "clip.ts")); err != nil {
		t.Errorf("a refused delete removed the file: %v", err)
	}
}

func TestMutatingLibraryEndpointsAllowSameOriginAndHeaderlessRequests(t *testing.T) {
	ts, _, root := libServer(t, library.Options{})
	id := stage(t, ts.URL, root, "clip.ts", []byte("not media"))

	allowed := map[string]map[string]string{
		"no headers":       nil,
		"same origin":      {"Origin": ts.URL, "Sec-Fetch-Site": "same-origin"},
		"matching origin":  {"Origin": ts.URL},
		"same-site match":  {"Origin": ts.URL, "Sec-Fetch-Site": "same-site"},
		"none (navigated)": {"Sec-Fetch-Site": "none"},
	}
	for _, ep := range mutating(id) {
		for hname, hdr := range allowed {
			t.Run(ep.name+"/"+hname, func(t *testing.T) {
				_, body := originReq(t, ts.URL, ep, hdr)
				if strings.Contains(body, refusedText) {
					t.Errorf("guard refused an allowed request: %s", body)
				}
			})
		}
	}
}

// countingBody records whether the handler read any of it.
type countingBody struct {
	r    io.Reader
	read atomic.Int64
}

func (c *countingBody) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.read.Add(int64(n))
	return n, err
}

func TestCrossOriginUploadIsRefusedBeforeTheBodyIsRead(t *testing.T) {
	lib, err := library.New(library.Options{Roots: []string{t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	f := camfarm.StartT(t, camfarm.Spec{Seed: 1, Cameras: []camfarm.CameraSpec{{ID: "c"}}})
	h := Handler(f, Options{Library: lib})
	root := lib.Roots()[0].Path

	cb := &countingBody{r: strings.NewReader(mutating("x")[3].body)}
	req := httptest.NewRequest("POST", "/api/v1/library/upload", cb)
	req.Header.Set("Content-Type", "multipart/form-data; boundary=x")
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("code = %d, want 403", rec.Code)
	}
	if n := cb.read.Load(); n != 0 {
		t.Errorf("handler read %d body bytes before refusing", n)
	}
	var leftovers []string
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, _ error) error {
		if !d.IsDir() {
			leftovers = append(leftovers, p)
		}
		return nil
	})
	if len(leftovers) != 0 {
		t.Errorf("refused upload left files: %v", leftovers)
	}
}
