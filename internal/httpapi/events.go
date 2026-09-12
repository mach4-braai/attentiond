package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/devanmcgeer/attentiond/internal/attention"
)

// eventRequest is the body of POST /api/events. It is the contract local
// processes such as builds, tests, tofu runs and agent hooks report against.
type eventRequest struct {
	Source    string            `json:"source"`
	ID        string            `json:"id"`
	Event     string            `json:"event"`
	Title     string            `json:"title"`
	Severity  string            `json:"severity"`
	Context   map[string]string `json:"context"`
	URL       string            `json:"url"`
	Timestamp time.Time         `json:"timestamp"`
}

// eventStates maps lifecycle verbs onto the core model. The verbs are what a
// producer knows ("I finished"); the states are what a consumer needs.
var eventStates = map[string]attention.State{
	"started":         attention.StateWorking,
	"working":         attention.StateWorking,
	"waiting":         attention.StateWaiting,
	"needs_attention": attention.StateNeedsAttention,
	"completed":       attention.StateDone,
	"failed":          attention.StateFailed,
}

func defaultSeverity(state attention.State) attention.Severity {
	switch state {
	case attention.StateFailed:
		return attention.SeverityCritical
	case attention.StateNeedsAttention:
		return attention.SeverityWarning
	default:
		return attention.SeverityInfo
	}
}

func (s *server) ingestEvent(w http.ResponseWriter, r *http.Request) {
	var request eventRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid event body: "+err.Error())
		return
	}

	if _, reserved := s.cfg.Sources[request.Source]; reserved {
		writeError(w, http.StatusConflict,
			fmt.Sprintf("source %q is owned by an adapter and rebuilt on every poll", request.Source))
		return
	}

	item, err := s.buildItem(request)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, s.cfg.Store.Put(item))
}

func (s *server) buildItem(request eventRequest) (attention.Item, error) {
	source := strings.TrimSpace(request.Source)
	id := strings.TrimSpace(request.ID)
	if source == "" || id == "" {
		return attention.Item{}, errors.New("source and id are required and form the item identity")
	}

	state, ok := eventStates[request.Event]
	if !ok {
		return attention.Item{}, fmt.Errorf("unknown event %q, expected one of %s",
			request.Event, strings.Join(eventVerbs(), ", "))
	}

	severity := defaultSeverity(state)
	if request.Severity != "" {
		parsed, err := attention.ParseSeverity(request.Severity)
		if err != nil {
			return attention.Item{}, err
		}
		severity = parsed
	}

	title := strings.TrimSpace(request.Title)
	if title == "" {
		title = source + " " + id
	}

	meta := map[string]string{"event": request.Event}
	for key, value := range request.Context {
		meta[key] = value
	}

	var actions []attention.Action
	if request.URL != "" {
		actions = append(actions, attention.Action{
			ID:     "open",
			Label:  "Open",
			Method: "GET",
			Href:   request.URL,
		})
	}

	return attention.Item{
		ID:        attention.Key(source, id),
		Source:    source,
		Title:     title,
		State:     state,
		Severity:  severity,
		Context:   meta,
		UpdatedAt: request.Timestamp,
		Actions:   actions,
	}, nil
}

func eventVerbs() []string {
	verbs := make([]string, 0, len(eventStates))
	for verb := range eventStates {
		verbs = append(verbs, verb)
	}
	sort.Strings(verbs)
	return verbs
}
