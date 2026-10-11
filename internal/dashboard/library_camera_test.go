package dashboard

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/0x524a/camfarm/internal/library"
	"github.com/0x524a/camfarm/internal/media"
	"github.com/0x524a/camfarm/internal/media/fixture"
)

// stage writes a file into the library root and scans, returning its id.
func stage(t *testing.T, ts string, root, name string, data []byte) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), data, 0o644); err != nil {
		t.Fatal(err)
	}
	v := decodeLibrary(t, do(t, "POST", ts+"/api/v1/library/scan", ""))
	for _, f := range v.Files {
		if f.Name == name {
			return f.ID
		}
	}
	t.Fatalf("%s not in the library: %+v", name, v.Files)
	return ""
}

func addFromLibrary(t *testing.T, ts, camID, libID string) *http.Response {
	t.Helper()
	return do(t, "POST", ts+"/api/v1/cameras", `{"id":"`+camID+`","libraryId":"`+libID+`"}`)
}

func TestCameraFromLibraryFileServesIt(t *testing.T) {
	ts, _, root := libServer(t, library.Options{})
	id := stage(t, ts.URL, root, "clip.ts", fixture.Bytes())

	resp := addFromLibrary(t, ts.URL, "from-lib", id)
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("add = %d %s", resp.StatusCode, b)
	}
	var cam cameraDTO
	_ = json.NewDecoder(resp.Body).Decode(&cam)
	if cam.LibraryID != id || cam.Source != "file:"+filepath.Join(root, "clip.ts") || len(cam.Warnings) != 0 {
		t.Errorf("camera = %+v", cam)
	}

	// The proof the file is really served: a snapshot is captured over RTSP.
	snap := do(t, "GET", ts.URL+"/api/v1/cameras/from-lib/snapshot.jpg", "")
	if snap.StatusCode != 200 || snap.Header.Get("Content-Type") != "image/jpeg" {
		t.Errorf("snapshot = %d %s", snap.StatusCode, snap.Header.Get("Content-Type"))
	}

	v := decodeLibrary(t, do(t, "GET", ts.URL+"/api/v1/library", ""))
	if len(v.Files[0].InUseBy) != 1 || v.Files[0].InUseBy[0] != "from-lib" {
		t.Errorf("inUseBy = %v", v.Files[0].InUseBy)
	}
	del := do(t, "DELETE", ts.URL+"/api/v1/library/files/"+id, "")
	if del.StatusCode != 409 {
		t.Errorf("deleting a file in use = %d, want 409", del.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(root, "clip.ts")); err != nil {
		t.Errorf("a file in use was removed: %v", err)
	}
}

func TestCameraFromWarningFileCarriesItsWarnings(t *testing.T) {
	ts, _, root := libServer(t, library.Options{})
	out := filepath.Join(root, "b.mp4")
	args := []string{"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size=320x240:rate=25:duration=2",
		"-c:v", "libx264", "-bf", "2", "-g", "25", "-pix_fmt", "yuv420p", out}
	if b, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v\n%s", err, b)
	}
	v := decodeLibrary(t, do(t, "POST", ts.URL+"/api/v1/library/scan", ""))
	if len(v.Files) != 1 || v.Files[0].Report.Status != media.StatusWarning {
		t.Fatalf("files = %+v", v.Files)
	}
	resp := addFromLibrary(t, ts.URL, "bframes", v.Files[0].ID)
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("a warning file must be usable: %d %s", resp.StatusCode, b)
	}
	var cam cameraDTO
	_ = json.NewDecoder(resp.Body).Decode(&cam)
	codes := map[string]bool{}
	for _, w := range cam.Warnings {
		codes[w.Code] = true
	}
	if !codes["b_frames"] {
		t.Errorf("warnings = %+v, want b_frames", cam.Warnings)
	}
}

func TestInvalidLibraryFileCannotBecomeACamera(t *testing.T) {
	ts, _, root := libServer(t, library.Options{})
	id := stage(t, ts.URL, root, "junk.mp4", []byte("this is just text"))
	resp := addFromLibrary(t, ts.URL, "nope", id)
	if resp.StatusCode != 422 {
		t.Fatalf("add = %d, want 422", resp.StatusCode)
	}
	var body struct {
		Error  string       `json:"error"`
		Report media.Report `json:"report"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.Error == "" || body.Report.Status != media.StatusInvalid || len(body.Report.Issues) == 0 {
		t.Errorf("body = %+v", body)
	}
	if r := do(t, "GET", ts.URL+"/api/v1/cameras/nope", ""); r.StatusCode != 404 {
		t.Errorf("a refused camera exists anyway: %d", r.StatusCode)
	}
}

// A file can change between the scan and the click. The report the user saw may
// be stale, so the server re-checks before it builds the camera.
func TestStaleLibraryFileIsRecheckedAtCreation(t *testing.T) {
	ts, _, root := libServer(t, library.Options{})
	id := stage(t, ts.URL, root, "clip.ts", fixture.Bytes())
	if err := os.WriteFile(filepath.Join(root, "clip.ts"), []byte("replaced by text after the scan"), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := addFromLibrary(t, ts.URL, "stale", id); r.StatusCode != 422 {
		t.Errorf("edited-after-scan file = %d, want 422", r.StatusCode)
	}

	id2 := stage(t, ts.URL, root, "other.ts", fixture.Bytes())
	if err := os.Remove(filepath.Join(root, "other.ts")); err != nil {
		t.Fatal(err)
	}
	if r := addFromLibrary(t, ts.URL, "gone", id2); r.StatusCode != 404 {
		t.Errorf("deleted-after-scan file = %d, want 404", r.StatusCode)
	}
}

func TestLibraryIDRequestShapes(t *testing.T) {
	ts, _, root := libServer(t, library.Options{})
	id := stage(t, ts.URL, root, "clip.ts", fixture.Bytes())
	cases := []struct {
		name, body string
		want       int
	}{
		{"unknown id", `{"id":"a","libraryId":"deadbeef"}`, 404},
		{"with a typed path", `{"id":"b","libraryId":"` + id + `","source":{"kind":"file","path":"/x.ts"}}`, 400},
		{"with the fixture kind", `{"id":"c","libraryId":"` + id + `","source":{"kind":"fixture"}}`, 400},
		{"with an explicit file kind", `{"id":"d","libraryId":"` + id + `","source":{"kind":"file"}}`, 201},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if r := do(t, "POST", ts.URL+"/api/v1/cameras", c.body); r.StatusCode != c.want {
				b, _ := io.ReadAll(r.Body)
				t.Errorf("status = %d, want %d: %s", r.StatusCode, c.want, b)
			}
		})
	}
}

func TestLibraryIDWithoutALibrary(t *testing.T) {
	ts, _ := newServer(t, Options{})
	if r := do(t, "POST", ts.URL+"/api/v1/cameras", `{"id":"a","libraryId":"x"}`); r.StatusCode != 404 {
		t.Errorf("status = %d, want 404", r.StatusCode)
	}
}

func TestTypedPathStillWorksAndIsLinkedToTheLibrary(t *testing.T) {
	ts, _, root := libServer(t, library.Options{})
	id := stage(t, ts.URL, root, "clip.ts", fixture.Bytes())
	p := filepath.Join(root, "clip.ts")
	resp := do(t, "POST", ts.URL+"/api/v1/cameras", `{"id":"typed","source":{"kind":"file","path":"`+p+`"}}`)
	if resp.StatusCode != 201 {
		t.Fatalf("typed path = %d", resp.StatusCode)
	}
	var cam cameraDTO
	_ = json.NewDecoder(resp.Body).Decode(&cam)
	if cam.LibraryID != id {
		t.Errorf("libraryId = %q, want %q: a camera on a library path should link to its entry", cam.LibraryID, id)
	}
}

// A typed path that is not canonical (a ./ segment, a symlinked directory) must
// still link to its library entry.
func TestTypedNonCanonicalPathStillLinks(t *testing.T) {
	ts, _, root := libServer(t, library.Options{})
	id := stage(t, ts.URL, root, "clip.ts", fixture.Bytes())
	link := filepath.Join(t.TempDir(), "via")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]string{
		"dot":     root + "/./clip.ts",
		"symlink": filepath.Join(link, "clip.ts"),
	} {
		t.Run(name, func(t *testing.T) {
			resp := do(t, "POST", ts.URL+"/api/v1/cameras", `{"id":"nc-`+name+`","source":{"kind":"file","path":"`+p+`"}}`)
			if resp.StatusCode != 201 {
				b, _ := io.ReadAll(resp.Body)
				t.Fatalf("add = %d %s", resp.StatusCode, b)
			}
			var cam cameraDTO
			_ = json.NewDecoder(resp.Body).Decode(&cam)
			if cam.LibraryID != id {
				t.Errorf("libraryId = %q, want %q for typed path %s", cam.LibraryID, id, p)
			}
		})
	}
}

// Creating a camera from a library file races deleting that file. Whatever
// order the requests land in, no camera may be left serving a deleted file.
func TestCameraCreationRacingFileDeletion(t *testing.T) {
	ts, _, root := libServer(t, library.Options{})
	const rounds, adders = 25, 4
	for round := 0; round < rounds; round++ {
		name := fmt.Sprintf("race%d.ts", round)
		var f fileDTO
		_ = json.NewDecoder(postFile(t, ts.URL, name, fixture.Bytes()).Body).Decode(&f)
		if f.ID == "" {
			t.Fatalf("upload %s failed", name)
		}

		var wg sync.WaitGroup
		start := make(chan struct{})
		check := func(r *http.Response, ok ...int) {
			defer r.Body.Close()
			if r.StatusCode >= 500 {
				b, _ := io.ReadAll(r.Body)
				t.Errorf("5xx: %d %s", r.StatusCode, b)
				return
			}
			for _, c := range ok {
				if r.StatusCode == c {
					return
				}
			}
			t.Errorf("unexpected status %d", r.StatusCode)
		}
		for i := 0; i < adders; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				check(addFromLibrary(t, ts.URL, fmt.Sprintf("r%d-c%d", round, i), f.ID), 201, 404)
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			check(do(t, "DELETE", ts.URL+"/api/v1/library/files/"+f.ID, ""), 204, 409, 404)
		}()
		close(start)
		wg.Wait()

		// Invariant: every camera that exists has its file on disk.
		var cams []cameraDTO
		_ = json.NewDecoder(do(t, "GET", ts.URL+"/api/v1/cameras", "").Body).Decode(&cams)
		for _, c := range cams {
			if p, ok := strings.CutPrefix(c.Source, "file:"); ok {
				if _, err := os.Stat(p); err != nil {
					t.Fatalf("round %d: camera %s serves a deleted file: %v", round, c.ID, err)
				}
			}
			do(t, "DELETE", ts.URL+"/api/v1/cameras/"+c.ID, "").Body.Close()
		}
		_ = os.Remove(filepath.Join(root, name))
	}
}
