package camfarm

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/0x524a/camfarm/internal/media/fixture"
)

func codecOf(t *testing.T, f *Fleet, id string) string {
	t.Helper()
	for _, st := range f.List() {
		if st.ID == id {
			return st.Codec
		}
	}
	t.Fatalf("camera %q not listed", id)
	return ""
}

// TestAddCameraReparsesAFileThatChanged proves a file overwritten in place
// after its last camera was removed is parsed again, not served from the cache
// of the old contents.
func TestAddCameraReparsesAFileThatChanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clip.ts")
	if err := os.WriteFile(path, fixture.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	f := StartT(t, minimalSpec())
	src := SourceSpec{Kind: SourceFile, Path: path}

	if _, err := f.AddCamera(CameraSpec{ID: "a", Source: src}); err != nil {
		t.Fatalf("AddCamera: %v", err)
	}
	if got := codecOf(t, f, "a"); got != "H264" {
		t.Fatalf("first codec = %q, want H264", got)
	}
	if err := f.RemoveCamera("a"); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, fixture.BytesH265(), 0o600); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
	if _, err := f.AddCamera(CameraSpec{ID: "b", Source: src}); err != nil {
		t.Fatalf("AddCamera after overwrite: %v", err)
	}
	if got := codecOf(t, f, "b"); got != "H265" {
		t.Fatalf("codec after overwrite = %q, want H265 (stale cache)", got)
	}
}

// TestAddCameraReparsesWhileAnotherCameraHoldsTheOldFile proves the key also
// covers the case where the old parse is still in use: the new camera gets the
// new media and the old camera keeps the old.
func TestAddCameraReparsesWhileAnotherCameraHoldsTheOldFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clip.ts")
	if err := os.WriteFile(path, fixture.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	f := StartT(t, minimalSpec())
	src := SourceSpec{Kind: SourceFile, Path: path}
	if _, err := f.AddCamera(CameraSpec{ID: "a", Source: src}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, fixture.BytesH265(), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.AddCamera(CameraSpec{ID: "b", Source: src}); err != nil {
		t.Fatal(err)
	}
	if a, b := codecOf(t, f, "a"), codecOf(t, f, "b"); a != "H264" || b != "H265" {
		t.Fatalf("codecs a=%q b=%q, want H264 and H265", a, b)
	}
}

func (f *Fleet) cachedSources() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sources)
}

// TestRemovingTheLastCameraEvictsAFileSource proves the cache does not keep
// every file ever used, and that fixture sources stay for the fleet's life.
func TestRemovingTheLastCameraEvictsAFileSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clip.ts")
	if err := os.WriteFile(path, fixture.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	f := StartT(t, minimalSpec()) // two fixture cameras: one cached fixture source
	base := f.cachedSources()
	src := SourceSpec{Kind: SourceFile, Path: path}

	for _, id := range []string{"a", "b"} {
		if _, err := f.AddCamera(CameraSpec{ID: id, Source: src}); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.cachedSources(); got != base+1 {
		t.Fatalf("cached = %d, want %d (two cameras share one file entry)", got, base+1)
	}
	if err := f.RemoveCamera("a"); err != nil {
		t.Fatal(err)
	}
	if got := f.cachedSources(); got != base+1 {
		t.Fatalf("cached after removing one of two = %d, want %d", got, base+1)
	}
	if err := f.RemoveCamera("b"); err != nil {
		t.Fatal(err)
	}
	if got := f.cachedSources(); got != base {
		t.Fatalf("cached after removing the last = %d, want %d", got, base)
	}
	for _, id := range []string{"front-door", "lobby"} {
		if err := f.RemoveCamera(id); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.cachedSources(); got != base {
		t.Fatalf("fixture entry evicted: cached = %d, want %d", got, base)
	}
}
