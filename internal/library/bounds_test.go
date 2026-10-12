package library

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0x524a/camfarm/internal/media"
	"github.com/0x524a/camfarm/internal/media/fixture"
)

// gate is a reader that delivers nothing until released, so an upload holding
// one blocks inside Upload.
type gate struct{ release chan struct{} }

func (g gate) Read([]byte) (int, error) {
	<-g.release
	return 0, io.EOF
}

func TestUploadsNeverRunMoreAnalysesThanWorkers(t *testing.T) {
	var cur, peak atomic.Int32
	l := newLib(t, Options{Roots: []string{t.TempDir()}, Workers: 1, Analyze: func(string) media.Report {
		n := cur.Add(1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		time.Sleep(60 * time.Millisecond)
		cur.Add(-1)
		return media.Report{Status: media.StatusValid}
	}})
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := l.Upload(context.Background(), "a.ts", bytes.NewReader(fixture.Bytes())); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if p := peak.Load(); p != 1 {
		t.Errorf("peak concurrent analyses = %d, want 1 with Workers=1", p)
	}
}

func TestRefreshTakesTheAnalysisSlot(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.ts"), fixture.Bytes())
	write(t, filepath.Join(dir, "b.ts"), fixture.Bytes())
	var cur, peak atomic.Int32
	slow := false
	l := newLib(t, Options{Roots: []string{dir}, Workers: 1, Analyze: func(string) media.Report {
		if slow {
			n := cur.Add(1)
			for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
			}
			time.Sleep(60 * time.Millisecond)
			cur.Add(-1)
		}
		return media.Report{Status: media.StatusValid}
	}})
	scanAll(t, l)
	// Change both files so Refresh must re-analyze them.
	for _, n := range []string{"a.ts", "b.ts"} {
		write(t, filepath.Join(dir, n), append(fixture.Bytes(), 0))
	}
	slow = true
	var wg sync.WaitGroup
	for _, e := range l.List() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := l.Refresh(context.Background(), e.ID); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if p := peak.Load(); p != 1 {
		t.Errorf("peak concurrent analyses = %d, want 1 with Workers=1", p)
	}
}

func TestUploadBeyondTheInFlightCapIsBusy(t *testing.T) {
	l := newLib(t, Options{Roots: []string{t.TempDir()}})
	g := gate{release: make(chan struct{})}
	var wg sync.WaitGroup
	for i := 0; i < maxConcurrentUploads; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = l.Upload(context.Background(), "held.ts", g)
		}()
	}
	// The held uploads own their slots once they are blocked reading.
	deadline := time.Now().Add(5 * time.Second)
	for l.uploadsInFlight() < maxConcurrentUploads {
		if time.Now().After(deadline) {
			t.Fatalf("only %d uploads in flight", l.uploadsInFlight())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := l.Upload(context.Background(), "extra.ts", bytes.NewReader(fixture.Bytes())); !errors.Is(err, ErrBusy) {
		t.Errorf("fifth upload err = %v, want ErrBusy", err)
	}
	close(g.release)
	wg.Wait()
	if _, err := l.Upload(context.Background(), "after.ts", bytes.NewReader(fixture.Bytes())); err != nil {
		t.Errorf("upload after the others finished: %v", err)
	}
}

func TestUploadSlotsAreReleasedOnEveryReturnPath(t *testing.T) {
	l := newLib(t, Options{Roots: []string{t.TempDir()}, MaxUpload: 1000})
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	boom := errors.New("boom")
	paths := map[string]func() error{
		"too large": func() error {
			_, e := l.Upload(context.Background(), "a.ts", bytes.NewReader(make([]byte, 5000)))
			return e
		},
		"reader error": func() error { _, e := l.Upload(context.Background(), "a.ts", errReader{boom}); return e },
		"cancelled":    func() error { _, e := l.Upload(cancelled, "a.ts", bytes.NewReader([]byte("x"))); return e },
		"bad name":     func() error { _, e := l.Upload(context.Background(), "a.txt", bytes.NewReader([]byte("x"))); return e },
		"ok":           func() error { _, e := l.Upload(context.Background(), "a.ts", bytes.NewReader([]byte("x"))); return e },
	}
	for name, fn := range paths {
		// More rounds than slots: a leak on any path would surface as ErrBusy.
		for i := 0; i < maxConcurrentUploads+2; i++ {
			if err := fn(); errors.Is(err, ErrBusy) {
				t.Fatalf("%s: slot leaked after %d calls", name, i)
			}
		}
	}
	if n := l.uploadsInFlight(); n != 0 {
		t.Errorf("uploadsInFlight = %d, want 0", n)
	}
}

// A context cancelled while waiting for an analysis slot must not leak the
// upload slot or leave a part file.
func TestUploadCancelledWhileWaitingForAnAnalysisSlot(t *testing.T) {
	root := t.TempDir()
	block := make(chan struct{})
	l := newLib(t, Options{Roots: []string{root}, Workers: 1, Analyze: func(string) media.Report {
		<-block
		return media.Report{Status: media.StatusValid}
	}})
	first := make(chan error, 1)
	go func() {
		_, err := l.Upload(context.Background(), "a.ts", bytes.NewReader(fixture.Bytes()))
		first <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for len(l.sem) == 0 { // the first upload is analysing
		if time.Now().After(deadline) {
			close(block)
			t.Fatal("the first upload never took an analysis slot")
		}
		time.Sleep(5 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := l.Upload(ctx, "b.ts", bytes.NewReader(fixture.Bytes())); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want DeadlineExceeded", err)
	}
	close(block)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if n := l.uploadsInFlight(); n != 0 {
		t.Errorf("uploadsInFlight = %d, want 0", n)
	}
	if got := uploadEntries(t, root); len(got) != 1 {
		t.Errorf("uploads = %v, want only a.ts", got)
	}
}
