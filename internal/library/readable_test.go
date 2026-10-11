package library

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// unreadableDir makes a directory that exists but cannot be listed. It skips
// when permission bits do not bind (root) and restores them on cleanup.
func unreadableDir(t *testing.T) string {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("permission bits do not bind root")
	}
	dir := filepath.Join(t.TempDir(), "locked")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	return dir
}

func TestNewRejectsAnUnreadableMediaDirectory(t *testing.T) {
	dir := unreadableDir(t)
	_, err := New(Options{Roots: []string{dir}})
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("err = %v, want a permission error", err)
	}
}

func TestAddRootRejectsAnUnreadableDirectory(t *testing.T) {
	dir := unreadableDir(t)
	l := newLib(t, Options{AllowUIPaths: true})
	if err := l.AddRoot(dir); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("err = %v, want a permission error", err)
	}
	if len(l.Roots()) != 0 {
		t.Errorf("an unreadable directory was added: %v", l.Roots())
	}
}

func TestAnEmptyDirectoryIsAValidRoot(t *testing.T) {
	if _, err := New(Options{Roots: []string{t.TempDir()}}); err != nil {
		t.Fatalf("empty directory rejected: %v", err)
	}
}
