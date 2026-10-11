package dashboard

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/0x524a/camfarm/internal/library"
	"github.com/0x524a/camfarm/internal/media/fixture"
)

type scanView struct {
	libraryDTO
	ScanErrors *[]string `json:"scanErrors"`
}

func decodeScanView(t *testing.T, body []byte) scanView {
	t.Helper()
	var v scanView
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return v
}

func readAll(t *testing.T, r io.Reader) []byte {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestScanWithAVanishedRootStill200sAndNamesIt(t *testing.T) {
	gone := t.TempDir()
	_ = os.WriteFile(filepath.Join(gone, "gone.ts"), fixture.Bytes(), 0o644)
	ts, lib, root := libServer(t, library.Options{Roots: []string{gone}})
	_ = os.WriteFile(filepath.Join(root, "keep.ts"), fixture.Bytes(), 0o644)
	goneRoot := lib.Roots()[1].Path

	first := do(t, "POST", ts.URL+"/api/v1/library/scan", "")
	v := decodeScanView(t, readAll(t, first.Body))
	if first.StatusCode != 200 || len(v.Files) != 2 || v.ScanErrors == nil || len(*v.ScanErrors) != 0 {
		t.Fatalf("setup: %d files=%d scanErrors=%v (an empty list, never null)", first.StatusCode, len(v.Files), v.ScanErrors)
	}

	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}
	resp := do(t, "POST", ts.URL+"/api/v1/library/scan", "")
	body := readAll(t, resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("scan with a vanished root = %d %s, want 200", resp.StatusCode, body)
	}
	v = decodeScanView(t, body)
	if v.ScanErrors == nil || len(*v.ScanErrors) != 1 || (*v.ScanErrors)[0] != "media directory unavailable: "+goneRoot {
		t.Errorf("scanErrors = %v", v.ScanErrors)
	}
	if len(v.Files) != 1 || v.Files[0].Name != "keep.ts" {
		t.Errorf("files = %+v, want only keep.ts", v.Files)
	}
}

func TestAddRootStillSucceedsWhenAnEarlierRootVanished(t *testing.T) {
	first := t.TempDir()
	ts, lib, _ := libServer(t, library.Options{AllowUIPaths: true})
	if r := do(t, "POST", ts.URL+"/api/v1/library/roots", `{"path":"`+first+`"}`); r.StatusCode != 200 {
		t.Fatalf("add first root = %d", r.StatusCode)
	}
	firstRoot := lib.Roots()[1].Path
	if err := os.RemoveAll(first); err != nil {
		t.Fatal(err)
	}

	second := t.TempDir()
	_ = os.WriteFile(filepath.Join(second, "new.ts"), fixture.Bytes(), 0o644)
	resp := do(t, "POST", ts.URL+"/api/v1/library/roots", `{"path":"`+second+`"}`)
	body := readAll(t, resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("addRoot = %d %s, want 200", resp.StatusCode, body)
	}
	v := decodeScanView(t, body)
	if v.ScanErrors == nil || len(*v.ScanErrors) != 1 || (*v.ScanErrors)[0] != "media directory unavailable: "+firstRoot {
		t.Errorf("scanErrors = %v", v.ScanErrors)
	}
	if len(v.Files) != 1 || v.Files[0].Name != "new.ts" {
		t.Errorf("files = %+v, want the new root's file", v.Files)
	}
}
