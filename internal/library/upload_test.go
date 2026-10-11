package library

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/0x524a/camfarm/internal/media"
	"github.com/0x524a/camfarm/internal/media/fixture"
)

func upLib(t *testing.T, max int64) (*Library, string) {
	t.Helper()
	dir := t.TempDir()
	l := newLib(t, Options{Roots: []string{dir}, MaxUpload: max})
	return l, l.Roots()[0].Path
}

func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}

// uploadEntries lists the uploads/ subdirectory of root; missing is empty.
func uploadEntries(t *testing.T, root string) []string {
	t.Helper()
	es, err := os.ReadDir(filepath.Join(root, "uploads"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}

func TestUploadStoresValidatesAndListsTheFile(t *testing.T) {
	l, root := upLib(t, 0)
	e, err := l.Upload(context.Background(), "clip.ts", bytes.NewReader(fixture.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if e.Report.Status != media.StatusValid || e.Name != "uploads/clip.ts" || e.Path != filepath.Join(root, "uploads", "clip.ts") {
		t.Errorf("entry = %+v", e)
	}
	if got, ok := l.Resolve(e.ID); !ok || got.Path != e.Path {
		t.Error("uploaded file is not in the library")
	}
	if names := dirEntries(t, root); len(names) != 1 || names[0] != "uploads" {
		t.Errorf("root = %v, want only the uploads directory", names)
	}
	if names := uploadEntries(t, root); len(names) != 1 || names[0] != "clip.ts" {
		t.Errorf("uploads = %v (a part file must not be left behind)", names)
	}
	// A rescan must find the very same entry: same id, same name.
	scanAll(t, l)
	if got, ok := l.Resolve(e.ID); !ok || got.Name != "uploads/clip.ts" || len(l.List()) != 1 {
		t.Errorf("after rescan: %+v, %d entries", got, len(l.List()))
	}
}

func TestDeleteRefusesAScannedOriginalInTheWritableRoot(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "original.ts"), fixture.Bytes())
	write(t, filepath.Join(dir, "sub", "deep.ts"), fixture.Bytes())
	l := newLib(t, Options{Roots: []string{dir}})
	scanAll(t, l)
	if !l.UploadEnabled() || len(l.List()) != 2 {
		t.Fatalf("setup: upload=%v entries=%v", l.UploadEnabled(), names(l))
	}
	for _, e := range l.List() {
		if err := l.Delete(e.ID); !errors.Is(err, ErrNotDeletable) {
			t.Errorf("Delete(%s) = %v, want ErrNotDeletable", e.Name, err)
		}
		if _, err := os.Stat(e.Path); err != nil {
			t.Errorf("%s was removed: %v", e.Name, err)
		}
	}
}

func TestDeleteAllowsAFilePlacedByHandUnderUploads(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "uploads", "mine.ts"), fixture.Bytes())
	l := newLib(t, Options{Roots: []string{dir}})
	scanAll(t, l)
	es := l.List()
	if len(es) != 1 || es[0].Name != "uploads/mine.ts" {
		t.Fatalf("entries = %v", names(l))
	}
	if err := l.Delete(es[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(es[0].Path); !os.IsNotExist(err) {
		t.Errorf("file still on disk: %v", err)
	}
}

func TestUploadKeepsAnInvalidFileWithItsReason(t *testing.T) {
	l, root := upLib(t, 0)
	e, err := l.Upload(context.Background(), "junk.mp4", strings.NewReader("definitely not a video file"))
	if err != nil {
		t.Fatal(err)
	}
	if e.Report.Status != media.StatusInvalid || e.Report.Issues[0].Code != "unsupported_container" {
		t.Errorf("report = %+v", e.Report)
	}
	if _, err := os.Stat(filepath.Join(root, "uploads", "junk.mp4")); err != nil {
		t.Errorf("invalid upload was not kept: %v", err)
	}
}

func TestUploadNeverOverwrites(t *testing.T) {
	l, root := upLib(t, 0)
	for i := 0; i < 3; i++ {
		if _, err := l.Upload(context.Background(), "clip.ts", bytes.NewReader(fixture.Bytes())); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"clip-1.ts", "clip-2.ts", "clip.ts"}
	if got := uploadEntries(t, root); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("directory = %v, want %v", got, want)
	}
}

func TestConcurrentUploadsOfOneNameKeepEveryFile(t *testing.T) {
	l, root := upLib(t, 0)
	var wg sync.WaitGroup
	for i := 0; i < maxConcurrentUploads; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := l.Upload(context.Background(), "same.ts", bytes.NewReader(fixture.Bytes())); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if got := uploadEntries(t, root); len(got) != maxConcurrentUploads {
		t.Errorf("directory = %v, want %d distinct files", got, maxConcurrentUploads)
	}
	if len(l.List()) != maxConcurrentUploads {
		t.Errorf("List has %d entries, want %d", len(l.List()), maxConcurrentUploads)
	}
}

func TestUploadOverTheLimitLeavesNothingBehind(t *testing.T) {
	l, root := upLib(t, 1000)
	_, err := l.Upload(context.Background(), "big.ts", bytes.NewReader(make([]byte, 5000)))
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	if got := uploadEntries(t, root); len(got) != 0 {
		t.Errorf("directory = %v, want empty", got)
	}
	// Exactly at the limit is accepted.
	if _, err := l.Upload(context.Background(), "edge.ts", bytes.NewReader(make([]byte, 1000))); err != nil {
		t.Errorf("upload of exactly the limit: %v", err)
	}
}

func TestUploadReaderFailureLeavesNothingBehind(t *testing.T) {
	l, root := upLib(t, 0)
	boom := errors.New("connection reset")
	r := io.MultiReader(bytes.NewReader(fixture.Bytes()[:400]), errReader{boom})
	if _, err := l.Upload(context.Background(), "x.ts", r); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if got := uploadEntries(t, root); len(got) != 0 {
		t.Errorf("directory = %v, want empty", got)
	}
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

func TestUploadNames(t *testing.T) {
	cases := []struct {
		name string
		want string // file created in the root; "" means rejected
	}{
		{"clip.ts", "clip.ts"},
		{"CLIP.MP4", "CLIP.MP4"},
		{"../escape.ts", "escape.ts"},
		{`C:\videos\win.ts`, "win.ts"},
		{"a/b/c.ts", "c.ts"},
		{"", ""},
		{".", ""},
		{"..", ""},
		{".hidden.ts", ""},
		{"no-extension", ""},
		{"notes.txt", ""},
		{"bad\x00name.ts", ""},
		{"tab\tname.ts", ""},
		{strings.Repeat("a", 201) + ".ts", ""},
		{strings.Repeat("a", 197) + ".ts", strings.Repeat("a", 197) + ".ts"},
		{"  clip.ts ", "clip.ts"},
		{"evil\u202Egnp.ts", ""},
		{"bad\xffutf8.ts", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l, root := upLib(t, 0)
			outside := filepath.Join(filepath.Dir(root), "escape.ts")
			_, err := l.Upload(context.Background(), c.name, bytes.NewReader(fixture.Bytes()))
			if c.want == "" {
				if !errors.Is(err, ErrBadName) {
					t.Errorf("err = %v, want ErrBadName", err)
				}
				if got := uploadEntries(t, root); len(got) != 0 {
					t.Errorf("a rejected name left %v behind", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := uploadEntries(t, root); len(got) != 1 || got[0] != c.want {
				t.Errorf("directory = %v, want [%s]", got, c.want)
			}
			if _, err := os.Stat(outside); err == nil {
				t.Error("a file was written outside the upload root")
			}
		})
	}
}

func TestUploadReadOnlyLibrary(t *testing.T) {
	l := newLib(t, Options{})
	if _, err := l.Upload(context.Background(), "a.ts", bytes.NewReader(fixture.Bytes())); !errors.Is(err, ErrReadOnly) {
		t.Errorf("err = %v, want ErrReadOnly", err)
	}
	if err := l.Delete("anything"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete on an empty library = %v", err)
	}
}

func TestNewRemovesLeftoverPartFiles(t *testing.T) {
	dir := t.TempDir()
	part := filepath.Join(dir, "uploads", ".upload-123.part")
	write(t, part, []byte("partial"))
	past := time.Now().Add(-2 * time.Hour) // only parts older than the grace period are leftovers
	if err := os.Chtimes(part, past, past); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "uploads", "keep.ts"), fixture.Bytes())
	newLib(t, Options{Roots: []string{dir}})
	if got := uploadEntries(t, dir); len(got) != 1 || got[0] != "keep.ts" {
		t.Errorf("directory = %v", got)
	}
}

func TestDelete(t *testing.T) {
	l, root := upLib(t, 0)
	e, err := l.Upload(context.Background(), "gone.ts", bytes.NewReader(fixture.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Delete(e.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(e.Path); !os.IsNotExist(err) {
		t.Errorf("file still on disk: %v", err)
	}
	if _, ok := l.Resolve(e.ID); ok {
		t.Error("entry still listed")
	}
	if got := uploadEntries(t, root); len(got) != 0 {
		t.Errorf("directory = %v", got)
	}
	if err := l.Delete(e.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete = %v", err)
	}
}

func TestDeleteRefusesFilesOutsideTheUploadRoot(t *testing.T) {
	up, other := t.TempDir(), t.TempDir()
	write(t, filepath.Join(other, "keep.ts"), fixture.Bytes())
	l := newLib(t, Options{Roots: []string{up, other}})
	scanAll(t, l)
	var id string
	for _, e := range l.List() {
		id = e.ID
	}
	if err := l.Delete(id); !errors.Is(err, ErrNotDeletable) {
		t.Errorf("err = %v, want ErrNotDeletable", err)
	}
	if _, err := os.Stat(filepath.Join(other, "keep.ts")); err != nil {
		t.Errorf("a file outside the upload root was removed: %v", err)
	}
}

func TestUploadScanAndListRaceFree(t *testing.T) {
	l, root := upLib(t, 0)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(3)
		go func() {
			defer wg.Done()
			if _, err := l.Upload(context.Background(), "r.ts", bytes.NewReader(fixture.Bytes())); err != nil {
				t.Error(err)
			}
		}()
		go func() { defer wg.Done(); _ = l.Scan(context.Background(), "", false) }()
		go func() { defer wg.Done(); _ = l.List() }()
	}
	wg.Wait()
	scanAll(t, l)
	if got, want := len(l.List()), len(uploadEntries(t, root)); got != 4 || got != want {
		t.Errorf("listed %d files, %d on disk, want 4 each", got, want)
	}
}

func TestDeleteCannotEscapeThroughASwappedParent(t *testing.T) {
	up, victim := t.TempDir(), t.TempDir()
	write(t, filepath.Join(up, "uploads", "a", "x.ts"), fixture.Bytes())
	write(t, filepath.Join(victim, "x.ts"), fixture.Bytes())
	l := newLib(t, Options{Roots: []string{up}})
	scanAll(t, l)
	es := l.List()
	if len(es) != 1 {
		t.Fatalf("entries = %v", names(l))
	}
	id := es[0].ID
	if err := os.RemoveAll(filepath.Join(up, "uploads", "a")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(up, "uploads", "a")); err != nil {
		t.Fatal(err)
	}
	scanAll(t, l) // the entry must not survive being reached through a link
	if err := l.Delete(id); err == nil {
		t.Error("Delete succeeded through a swapped parent directory")
	}
	if _, err := os.Stat(filepath.Join(victim, "x.ts")); err != nil {
		t.Errorf("a file outside the upload root was removed: %v", err)
	}
}

func TestDeleteThroughASwappedParentWithoutRescan(t *testing.T) {
	up, victim := t.TempDir(), t.TempDir()
	write(t, filepath.Join(up, "uploads", "a", "x.ts"), fixture.Bytes())
	write(t, filepath.Join(victim, "x.ts"), fixture.Bytes())
	l := newLib(t, Options{Roots: []string{up}})
	scanAll(t, l)
	id := l.List()[0].ID
	if err := os.RemoveAll(filepath.Join(up, "uploads", "a")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(up, "uploads", "a")); err != nil {
		t.Fatal(err)
	}
	_ = l.Delete(id)
	if _, err := os.Stat(filepath.Join(victim, "x.ts")); err != nil {
		t.Errorf("a file outside the upload root was removed: %v", err)
	}
}

func TestUploadNeverFollowsAnInRootSymlink(t *testing.T) {
	for _, mode := range []string{"link", "no-hardlinks-fallback"} {
		t.Run(mode, func(t *testing.T) {
			if mode != "link" {
				old := linkFile
				linkFile = func(string, string) error { return &os.LinkError{Op: "link", Err: syscall.EPERM} }
				t.Cleanup(func() { linkFile = old })
			}
			l, root := upLib(t, 0)
			outside := filepath.Join(t.TempDir(), "target.ts")
			write(t, outside, []byte("precious"))
			if err := os.MkdirAll(filepath.Join(root, "uploads"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(root, "uploads", "clip.ts")); err != nil {
				t.Fatal(err)
			}
			e, err := l.Upload(context.Background(), "clip.ts", bytes.NewReader(fixture.Bytes()))
			if err != nil {
				t.Fatal(err)
			}
			if e.Name != "uploads/clip-1.ts" {
				t.Errorf("name = %q, want uploads/clip-1.ts", e.Name)
			}
			if b, _ := os.ReadFile(outside); string(b) != "precious" {
				t.Errorf("the file behind the symlink was overwritten: %q", b)
			}
		})
	}
}

func TestUploadFallsBackWhenHardLinksAreUnsupported(t *testing.T) {
	old := linkFile
	linkFile = func(string, string) error { return &os.LinkError{Op: "link", Err: syscall.EPERM} }
	t.Cleanup(func() { linkFile = old })

	l, root := upLib(t, 0)
	for i := 0; i < 2; i++ {
		if _, err := l.Upload(context.Background(), "clip.ts", bytes.NewReader(fixture.Bytes())); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"clip-1.ts", "clip.ts"}
	if got := uploadEntries(t, root); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("directory = %v, want %v", got, want)
	}
	for _, e := range l.List() {
		if e.Report.Status != media.StatusValid || e.Size != int64(len(fixture.Bytes())) {
			t.Errorf("entry %s = %+v", e.Name, e)
		}
	}
}

func TestCancelledUploadPlacesNothing(t *testing.T) {
	l, root := upLib(t, 0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := l.Upload(ctx, "c.ts", bytes.NewReader(fixture.Bytes())); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
	if got := uploadEntries(t, root); len(got) != 0 || len(l.List()) != 0 {
		t.Errorf("directory = %v, entries = %v", got, names(l))
	}
}

func TestUploadValidatesThePartFileBeforePlacingIt(t *testing.T) {
	dir := t.TempDir()
	var calls int
	l := newLib(t, Options{Roots: []string{dir}, Analyze: func(path string) media.Report {
		calls++
		if b := filepath.Base(path); !strings.HasPrefix(b, ".upload-") || !strings.HasSuffix(b, ".part") {
			t.Errorf("analysed %q, want the part file", b)
		}
		if _, err := os.Lstat(filepath.Join(dir, "uploads", "c.ts")); !os.IsNotExist(err) {
			t.Errorf("the final name already exists during validation: %v", err)
		}
		return media.AnalyzeFile(path)
	}})
	if _, err := l.Upload(context.Background(), "c.ts", bytes.NewReader(fixture.Bytes())); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("analyzer called %d times, want 1", calls)
	}
}

func TestScanKeepsEntriesItCannotStat(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission bits do not bind root")
	}
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	write(t, filepath.Join(sub, "a.ts"), fixture.Bytes())
	l := newLib(t, Options{Roots: []string{dir}})
	scanAll(t, l)
	if err := os.Chmod(sub, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sub, 0o755) })
	scanAll(t, l)
	if len(l.List()) != 1 {
		t.Errorf("an unreadable directory dropped its entries: %v", names(l))
	}
}
