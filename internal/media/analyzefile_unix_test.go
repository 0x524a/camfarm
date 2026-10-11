//go:build unix

package media

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// Opening a FIFO blocks until a writer appears; AnalyzeFile must reject it
// before it ever calls open.
func TestAnalyzeFileFifoDoesNotBlock(t *testing.T) {
	p := filepath.Join(t.TempDir(), "pipe")
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan Report, 1)
	go func() { done <- AnalyzeFile(p) }()
	select {
	case r := <-done:
		if got := firstInvalid(t, r); got.Code != "unreadable" {
			t.Errorf("code = %q (%+v)", got.Code, got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("AnalyzeFile blocked on a FIFO")
	}
}
