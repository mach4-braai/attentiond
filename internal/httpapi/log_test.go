package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/devanmcgeer/attentiond/internal/attention"
)

func signalServer(t *testing.T, clock *time.Time) (http.Handler, *attention.Store) {
	t.Helper()
	return newQueueServer(t, attention.StoreConfig{
		SignalLogPath: filepath.Join(t.TempDir(), "signals.jsonl"),
	}, clock)
}

func decodeLog(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return out
}

func logEntries(t *testing.T, handler http.Handler, query string) []attention.SignalEntry {
	t.Helper()
	recorder := do(t, handler, http.MethodGet, "/api/log"+query, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /api/log%s = %d: %s", query, recorder.Code, recorder.Body)
	}
	var response logResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Count != len(response.Entries) {
		t.Fatalf("count = %d for %d entries", response.Count, len(response.Entries))
	}
	return response.Entries
}

func TestLogServesNewestFirstInTheContractShape(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	handler, store := signalServer(t, &now)

	pull := review("example-org/infra#1")
	store.ReplaceSource("github", []attention.Item{pull})
	now = now.Add(time.Minute)
	pull.Label = "ready to merge"
	store.ReplaceSource("github", []attention.Item{pull})
	now = now.Add(time.Minute)
	store.ReplaceSource("github", nil)

	recorder := do(t, handler, http.MethodGet, "/api/log", "")
	if recorder.Code != http.StatusOK || !strings.HasPrefix(recorder.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("status %d, content type %q", recorder.Code, recorder.Header().Get("Content-Type"))
	}
	body := decodeLog(t, recorder.Body.Bytes())
	if _, err := time.Parse(time.RFC3339, body["generated_at"].(string)); err != nil {
		t.Errorf("generated_at: %v", err)
	}
	if body["count"].(float64) != 3 {
		t.Errorf("count = %v", body["count"])
	}

	entries := body["entries"].([]any)
	last := entries[2].(map[string]any)
	for _, key := range []string{"at", "kind", "item_id", "source", "title", "state", "label", "severity"} {
		if _, ok := last[key]; !ok {
			t.Errorf("appeared entry lacks %q: %v", key, last)
		}
	}
	for _, key := range []string{"prev_state", "prev_label"} {
		if _, ok := last[key]; ok {
			t.Errorf("appeared entry carries %q: %v", key, last)
		}
	}
	changed := entries[1].(map[string]any)
	if changed["kind"] != "changed" || changed["label"] != "ready to merge" ||
		changed["prev_label"] != "review requested" || changed["prev_state"] != "needs_attention" {
		t.Errorf("changed entry = %v", changed)
	}
	if entries[0].(map[string]any)["kind"] != "disappeared" {
		t.Errorf("newest entry = %v", entries[0])
	}
}

func TestLogSinceAndLimit(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	handler, store := signalServer(t, &now)
	start := now
	for i := range 5 {
		store.Put(review(fmt.Sprintf("example-org/infra#%d", i)))
		now = now.Add(time.Minute)
	}

	since := url.QueryEscape(start.Add(3 * time.Minute).Format(time.RFC3339))
	got := logEntries(t, handler, "?since="+since)
	if len(got) != 2 || got[0].ItemID != "github:example-org/infra#4" || got[1].ItemID != "github:example-org/infra#3" {
		t.Fatalf("since = %+v", got)
	}

	got = logEntries(t, handler, "?limit=2")
	if len(got) != 2 || got[0].ItemID != "github:example-org/infra#4" {
		t.Fatalf("limit = %+v", got)
	}

	if got := logEntries(t, handler, ""); len(got) != 5 {
		t.Fatalf("default returned %d entries, want all 5", len(got))
	}
}

func TestLogCapsLimitAndDefaultsToTwoHundred(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	handler, store := signalServer(t, &now)
	for i := range 1005 {
		store.Put(review(fmt.Sprintf("example-org/infra#%d", i)))
	}

	if got := logEntries(t, handler, ""); len(got) != 200 {
		t.Errorf("default limit returned %d, want 200", len(got))
	}
	if got := logEntries(t, handler, "?limit=5000"); len(got) != 1000 {
		t.Errorf("limit=5000 returned %d, want the cap of 1000", len(got))
	}
}

func TestLogRejectsBadInput(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	handler, _ := signalServer(t, &now)

	for _, query := range []string{
		"?since=yesterday",
		"?since=2026-09-12",
		"?limit=0",
		"?limit=-3",
		"?limit=many",
		"?limit=1.5",
	} {
		recorder := do(t, handler, http.MethodGet, "/api/log"+query, "")
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", query, recorder.Code)
			continue
		}
		var body map[string]string
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil || body["error"] == "" {
			t.Errorf("%s: body %s is not {\"error\":...}", query, recorder.Body)
		}
	}
}

func TestLogIsEmptyWhenNothingHappened(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	handler, _ := signalServer(t, &now)

	recorder := do(t, handler, http.MethodGet, "/api/log", "")
	if !strings.Contains(recorder.Body.String(), `"entries":[]`) {
		t.Fatalf("body = %s, want an empty entries array rather than null", recorder.Body)
	}
}
