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
	"testing"

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

func TestUploadStoresValidatesAndListsTheFile(t *testing.T) {
	l, root := upLib(t, 0)
	e, err := l.Upload(context.Background(), "clip.ts", bytes.NewReader(fixture.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if e.Report.Status != media.StatusValid || e.Name != "clip.ts" || e.Path != filepath.Join(root, "clip.ts") {
		t.Errorf("entry = %+v", e)
	}
	if got, ok := l.Resolve(e.ID); !ok || got.Path != e.Path {
		t.Error("uploaded file is not in the library")
	}
	if names := dirEntries(t, root); len(names) != 1 || names[0] != "clip.ts" {
		t.Errorf("directory = %v (a part file must not be left behind)", names)
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
	if _, err := os.Stat(filepath.Join(root, "junk.mp4")); err != nil {
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
	if got := dirEntries(t, root); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("directory = %v, want %v", got, want)
	}
}

func TestConcurrentUploadsOfOneNameKeepEveryFile(t *testing.T) {
	l, root := upLib(t, 0)
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := l.Upload(context.Background(), "same.ts", bytes.NewReader(fixture.Bytes())); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if got := dirEntries(t, root); len(got) != 6 {
		t.Errorf("directory = %v, want 6 distinct files", got)
	}
	if len(l.List()) != 6 {
		t.Errorf("List has %d entries, want 6", len(l.List()))
	}
}

func TestUploadOverTheLimitLeavesNothingBehind(t *testing.T) {
	l, root := upLib(t, 1000)
	_, err := l.Upload(context.Background(), "big.ts", bytes.NewReader(make([]byte, 5000)))
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	if got := dirEntries(t, root); len(got) != 0 {
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
	if got := dirEntries(t, root); len(got) != 0 {
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
				if got := dirEntries(t, root); len(got) != 0 {
					t.Errorf("a rejected name left %v behind", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := dirEntries(t, root); len(got) != 1 || got[0] != c.want {
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
	write(t, filepath.Join(dir, ".upload-123.part"), []byte("partial"))
	write(t, filepath.Join(dir, "keep.ts"), fixture.Bytes())
	newLib(t, Options{Roots: []string{dir}})
	if got := dirEntries(t, dir); len(got) != 1 || got[0] != "keep.ts" {
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
	if got := dirEntries(t, root); len(got) != 0 {
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
	l, _ := upLib(t, 0)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(3)
		go func() {
			defer wg.Done()
			_, _ = l.Upload(context.Background(), "r.ts", bytes.NewReader(fixture.Bytes()))
		}()
		go func() { defer wg.Done(); _ = l.Scan(context.Background(), "", false) }()
		go func() { defer wg.Done(); _ = l.List() }()
	}
	wg.Wait()
}
