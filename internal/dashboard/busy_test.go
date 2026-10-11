package dashboard

import (
	"io"
	"mime/multipart"
	"net/http"
	"testing"
	"time"

	"github.com/0x524a/camfarm/internal/library"
	"github.com/0x524a/camfarm/internal/media/fixture"
)

// heldUpload is an upload whose file part never finishes, so the server keeps
// its upload slot until release is called.
type heldUpload struct {
	pw   *io.PipeWriter
	done chan struct{}
}

func startHeldUpload(t *testing.T, base string) *heldUpload {
	t.Helper()
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	req, err := http.NewRequest("POST", base+"/api/v1/library/upload", pr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	h := &heldUpload{pw: pw, done: make(chan struct{})}
	go func() {
		defer close(h.done)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	go func() {
		if fw, err := mw.CreateFormFile("file", "held.ts"); err == nil {
			_, _ = fw.Write([]byte("start"))
		}
	}()
	return h
}

func (h *heldUpload) release() {
	_ = h.pw.CloseWithError(io.ErrClosedPipe)
	<-h.done
}

func (h *heldUpload) finished() bool {
	select {
	case <-h.done:
		return true
	default:
		return false
	}
}

func TestUploadBeyondTheCapIs429WithRetryAfter(t *testing.T) {
	ts, _, _ := libServer(t, library.Options{})
	held := make([]*heldUpload, 4)
	for i := range held {
		held[i] = startHeldUpload(t, ts.URL)
	}
	t.Cleanup(func() {
		for _, h := range held {
			h.release()
		}
	})

	// The held uploads take their slots once the server is reading their parts.
	// A probe that wins a slot first makes one held upload end early with a 429;
	// that one is restarted. Probes accepted before the slots fill leave a tiny
	// invalid file behind, which is harmless here.
	deadline := time.Now().Add(10 * time.Second)
	for {
		restarted := false
		for i, h := range held {
			if h.finished() {
				held[i] = startHeldUpload(t, ts.URL)
				restarted = true
			}
		}
		if restarted || time.Until(deadline) > 9*time.Second {
			// Give the held uploads time to reach the server before probing, or
			// the probe takes the slot they were about to use.
			time.Sleep(300 * time.Millisecond)
		}
		resp := postFile(t, ts.URL, "extra.mp4", fixture.Bytes()[:200])
		if resp.StatusCode == http.StatusTooManyRequests {
			if resp.Header.Get("Retry-After") == "" {
				t.Error("429 without Retry-After")
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("never saw 429; last status %d", resp.StatusCode)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
