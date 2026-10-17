package dashboard

import "net/http"

// getMetrics returns a host metrics sample no older than two seconds and marks
// the sampler wanted, so the event stream keeps flowing for the next ten.
// Read-only, so it needs no same-origin guard.
func (s *server) getMetrics(w http.ResponseWriter, r *http.Request) {
	if s.metrics == nil {
		writeError(w, http.StatusNotFound, "host metrics are not enabled")
		return
	}
	writeJSON(w, http.StatusOK, s.metrics.Current(r.Context()))
}
