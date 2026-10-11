package library

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/0x524a/camfarm/internal/media"
	"github.com/0x524a/camfarm/internal/media/fixture"
)

func TestCleanPartFilesOnlyRemovesOldOnes(t *testing.T) {
	dir := t.TempDir()
	up := filepath.Join(dir, "uploads")
	old := filepath.Join(up, ".upload-old.part")
	fresh := filepath.Join(up, ".upload-fresh.part")
	write(t, old, []byte("stale"))
	write(t, fresh, []byte("another process is writing this"))
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	newLib(t, Options{Roots: []string{dir}})
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("a part file older than the grace period survived: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("a recent part file (maybe another process's upload) was removed: %v", err)
	}
}

func TestByPathPicksTheLowestIDWhenRootsOverlap(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "sub", "x.ts"), fixture.Bytes())
	l := newLib(t, Options{Roots: []string{dir, filepath.Join(dir, "sub")}})
	scanAll(t, l)
	es := l.List()
	if len(es) != 2 || es[0].Path != es[1].Path {
		t.Fatalf("setup: %+v", es)
	}
	want := min(es[0].ID, es[1].ID)
	for i := 0; i < 100; i++ { // map order is random; the answer must not be
		got, ok := l.ByPath(es[0].Path)
		if !ok || got.ID != want {
			t.Fatalf("ByPath = %q (ok=%v), want the lowest id %q", got.ID, ok, want)
		}
	}
}

func TestScanStopsWalkingWhenTheContextEnds(t *testing.T) {
	dir := t.TempDir()
	const files = 200
	for i := 0; i < files; i++ {
		write(t, filepath.Join(dir, "d", string(rune('a'+i%26)), "f"+string(rune('a'+i/26))+".txt"), nil)
	}
	l := newLib(t, Options{Roots: []string{dir}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	visited := 0
	walkVisit = func(string) {
		visited++
		if visited == 5 {
			cancel()
		}
	}
	t.Cleanup(func() { walkVisit = nil })
	if err := l.Scan(ctx, "", false); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if visited > 10 {
		t.Errorf("walk visited %d entries after cancellation, want it to stop at once", visited)
	}
}

func TestTimeoutReportCarriesTheFileSize(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "slow.ts"), fixture.Bytes())
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	l := newLib(t, Options{Roots: []string{dir}, Timeout: 30 * time.Millisecond, Analyze: func(string) media.Report {
		<-block
		return media.Report{}
	}})
	scanAll(t, l)
	es := l.List()
	if len(es) != 1 || es[0].Report.Status != media.StatusInvalid {
		t.Fatalf("entries = %+v", es)
	}
	if got, want := es[0].Report.Facts.SizeBytes, int64(len(fixture.Bytes())); got != want {
		t.Errorf("SizeBytes = %d, want %d", got, want)
	}
}
