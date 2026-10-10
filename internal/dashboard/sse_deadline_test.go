package dashboard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0x524a/camfarm"
)

// deadlineWriter records the write deadlines the handler sets. A stalled
// client cannot be simulated reliably (kernel socket buffers swallow megabytes
// first), so the contract is pinned at the seam instead: every SSE write is
// preceded by a fresh, finite deadline.
type deadlineWriter struct {
	*httptest.ResponseRecorder
	mu        sync.Mutex
	deadlines []time.Time
}

func (d *deadlineWriter) SetWriteDeadline(t time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.deadlines = append(d.deadlines, t)
	return nil
}

func (d *deadlineWriter) Write(b []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.ResponseRecorder.Write(b)
}

func (d *deadlineWriter) body() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.ResponseRecorder.Body.String()
}

func (d *deadlineWriter) deadlineList() []time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]time.Time(nil), d.deadlines...)
}

func TestSSEWritesEachSetAFiniteDeadline(t *testing.T) {
	f := camfarm.StartT(t, camfarm.Spec{Seed: 1, Cameras: []camfarm.CameraSpec{{ID: "front-door"}}})
	h := Handler(f, Options{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil).WithContext(ctx)
	w := &deadlineWriter{ResponseRecorder: httptest.NewRecorder()}
	done := make(chan struct{})
	go func() { h.ServeHTTP(w, req); close(done) }()

	waitFor(t, "stream to open", func() bool { return strings.Contains(w.body(), ": connected") })
	if _, err := f.AddCamera(camfarm.CameraSpec{ID: "lobby"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "camera_added event", func() bool { return strings.Contains(w.body(), "event: camera_added") })
	cancel()
	<-done

	ds := w.deadlineList()
	if len(ds) < 2 {
		t.Fatalf("got %d write deadlines, want one per write (>= 2)", len(ds))
	}
	for i, d := range ds {
		if rem := time.Until(d); rem <= 0 || rem > sseWriteTimeout+time.Second {
			t.Errorf("deadline %d is %v away, want within (0, %v]", i, rem, sseWriteTimeout)
		}
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
