package library

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0x524a/camfarm/internal/media"
	"github.com/0x524a/camfarm/internal/media/fixture"
)

func write(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func newLib(t *testing.T, opt Options) *Library {
	t.Helper()
	l, err := New(opt)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func scanAll(t *testing.T, l *Library) {
	t.Helper()
	if err := l.Scan(context.Background(), "", false); err != nil {
		t.Fatal(err)
	}
}

func names(l *Library) []string {
	var out []string
	for _, e := range l.List() {
		out = append(out, e.Name)
	}
	return out
}

func TestScanListsVideoFilesWithRealReports(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "good.ts"), fixture.Bytes())
	write(t, filepath.Join(dir, "bad.mp4"), []byte("not video at all, just some text"))
	write(t, filepath.Join(dir, "notes.txt"), []byte("ignored"))
	l := newLib(t, Options{Roots: []string{dir}})
	scanAll(t, l)

	got := l.List()
	if len(got) != 2 || got[0].Name != "bad.mp4" || got[1].Name != "good.ts" {
		t.Fatalf("List = %v", names(l))
	}
	if got[0].Report.Status != media.StatusInvalid || got[1].Report.Status != media.StatusValid {
		t.Errorf("statuses = %q, %q", got[0].Report.Status, got[1].Report.Status)
	}
	if got[1].Size != int64(len(fixture.Bytes())) || got[1].Path != filepath.Join(l.Roots()[0].Path, "good.ts") {
		t.Errorf("entry = %+v", got[1])
	}
	if e, ok := l.Resolve(got[1].ID); !ok || e.Path != got[1].Path {
		t.Errorf("Resolve = %+v, %v", e, ok)
	}
	if e, ok := l.ByPath(got[1].Path); !ok || e.ID != got[1].ID {
		t.Errorf("ByPath = %+v, %v", e, ok)
	}
}

func TestScanDepthLimit(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.ts"), fixture.Bytes())
	write(t, filepath.Join(dir, "x", "b.ts"), fixture.Bytes())
	write(t, filepath.Join(dir, "x", "y", "c.ts"), fixture.Bytes())
	write(t, filepath.Join(dir, "x", "y", "z", "d.ts"), fixture.Bytes()) // depth 4: too deep
	l := newLib(t, Options{Roots: []string{dir}})
	scanAll(t, l)
	want := []string{"a.ts", filepath.Join("x", "b.ts"), filepath.Join("x", "y", "c.ts")}
	if got := names(l); len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("names = %v, want %v", got, want)
	}
}

func TestScanSkipsSymlinksAndHiddenFiles(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	write(t, filepath.Join(outside, "secret.ts"), fixture.Bytes())
	write(t, filepath.Join(dir, "real.ts"), fixture.Bytes())
	write(t, filepath.Join(dir, ".hidden.ts"), fixture.Bytes())
	write(t, filepath.Join(dir, ".upload-1.part"), []byte("partial"))
	if err := os.Symlink(filepath.Join(outside, "secret.ts"), filepath.Join(dir, "link.ts")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "linkdir")); err != nil {
		t.Fatal(err)
	}
	l := newLib(t, Options{Roots: []string{dir}})
	scanAll(t, l)
	if got := names(l); len(got) != 1 || got[0] != "real.ts" {
		t.Errorf("names = %v, want only real.ts", got)
	}
}

func TestScanCacheAndForce(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.ts"), filepath.Join(dir, "b.ts")
	write(t, a, fixture.Bytes())
	write(t, b, fixture.Bytes())
	var calls atomic.Int32
	l := newLib(t, Options{Roots: []string{dir}, Analyze: func(p string) media.Report {
		calls.Add(1)
		return media.AnalyzeFile(p)
	}})
	scanAll(t, l)
	if calls.Load() != 2 {
		t.Fatalf("first scan analysed %d files, want 2", calls.Load())
	}
	scanAll(t, l)
	if calls.Load() != 2 {
		t.Errorf("unchanged rescan analysed again (%d calls)", calls.Load())
	}
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(a, future, future); err != nil {
		t.Fatal(err)
	}
	scanAll(t, l)
	if calls.Load() != 3 {
		t.Errorf("after touching one file: %d calls, want 3", calls.Load())
	}
	if err := l.Scan(context.Background(), "", true); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 5 {
		t.Errorf("forced scan: %d calls, want 5", calls.Load())
	}
}

func TestScanDropsVanishedFiles(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "gone.ts")
	write(t, p, fixture.Bytes())
	l := newLib(t, Options{Roots: []string{dir}})
	scanAll(t, l)
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	scanAll(t, l)
	if len(l.List()) != 0 {
		t.Errorf("List = %v after the file was removed", names(l))
	}
}

func TestScanUnknownRootAndMissingDirectory(t *testing.T) {
	dir := t.TempDir()
	l := newLib(t, Options{Roots: []string{dir}})
	if err := l.Scan(context.Background(), "/not/a/root", false); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown root: %v", err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := l.Scan(context.Background(), "", false); err == nil {
		t.Error("scanning a root that was deleted: want an error")
	}
}

func TestAnalyzeTimeoutIsAnInvalidEntry(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "slow.ts"), fixture.Bytes())
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	l := newLib(t, Options{Roots: []string{dir}, Timeout: 50 * time.Millisecond,
		Analyze: func(string) media.Report { <-release; return media.Report{} }})
	scanAll(t, l)
	es := l.List()
	if len(es) != 1 || es[0].Report.Status != media.StatusInvalid || es[0].Report.Issues[0].Code != "loader_error" {
		t.Errorf("List = %+v", es)
	}
}

func TestNewRejectsBadRoots(t *testing.T) {
	f := filepath.Join(t.TempDir(), "file")
	write(t, f, []byte("x"))
	for name, root := range map[string]string{"missing": filepath.Join(t.TempDir(), "nope"), "a file": f} {
		if _, err := New(Options{Roots: []string{root}}); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestRootsReportWritabilityAndUploadTarget(t *testing.T) {
	rw := t.TempDir()
	l := newLib(t, Options{Roots: []string{rw}})
	r := l.Roots()
	if len(r) != 1 || !r[0].Writable || r[0].UI || !l.UploadEnabled() {
		t.Errorf("roots = %+v, upload %v", r, l.UploadEnabled())
	}
	if os.Geteuid() != 0 {
		ro := t.TempDir()
		if err := os.Chmod(ro, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(ro, 0o755) })
		l2 := newLib(t, Options{Roots: []string{ro}})
		if l2.Roots()[0].Writable || l2.UploadEnabled() {
			t.Errorf("read-only root reported writable: %+v", l2.Roots())
		}
	}
	if l3 := newLib(t, Options{}); l3.UploadEnabled() || len(l3.Roots()) != 0 {
		t.Error("a library with no roots must not allow uploads")
	}
}

func TestAddRootIsGated(t *testing.T) {
	dir := t.TempDir()
	if err := newLib(t, Options{}).AddRoot(dir); !errors.Is(err, ErrUIPathsDisabled) {
		t.Errorf("AddRoot without the flag = %v", err)
	}
	l := newLib(t, Options{AllowUIPaths: true})
	if err := l.AddRoot(dir); err != nil {
		t.Fatal(err)
	}
	if err := l.AddRoot(dir); err != nil || len(l.Roots()) != 1 {
		t.Errorf("adding the same root twice: err %v, roots %+v", err, l.Roots())
	}
	if r := l.Roots()[0]; !r.UI || r.Writable || l.UploadEnabled() {
		t.Errorf("a UI root must never be an upload target: %+v", r)
	}
	if err := l.AddRoot(filepath.Join(dir, "nope")); err == nil {
		t.Error("nonexistent path: want an error")
	}
	f := filepath.Join(dir, "f")
	write(t, f, []byte("x"))
	if err := l.AddRoot(f); !errors.Is(err, ErrNotDirectory) {
		t.Errorf("file path = %v", err)
	}
}

func TestRefresh(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.ts")
	write(t, p, fixture.Bytes())
	var calls atomic.Int32
	l := newLib(t, Options{Roots: []string{dir}, Analyze: func(p string) media.Report {
		calls.Add(1)
		return media.AnalyzeFile(p)
	}})
	scanAll(t, l)
	id := l.List()[0].ID

	if _, err := l.Refresh(context.Background(), id); err != nil || calls.Load() != 1 {
		t.Fatalf("unchanged: err %v, calls %d (want 1: no re-analysis)", err, calls.Load())
	}
	write(t, p, []byte("overwritten with text"))
	e, err := l.Refresh(context.Background(), id)
	if err != nil || calls.Load() != 2 || e.Report.Status != media.StatusInvalid {
		t.Fatalf("changed: err %v, calls %d, status %q", err, calls.Load(), e.Report.Status)
	}
	if got, _ := l.Resolve(id); got.Report.Status != media.StatusInvalid {
		t.Error("Refresh did not update the cached entry")
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Refresh(context.Background(), id); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleted file: %v", err)
	}
	if _, ok := l.Resolve(id); ok {
		t.Error("a vanished file stayed in the library")
	}
	if _, err := l.Refresh(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown id: %v", err)
	}
}

func TestConcurrentScanAndListIsRaceFree(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"a.ts", "b.ts", "c.ts"} {
		write(t, filepath.Join(dir, n), fixture.Bytes())
	}
	l := newLib(t, Options{Roots: []string{dir}})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); _ = l.Scan(context.Background(), "", i%2 == 0) }()
		go func() { defer wg.Done(); _ = l.List(); _ = l.Scanning(); _ = l.Roots() }()
	}
	wg.Wait()
	if len(l.List()) != 3 {
		t.Errorf("List = %v", names(l))
	}
}
