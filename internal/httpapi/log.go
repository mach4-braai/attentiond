package httpapi

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/devanmcgeer/attentiond/internal/attention"
)

// Page sizes for GET /api/log.
const (
	defaultLogLimit = 200
	maxLogLimit     = 1000
)

type logResponse struct {
	GeneratedAt time.Time               `json:"generated_at"`
	Count       int                     `json:"count"`
	Entries     []attention.SignalEntry `json:"entries"`
}

// signalLog serves the signal log newest first. since is an RFC 3339 time and
// is inclusive; limit defaults to 200 and is capped at 1000.
func (s *server) signalLog(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	var since time.Time
	if raw := strings.TrimSpace(query.Get("since")); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("since=%q: want an RFC 3339 time", raw))
			return
		}
		since = parsed
	}

	limit := defaultLogLimit
	if raw := strings.TrimSpace(query.Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("limit=%q: want a positive whole number", raw))
			return
		}
		limit = min(parsed, maxLogLimit)
	}

	entries, err := s.cfg.Store.Signals(since, limit)
	if err != nil {
		s.cfg.Log.Warn("signal log not read", "error", err)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, logResponse{
		GeneratedAt: time.Now().UTC(),
		Count:       len(entries),
		Entries:     entries,
	})
}
