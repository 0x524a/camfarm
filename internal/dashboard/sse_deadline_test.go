package dashboard

import (
	"context"
	"errors"
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

// failingWriter accepts failAfter writes, then errors like a connection whose
// peer went away or whose write deadline expired.
type failingWriter struct {
	*httptest.ResponseRecorder
	mu        sync.Mutex
	failAfter int
	writes    int
}

func (w *failingWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writes++
	if w.writes > w.failAfter {
		return 0, errors.New("write: connection reset by peer")
	}
	return w.ResponseRecorder.Write(b)
}

func (w *failingWriter) writeCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writes
}

// A failed write must end the handler promptly instead of leaving the
// goroutine (and its fleet subscription) parked until the request context ends.
func TestSSEHandlerReturnsWhenAWriteFails(t *testing.T) {
	for _, tc := range []struct {
		name      string
		failAfter int
	}{
		{"initial comment fails", 0},
		{"first event fails", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := camfarm.StartT(t, camfarm.Spec{Seed: 1, Cameras: []camfarm.CameraSpec{{ID: "front-door"}}})
			h := Handler(f, Options{})
			// The context is only cancelled by the deferred cleanup: the handler
			// has to exit on its own because of the failed write.
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil).WithContext(ctx)
			w := &failingWriter{ResponseRecorder: httptest.NewRecorder(), failAfter: tc.failAfter}
			done := make(chan struct{})
			go func() { h.ServeHTTP(w, req); close(done) }()

			if tc.failAfter > 0 {
				waitFor(t, "stream to open", func() bool { return w.writeCount() >= 1 })
				if _, err := f.AddCamera(camfarm.CameraSpec{ID: "lobby"}); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("handler still running after a failed write")
			}
		})
	}
}

// Closing the fleet ends every subscriber's channel; the handler must notice
// and return rather than wait for the client to disconnect.
func TestSSEHandlerReturnsWhenTheFleetCloses(t *testing.T) {
	f := camfarm.StartT(t, camfarm.Spec{Seed: 1, Cameras: []camfarm.CameraSpec{{ID: "front-door"}}})
	h := Handler(f, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil).WithContext(ctx)
	w := &deadlineWriter{ResponseRecorder: httptest.NewRecorder()}
	done := make(chan struct{})
	go func() { h.ServeHTTP(w, req); close(done) }()
	waitFor(t, "stream to open", func() bool { return strings.Contains(w.body(), ": connected") })

	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler still running after the fleet closed")
	}
}
