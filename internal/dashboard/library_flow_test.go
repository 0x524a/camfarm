package dashboard

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/0x524a/camfarm/internal/library"
	"github.com/0x524a/camfarm/internal/media/fixture"
)

// TestLibraryUploadToCameraToDeleteFlow is the spec's end-to-end path in one
// run: upload, list, camera from the file, snapshot over RTSP, delete refused
// while in use, delete after the camera is gone.
func TestLibraryUploadToCameraToDeleteFlow(t *testing.T) {
	ts, _, _ := libServer(t, library.Options{})

	up := postFile(t, ts.URL, "flow.ts", fixture.Bytes())
	if up.StatusCode != 201 {
		t.Fatalf("upload = %d", up.StatusCode)
	}
	var f fileDTO
	if err := json.NewDecoder(up.Body).Decode(&f); err != nil {
		t.Fatal(err)
	}

	v := decodeLibrary(t, do(t, "GET", ts.URL+"/api/v1/library", ""))
	if len(v.Files) != 1 || v.Files[0].ID != f.ID || v.Files[0].Report.Status != "valid" {
		t.Fatalf("library = %+v", v.Files)
	}

	if r := addFromLibrary(t, ts.URL, "flow-cam", f.ID); r.StatusCode != 201 {
		t.Fatalf("add camera = %d", r.StatusCode)
	}
	snap := do(t, "GET", ts.URL+"/api/v1/cameras/flow-cam/snapshot.jpg", "")
	if snap.StatusCode != 200 || snap.Header.Get("Content-Type") != "image/jpeg" {
		t.Fatalf("snapshot = %d %s", snap.StatusCode, snap.Header.Get("Content-Type"))
	}

	if r := do(t, "DELETE", ts.URL+"/api/v1/library/files/"+f.ID, ""); r.StatusCode != 409 {
		t.Fatalf("delete while in use = %d, want 409", r.StatusCode)
	}
	if _, err := os.Stat(f.Path); err != nil {
		t.Fatalf("the file vanished while in use: %v", err)
	}
	if r := do(t, "DELETE", ts.URL+"/api/v1/cameras/flow-cam", ""); r.StatusCode != 204 {
		t.Fatalf("delete camera = %d", r.StatusCode)
	}
	if r := do(t, "DELETE", ts.URL+"/api/v1/library/files/"+f.ID, ""); r.StatusCode != 204 {
		t.Fatalf("delete after the camera is gone = %d, want 204", r.StatusCode)
	}
	if _, err := os.Stat(f.Path); !os.IsNotExist(err) {
		t.Errorf("file still on disk: %v", err)
	}
}
