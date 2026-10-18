package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/devanmcgeer/attentiond/internal/attention"
	"github.com/devanmcgeer/attentiond/internal/github"
)

// Decisions a human can make about one item, as the last path segment of
// POST /api/items/{key}/{decision}.
const (
	decisionSnooze  = "snooze"
	decisionBump    = "bump"
	decisionClear   = "clear"
	decisionWatch   = "watch"
	decisionUnwatch = "unwatch"
)

// agentHeader must accompany watch and unwatch. A custom header makes a
// browser send a CORS preflight, which attentiond never answers, so another
// website cannot start agents through the browser of the human using it.
const agentHeader = "X-Attentiond"

// decide records a snooze, a bump, a watch, or the removal of one.
//
// The key is the whole item id, percent-encoded: a GitHub item is
// "github:didx-xyz/tofu#42", which contains both a slash and a fragment
// marker. Consumers post the href from the item rather than building it.
func (s *server) decide(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	decision := r.PathValue("decision")

	if (decision == decisionWatch || decision == decisionUnwatch) && r.Header.Get(agentHeader) != "1" {
		writeError(w, http.StatusForbidden, decision+" needs the header "+agentHeader+": 1")
		return
	}

	var (
		item attention.Item
		err  error
	)
	switch decision {
	case decisionSnooze:
		var until time.Time
		until, err = s.snoozeUntil(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		item, err = s.cfg.Store.Decide(key, attention.DecisionSnooze, until)
	case decisionBump:
		item, err = s.cfg.Store.Decide(key, attention.DecisionBump, time.Time{})
	case decisionClear:
		item, err = s.cfg.Store.Clear(key)
	case decisionWatch:
		current, ok := s.cfg.Store.Get(key)
		if ok && !watchable(current) {
			writeError(w, http.StatusBadRequest, "only a pull request you authored can be watched")
			return
		}
		item, err = s.cfg.Store.Watch(key)
	case decisionUnwatch:
		item, err = s.cfg.Store.Unwatch(key)
	default:
		writeError(w, http.StatusBadRequest, fmt.Sprintf(
			"unknown decision %q: want %s, %s, %s, %s or %s",
			decision, decisionSnooze, decisionBump, decisionClear, decisionWatch, decisionUnwatch))
		return
	}

	if err != nil {
		if errors.Is(err, attention.ErrItemMissing) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, s.decorate(item))
}

// watchable reports whether attentiond may run tools on an item: a GitHub pull
// request you authored. Anybody else's branch is not yours to push to.
func watchable(item attention.Item) bool {
	return item.Source == github.SourceName && item.Context["role"] == "author"
}

// snoozeUntil reads the optional ?for= override. "0" is the open-ended snooze:
// hide this until something actually happens to it.
func (s *server) snoozeUntil(r *http.Request) (time.Time, error) {
	length := s.cfg.SnoozeFor
	if raw := strings.TrimSpace(r.URL.Query().Get("for")); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			return time.Time{}, fmt.Errorf("for=%q: %w", raw, err)
		}
		if parsed < 0 {
			return time.Time{}, fmt.Errorf("for=%q: want zero or a period", raw)
		}
		length = parsed
	}
	if length == 0 {
		return time.Time{}, nil
	}
	return time.Now().Add(length), nil
}

// decorate appends the queue controls to an item's own actions.
//
// They are added here rather than by each adapter because they are not the
// adapter's business: snoozing is something the daemon does to its own queue,
// and an adapter that had to build these hrefs would be the second place that
// knows the route grammar.
func (s *server) decorate(item attention.Item) attention.Item {
	base := strings.TrimSuffix(s.cfg.PublicURL, "/") + "/api/items/" + url.PathEscape(item.ID)

	// A fresh slice: the stored item's Actions belongs to the store, and
	// appending to it would write into state a poll is still reading.
	actions := make([]attention.Action, 0, len(item.Actions)+4)
	actions = append(actions, item.Actions...)

	if item.Snoozed {
		actions = append(actions, attention.Action{
			ID: "wake", Label: "Wake", Method: "POST", Href: base + "/" + decisionClear,
		})
	} else {
		actions = append(actions, attention.Action{
			ID:     "snooze",
			Label:  "Snooze " + snoozeWord(s.cfg.SnoozeFor),
			Method: "POST",
			Href:   base + "/" + decisionSnooze,
		})
	}

	if item.Bumped {
		actions = append(actions, attention.Action{
			ID: "unbump", Label: "Unbump", Method: "POST", Href: base + "/" + decisionClear,
		})
	} else {
		actions = append(actions, attention.Action{
			ID: "bump", Label: "Bump", Method: "POST", Href: base + "/" + decisionBump,
		})
	}

	// Unwatch stays on offer even if the item stopped qualifying, so a watch
	// can always be taken back.
	switch {
	case item.Watched:
		actions = append(actions, attention.Action{
			ID: "unwatch", Label: "Unwatch", Method: "POST", Href: base + "/" + decisionUnwatch,
		})
	case watchable(item):
		actions = append(actions, attention.Action{
			ID: "watch", Label: "Watch", Method: "POST", Href: base + "/" + decisionWatch,
		})
	}

	if item.Job != nil && item.Job.Status == attention.JobFinished {
		actions = append(actions, attention.Action{
			ID: "clear", Label: "Dismiss", Method: "POST", Href: base + "/" + decisionClear,
		})
	}

	item.Actions = actions
	return item
}

// snoozeWord is the configured snooze length as it reads on a button. A button
// that says what it does needs "4h", not "4h0m0s", and an open-ended snooze
// has no number to show at all.
func snoozeWord(length time.Duration) string {
	switch {
	case length <= 0:
		return "until it changes"
	case length%(24*time.Hour) == 0:
		return fmt.Sprintf("%dd", length/(24*time.Hour))
	case length%time.Hour == 0:
		return fmt.Sprintf("%dh", length/time.Hour)
	case length%time.Minute == 0:
		return fmt.Sprintf("%dm", length/time.Minute)
	default:
		return length.String()
	}
}
