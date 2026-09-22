package httpapi

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/devanmcgeer/attentiond/internal/attention"
)

// newQueueServer builds a server over a store with a clock the test owns, for
// the parts of the queue that only move because time passes.
func newQueueServer(t *testing.T, storeConfig attention.StoreConfig, clock *time.Time) (http.Handler, *attention.Store) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	storeConfig.Now = func() time.Time { return *clock }
	store := attention.NewStore(log, storeConfig)

	handler := New(Config{
		Store:     store,
		Sources:   map[string]func() attention.SourceStatus{},
		Actions:   map[string]Executor{},
		PublicURL: "http://127.0.0.1:7717",
		SnoozeFor: 4 * time.Hour,
		Version:   "test",
		Started:   time.Now(),
		Log:       log,
	})
	return handler, store
}

func review(id string) attention.Item {
	return attention.Item{
		ID:       attention.Key("github", id),
		Source:   "github",
		Title:    id,
		State:    attention.StateNeedsAttention,
		Severity: attention.SeverityWarning,
		Label:    "review requested",
		Tone:     attention.ToneAttention,
		Priority: attention.PriorityAsked,
		Actions: []attention.Action{{
			ID: "open", Label: "Open", Method: "GET", Href: "https://github.com/" + id,
		}},
	}
}

func action(t *testing.T, item attention.Item, id string) attention.Action {
	t.Helper()
	for _, candidate := range item.Actions {
		if candidate.ID == id {
			return candidate
		}
	}
	t.Fatalf("no %q action on %s: %+v", id, item.ID, item.Actions)
	return attention.Action{}
}

// The dashboard posts the href the daemon gave it. An id with a slash and a
// fragment marker in it (every GitHub item has both) has to survive that round
// trip, or the button 404s on exactly the items worth snoozing.
func TestSnoozeThroughTheHrefTheItemCarries(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	handler, store := newQueueServer(t, attention.StoreConfig{}, &now)
	store.ReplaceSource("github", []attention.Item{review("didx-xyz/tofu#42")})

	queue := decodeList(t, do(t, handler, http.MethodGet, "/api/attention", ""))
	snooze := action(t, queue.Items[0], "snooze")
	if snooze.Label != "Snooze 4h" {
		t.Errorf("button says %q, which is not the configured snooze", snooze.Label)
	}

	path := strings.TrimPrefix(snooze.Href, "http://127.0.0.1:7717")
	if recorder := do(t, handler, http.MethodPost, path, ""); recorder.Code != http.StatusOK {
		t.Fatalf("snooze %s: %d %s", path, recorder.Code, recorder.Body)
	}

	if queue := decodeList(t, do(t, handler, http.MethodGet, "/api/attention", "")); len(queue.Items) != 0 {
		t.Fatalf("snoozed item still in the queue: %+v", queue.Items)
	}

	board := decodeList(t, do(t, handler, http.MethodGet, "/api/work", ""))
	if len(board.Items) != 1 || !board.Items[0].Snoozed {
		t.Fatalf("the board lost the snoozed item: %+v", board.Items)
	}
	if board.AttentionCount != 0 {
		t.Errorf("attention_count = %d, want 0: a snoozed item is not waiting on anybody", board.AttentionCount)
	}

	// The same item now offers the way back.
	wake := action(t, board.Items[0], "wake")
	if recorder := do(t, handler, http.MethodPost, strings.TrimPrefix(wake.Href, "http://127.0.0.1:7717"), ""); recorder.Code != http.StatusOK {
		t.Fatalf("wake: %d %s", recorder.Code, recorder.Body)
	}
	if queue := decodeList(t, do(t, handler, http.MethodGet, "/api/attention", "")); len(queue.Items) != 1 {
		t.Fatal("waking did not put the item back in the queue")
	}
}

func TestSnoozeLengthCanBeOverriddenPerRequest(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	handler, store := newQueueServer(t, attention.StoreConfig{}, &now)
	store.ReplaceSource("github", []attention.Item{review("didx-xyz/tofu#42")})

	path := "/api/items/github:didx-xyz%2Ftofu%2342/snooze?for=30m"
	if recorder := do(t, handler, http.MethodPost, path, ""); recorder.Code != http.StatusOK {
		t.Fatalf("snooze: %d %s", recorder.Code, recorder.Body)
	}

	board := decodeList(t, do(t, handler, http.MethodGet, "/api/work", ""))
	until := board.Items[0].SnoozedUntil
	if until == nil || until.Sub(time.Now()) > time.Hour {
		t.Fatalf("for=30m did not shorten the snooze: %v", until)
	}
}

func TestDecisionRejectsWhatItCannotDo(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	handler, store := newQueueServer(t, attention.StoreConfig{}, &now)
	store.ReplaceSource("github", []attention.Item{review("didx-xyz/tofu#42")})

	cases := map[string]struct {
		path string
		want int
	}{
		"unknown decision": {"/api/items/github:didx-xyz%2Ftofu%2342/postpone", http.StatusBadRequest},
		"unusable period":  {"/api/items/github:didx-xyz%2Ftofu%2342/snooze?for=soon", http.StatusBadRequest},
		"negative period":  {"/api/items/github:didx-xyz%2Ftofu%2342/snooze?for=-2h", http.StatusBadRequest},
		"item not held":    {"/api/items/github:didx-xyz%2Ftofu%23999/snooze", http.StatusNotFound},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if recorder := do(t, handler, http.MethodPost, tc.path, ""); recorder.Code != tc.want {
				t.Fatalf("got %d want %d: %s", recorder.Code, tc.want, recorder.Body)
			}
		})
	}
}

func TestStaleWorkGetsItsOwnListAndIsCountedOnTheBoard(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	handler, store := newQueueServer(t, attention.StoreConfig{StaleAfter: 30 * 24 * time.Hour}, &now)

	old := review("didx-xyz/mono#7")
	old.UpdatedAt = now.Add(-40 * 24 * time.Hour)
	fresh := review("didx-xyz/tofu#42")
	fresh.UpdatedAt = now.Add(-time.Hour)
	store.ReplaceSource("github", []attention.Item{old, fresh})

	board := decodeList(t, do(t, handler, http.MethodGet, "/api/work", ""))
	if len(board.Items) != 1 || board.Items[0].ID != "github:didx-xyz/tofu#42" {
		t.Fatalf("the board still carries month-old work: %+v", board.Items)
	}
	if board.StaleCount != 1 {
		t.Errorf("stale_count = %d: a list that is not everything has to say so", board.StaleCount)
	}

	stale := decodeList(t, do(t, handler, http.MethodGet, "/api/stale", ""))
	if len(stale.Items) != 1 || stale.Items[0].ID != "github:didx-xyz/mono#7" || !stale.Items[0].Stale {
		t.Fatalf("/api/stale did not hold the old review: %+v", stale.Items)
	}
	if action(t, stale.Items[0], "bump").Method != "POST" {
		t.Error("a stale item cannot be brought back")
	}
}

func TestTheBoardsAttentionCountMatchesTheQueue(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	handler, store := newQueueServer(t, attention.StoreConfig{DoneTTL: 10 * time.Minute}, &now)

	finished := review("didx-xyz/mono#7")
	finished.State = attention.StateDone
	finished.Label = "done"
	store.ReplaceSource("github", []attention.Item{finished, review("didx-xyz/tofu#42")})

	// Past done_ttl the finished review leaves the queue. The board keeps
	// showing it, and has to stop counting it.
	now = now.Add(11 * time.Minute)

	queue := decodeList(t, do(t, handler, http.MethodGet, "/api/attention", ""))
	board := decodeList(t, do(t, handler, http.MethodGet, "/api/work", ""))
	if len(board.Items) != 2 {
		t.Fatalf("the board dropped the finished review: %+v", board.Items)
	}
	if board.AttentionCount != len(queue.Items) {
		t.Fatalf("the board says %d want you, /api/attention lists %d",
			board.AttentionCount, len(queue.Items))
	}
}

func TestHealthCountsStandingDecisions(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	handler, store := newQueueServer(t, attention.StoreConfig{}, &now)
	store.ReplaceSource("github", []attention.Item{review("didx-xyz/tofu#42")})
	do(t, handler, http.MethodPost, "/api/items/github:didx-xyz%2Ftofu%2342/bump", "")

	var health healthResponse
	recorder := do(t, handler, http.MethodGet, "/health", "")
	if err := json.Unmarshal(recorder.Body.Bytes(), &health); err != nil {
		t.Fatal(err)
	}
	if health.Decisions != 1 {
		t.Fatalf("decisions = %d, want 1: an explained queue explains itself", health.Decisions)
	}
}
