// Package httpapi serves the normalized attention state over localhost
// HTTP/JSON and accepts lifecycle events and action invocations.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/devanmcgeer/attentiond/internal/attention"
)

// Executor runs one action for a source. attentiond owns the route grammar;
// the adapter owns what the three segments mean.
type Executor interface {
	Execute(ctx context.Context, kind, target, action string) error
}

// Config wires the server to the store and the registered adapters.
type Config struct {
	Store *attention.Store
	// Sources reports adapter health and reserves source names against event
	// ingestion, so an event cannot collide with adapter-owned items.
	Sources map[string]func() attention.SourceStatus
	Actions map[string]Executor
	// PublicURL is the address a browser reaches this daemon on. The snooze
	// and bump buttons are absolute hrefs built from it, the same way each
	// adapter builds its own.
	PublicURL string
	// SnoozeFor is how long the offered snooze lasts. Zero offers the
	// open-ended one, which lapses when the item's label changes.
	SnoozeFor time.Duration
	Version   string
	Started   time.Time
	Log       *slog.Logger
}

type server struct {
	cfg Config
}

// New returns the HTTP handler for the daemon.
func New(cfg Config) http.Handler {
	s := &server{cfg: cfg}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("GET /api/work", s.work)
	mux.HandleFunc("GET /api/attention", s.attention)
	mux.HandleFunc("GET /api/stale", s.stale)
	mux.HandleFunc("POST /api/events", s.ingestEvent)
	mux.HandleFunc("POST /api/actions/{source}/{kind}/{target}/{action}", s.runAction)
	mux.HandleFunc("POST /api/items/{key}/{decision}", s.decide)
	return s.logRequests(mux)
}

type listResponse struct {
	GeneratedAt    time.Time `json:"generated_at"`
	Count          int       `json:"count"`
	AttentionCount int       `json:"attention_count"`
	// Warnings carry adapter caveats to the consumer. A list that is known to
	// be incomplete has to say so where it is read, not only in /health, which
	// nobody has open.
	Warnings []string         `json:"warnings,omitempty"`
	Items    []attention.Item `json:"items"`
	// StaleCount says how many items /api/work left out. A list that is not
	// everything has to say so where it is read: this is the same promise
	// Warnings makes about a capped GitHub search.
	StaleCount int `json:"stale_count,omitempty"`
}

// warnings collects the degraded-but-working notices from every adapter.
func (s *server) warnings() []string {
	var warnings []string
	for name, status := range s.cfg.Sources {
		if warning := status().Warning; warning != "" {
			warnings = append(warnings, name+": "+warning)
		}
	}
	sort.Strings(warnings)
	return warnings
}

func (s *server) work(w http.ResponseWriter, r *http.Request) {
	// Stale items are held back rather than filtered out of the store: the
	// board is what is live, /api/stale is what has stopped moving, and the
	// count below is the link between them.
	all := s.cfg.Store.Items()
	items := make([]attention.Item, 0, len(all))
	staleCount := 0
	for _, item := range all {
		if item.Stale {
			staleCount++
			continue
		}
		items = append(items, s.decorate(item))
	}
	writeJSON(w, http.StatusOK, listResponse{
		GeneratedAt:    time.Now().UTC(),
		Count:          len(items),
		AttentionCount: s.cfg.Store.AttentionCount(),
		StaleCount:     staleCount,
		Warnings:       s.warnings(),
		Items:          items,
	})
}

func (s *server) attention(w http.ResponseWriter, r *http.Request) {
	items := s.cfg.Store.Attention()
	for i := range items {
		items[i] = s.decorate(items[i])
	}
	writeJSON(w, http.StatusOK, listResponse{
		GeneratedAt:    time.Now().UTC(),
		Count:          len(items),
		AttentionCount: len(items),
		Warnings:       s.warnings(),
		Items:          items,
	})
}

// stale serves the work nothing has happened to for longer than
// [attention] stale_after: the list you read on purpose, rather than the one
// that reads you.
func (s *server) stale(w http.ResponseWriter, r *http.Request) {
	all := s.cfg.Store.Items()
	items := make([]attention.Item, 0)
	for _, item := range all {
		if item.Stale {
			items = append(items, s.decorate(item))
		}
	}
	writeJSON(w, http.StatusOK, listResponse{
		GeneratedAt: time.Now().UTC(),
		Count:       len(items),
		StaleCount:  len(items),
		Warnings:    s.warnings(),
		Items:       items,
	})
}

type healthResponse struct {
	Status        string `json:"status"`
	Version       string `json:"version"`
	UptimeSeconds int64  `json:"uptime_seconds"`
	Items         int    `json:"items"`
	// Decisions is how many items carry a snooze or a bump. An empty queue
	// with a number here has an explanation; an empty queue without one is a
	// source that has stopped reporting.
	Decisions int                               `json:"decisions"`
	Sources   map[string]attention.SourceStatus `json:"sources"`
}

func (s *server) health(w http.ResponseWriter, r *http.Request) {
	counts := s.cfg.Store.CountBySource()
	total := 0
	for _, count := range counts {
		total += count
	}

	sources := make(map[string]attention.SourceStatus, len(s.cfg.Sources))
	for name, status := range s.cfg.Sources {
		sources[name] = status()
	}

	// The daemon is healthy even when an adapter is not: Herdr being down is a
	// normal state, and a red /health would train the operator to ignore it.
	writeJSON(w, http.StatusOK, healthResponse{
		Status:        "ok",
		Version:       s.cfg.Version,
		UptimeSeconds: int64(time.Since(s.cfg.Started).Seconds()),
		Items:         total,
		Decisions:     len(s.cfg.Store.Decisions()),
		Sources:       sources,
	})
}

func (s *server) runAction(w http.ResponseWriter, r *http.Request) {
	source := r.PathValue("source")
	kind := r.PathValue("kind")
	target := r.PathValue("target")
	action := r.PathValue("action")

	executor, ok := s.cfg.Actions[source]
	if !ok {
		writeError(w, http.StatusNotFound, "no actions registered for source "+source)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	err := executor.Execute(ctx, kind, target, action)
	if err != nil {
		s.cfg.Log.Warn("action failed",
			"source", source, "kind", kind, "target", target, "action", action, "error", err)
		writeError(w, actionStatus(err), err.Error())
		return
	}

	s.cfg.Log.Info("action executed",
		"source", source, "kind", kind, "target", target, "action", action)
	writeJSON(w, http.StatusOK, map[string]string{
		"status": "ok",
		"source": source,
		"kind":   kind,
		"target": target,
		"action": action,
	})
}

// actionStatus maps the shared action failures onto HTTP.
func actionStatus(err error) int {
	switch {
	case errors.Is(err, attention.ErrActionTargetMissing):
		return http.StatusNotFound
	case errors.Is(err, attention.ErrActionUnsupported):
		return http.StatusBadRequest
	default:
		return http.StatusServiceUnavailable
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (s *server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(recorder, r)

		// The handler that produced a 5xx already logged why, so this line
		// only needs to be findable, not loud.
		level := slog.LevelDebug
		if recorder.status >= http.StatusInternalServerError {
			level = slog.LevelWarn
		}
		s.cfg.Log.Log(r.Context(), level, "http request",
			"method", r.Method, "path", r.URL.Path,
			"status", recorder.status, "duration_ms", time.Since(started).Milliseconds())
	})
}
