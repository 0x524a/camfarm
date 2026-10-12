package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/0x524a/camfarm"
	"github.com/0x524a/camfarm/internal/quality"
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

// events streams fleet changes as Server-Sent Events, merged with "quality"
// events from the stream quality runner when one is configured. A client that falls
// behind is dropped by the fleet and sees the stream end; it reconnects and
// re-lists, which is why nothing is replayed.
func (s *server) events(w http.ResponseWriter, r *http.Request) {
	if _, ok := w.(http.Flusher); !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	ch := s.fleet.Subscribe(r.Context())
	// qch is nil (and so never ready) when stream quality tests are disabled.
	// Quality events come from the runner's own bus, not the fleet's feed: a
	// measurement is not a fleet change, so it neither enters the journal nor is
	// published to the fleet's feed (it is still an RTSP client and counts toward
	// the camera's request index, like Camera.Screenshot).
	var qch <-chan quality.Event
	if s.quality != nil {
		qch = s.quality.Subscribe(r.Context())
	}

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
		case qe, ok := <-qch:
			if !ok {
				return // dropped or closed: the client reconnects and re-lists
			}
			b, err := json.Marshal(qe)
			if err != nil {
				continue
			}
			if !send("event: quality\ndata: %s\n\n", b) {
				return
			}
		}
	}
}

// sseWriteTimeout bounds one SSE write to a client.
const sseWriteTimeout = 10 * time.Second
