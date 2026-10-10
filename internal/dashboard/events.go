package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/0x524a/camfarm"
)

func (s *server) getFleet(w http.ResponseWriter, _ *http.Request) {
	j := s.fleet.Journal()
	if j == nil {
		j = []camfarm.JournalEntry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"seed":       strconv.FormatUint(s.fleet.Seed(), 10),
		"specHash":   s.fleet.SpecHash(),
		"replayLine": s.fleet.ReplayLine(),
		"journal":    j,
	})
}

// events streams fleet changes as Server-Sent Events. A client that falls
// behind is dropped by the fleet and sees the stream end; it reconnects and
// re-lists, which is why nothing is replayed.
func (s *server) events(w http.ResponseWriter, r *http.Request) {
	if _, ok := w.(http.Flusher); !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	ch := s.fleet.Subscribe(r.Context())

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	// Each write gets its own deadline, so a client that stops reading while its
	// connection stays open cannot pin this goroutine (and Shutdown) forever.
	// A server-wide WriteTimeout would instead kill every healthy stream.
	rc := http.NewResponseController(w)
	send := func(format string, args ...any) bool {
		_ = rc.SetWriteDeadline(time.Now().Add(sseWriteTimeout))
		if _, err := fmt.Fprintf(w, format, args...); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	if !send(": connected\n\n") {
		return
	}

	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			b, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			if !send("event: %s\ndata: %s\n\n", ev.Type, b) {
				return
			}
		}
	}
}

// sseWriteTimeout bounds one SSE write to a client.
const sseWriteTimeout = 10 * time.Second
