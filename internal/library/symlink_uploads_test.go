package library

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/0x524a/camfarm/internal/media/fixture"
)

// swapUploadsForLink replaces <root>/uploads with a symlink to a directory
// outside the root and returns that directory.
func swapUploadsForLink(t *testing.T, root string) string {
	t.Helper()
	outside := t.TempDir()
	if err := os.Rename(filepath.Join(root, "uploads"), filepath.Join(root, "uploads-moved")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "uploads")); err != nil {
		t.Fatal(err)
	}
	return outside
}

func TestDeleteCannotEscapeThroughASymlinkedUploadsDirectory(t *testing.T) {
	l, root := upLib(t, 0)
	e, err := l.Upload(context.Background(), "clip.ts", bytes.NewReader(fixture.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	outside := swapUploadsForLink(t, root)
	victim := filepath.Join(outside, "clip.ts")
	write(t, victim, []byte("precious"))

	if err := l.Delete(e.ID); err == nil {
		t.Error("Delete succeeded through a symlinked uploads directory")
	}
	if b, err := os.ReadFile(victim); err != nil || string(b) != "precious" {
		t.Errorf("the file outside the media directory was touched: %q, %v", b, err)
	}
}

func TestUploadRefusesASymlinkedUploadsDirectoryEveryTime(t *testing.T) {
	l, root := upLib(t, 0)
	if _, err := l.Upload(context.Background(), "first.ts", bytes.NewReader(fixture.Bytes())); err != nil {
		t.Fatal(err)
	}
	outside := swapUploadsForLink(t, root)
	if _, err := l.Upload(context.Background(), "second.ts", bytes.NewReader(fixture.Bytes())); err == nil {
		t.Error("Upload succeeded through a symlinked uploads directory")
	}
	if es, _ := os.ReadDir(outside); len(es) != 0 {
		t.Errorf("Upload wrote outside the media directory: %v", es)
	}
}

func TestUploadDoesNotRecreateAVanishedMediaDirectory(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "media")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	l := newLib(t, Options{Roots: []string{root}})
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Upload(context.Background(), "a.ts", bytes.NewReader(fixture.Bytes())); err == nil {
		t.Error("Upload succeeded into a vanished media directory")
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Errorf("the media directory was recreated: %v", err)
	}
}
