package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

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
	fl, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	ch := s.fleet.Subscribe(r.Context())

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, ": connected\n\n")
	fl.Flush()

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
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, b)
			fl.Flush()
		}
	}
}
