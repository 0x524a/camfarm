package library

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/0x524a/camfarm/internal/media/fixture"
)

func TestScanPrunesAVanishedRootAndKeepsTheOthers(t *testing.T) {
	keep, gone := t.TempDir(), t.TempDir()
	write(t, filepath.Join(keep, "keep.ts"), fixture.Bytes())
	write(t, filepath.Join(gone, "gone.ts"), fixture.Bytes())
	l := newLib(t, Options{Roots: []string{keep, gone}})
	scanAll(t, l)
	if len(l.List()) != 2 {
		t.Fatalf("setup: %v", names(l))
	}
	goneRoot := l.Roots()[1].Path
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}

	err := l.Scan(t.Context(), "", false)
	if err == nil {
		t.Fatal("Scan reported no error for a vanished root")
	}
	var re *RootError
	if !errors.As(err, &re) || re.Root != goneRoot || !errors.Is(err, ErrRootUnavailable) {
		t.Errorf("err = %v, want a RootError for %s", err, goneRoot)
	}
	if got := RootErrors(err); len(got) != 1 || got[0].Root != goneRoot {
		t.Errorf("RootErrors = %v", got)
	}
	if got := names(l); len(got) != 1 || got[0] != "keep.ts" {
		t.Errorf("entries = %v, want only keep.ts", got)
	}

	// A later scan keeps reporting it, and keeps serving the other root.
	if err := l.Scan(t.Context(), "", false); len(RootErrors(err)) != 1 {
		t.Errorf("second scan err = %v", err)
	}
	if got := names(l); len(got) != 1 {
		t.Errorf("entries after second scan = %v", got)
	}
}
