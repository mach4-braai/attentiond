package outline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/devanmcgeer/attentiond/internal/attention"
)

const secret = "ol_api_s3cret"

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type logBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// notification builds the wire shape of the published schema: a documentId
// and no embedded document.
func notification(id, event string, viewed, archived bool) map[string]any {
	n := map[string]any{
		"id":         id,
		"event":      event,
		"actor":      map[string]any{"name": "alice"},
		"documentId": "doc-" + id,
		"createdAt":  "2026-10-20T09:15:00Z",
		"viewedAt":   nil,
		"archivedAt": nil,
	}
	if viewed {
		n["viewedAt"] = "2026-10-20T10:00:00Z"
	}
	if archived {
		n["archivedAt"] = "2026-10-20T10:00:00Z"
	}
	return n
}

// outlineServer serves notifications.list pages by limit and offset and
// documents.info by id.
type outlineServer struct {
	*httptest.Server
	mu       sync.Mutex
	notes    []map[string]any
	listCall int
	docCalls int
	auth     string
}

func newOutlineServer(t *testing.T, notes []map[string]any) *outlineServer {
	t.Helper()
	s := &outlineServer{notes: notes}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/notifications.list", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Limit, Offset int }
		_ = json.NewDecoder(r.Body).Decode(&req)
		s.mu.Lock()
		s.listCall++
		s.auth = r.Header.Get("Authorization")
		notes := s.notes
		s.mu.Unlock()

		end := min(req.Offset+req.Limit, len(notes))
		start := min(req.Offset, end)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": notes[start:end]})
	})
	mux.HandleFunc("/api/documents.info", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ ID string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		s.mu.Lock()
		s.docCalls++
		s.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": map[string]any{
			"id": req.ID, "title": "Acme handbook", "url": "/doc/acme-handbook-AbC123",
		}})
	})
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func titles(items []attention.Item) string {
	var out []string
	for _, item := range items {
		out = append(out, item.ID)
	}
	return strings.Join(out, ",")
}

func TestUnreadMentionBecomesAnItemWithAbsoluteURL(t *testing.T) {
	comment := notification("n2", EventCommentMentioned, false, false)
	comment["commentId"] = "c9"
	server := newOutlineServer(t, []map[string]any{
		notification("n1", EventDocumentMentioned, false, false),
		comment,
	})

	notes, err := NewClient(server.URL, secret, 5*time.Second).Mentions(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	items := Normalize(notes, server.URL, 100, time.Now())
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2: %s", len(items), titles(items))
	}

	doc := items[0]
	if doc.ID != "outline:n1" || doc.Source != "outline" || doc.Label != "mentioned you" ||
		doc.Title != "alice mentioned you in Acme handbook" || doc.State != attention.StateNeedsAttention {
		t.Errorf("document mention = %+v", doc)
	}
	wantURL := server.URL + "/doc/acme-handbook-AbC123"
	if doc.Context["url"] != wantURL || doc.Context["actor"] != "alice" ||
		doc.Context["document"] != "Acme handbook" || doc.Context["event"] != EventDocumentMentioned {
		t.Errorf("document context = %v", doc.Context)
	}
	if got := items[1].Context["url"]; got != wantURL+"?commentId=c9" {
		t.Errorf("comment mention url = %q, want the comment anchor", got)
	}
	if server.auth != "Bearer "+secret {
		t.Errorf("Authorization = %q", server.auth)
	}
}

func TestReadArchivedAndNonMentionNotificationsAreFiltered(t *testing.T) {
	server := newOutlineServer(t, []map[string]any{
		notification("unread", EventDocumentMentioned, false, false),
		notification("read", EventDocumentMentioned, true, false),
		notification("archived", EventCommentMentioned, false, true),
		notification("reply", "comments.create", false, false),
		notification("shared", "documents.publish", false, false),
	})

	notes, err := NewClient(server.URL, secret, 5*time.Second).Mentions(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	items := Normalize(notes, server.URL, 100, time.Now())
	if got := titles(items); got != "outline:unread" {
		t.Errorf("items = %s, want only the unread mention", got)
	}
	if server.docCalls != 1 {
		t.Errorf("documents.info called %d times, want 1: filtered notifications need no lookup", server.docCalls)
	}
}

func TestPaginationStopsAtMaxItems(t *testing.T) {
	var notes []map[string]any
	for i := range 250 {
		notes = append(notes, notification(fmt.Sprintf("n%03d", i), EventDocumentMentioned, false, false))
	}
	server := newOutlineServer(t, notes)

	got, err := NewClient(server.URL, secret, 5*time.Second).Mentions(context.Background(), 150)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 150 {
		t.Errorf("got %d mentions, want 150", len(got))
	}
	if server.listCall != 2 {
		t.Errorf("notifications.list called %d times, want 2 (no page past the cap)", server.listCall)
	}
}

func TestPaginationSkipsNonMentionsAcrossPages(t *testing.T) {
	var notes []map[string]any
	for i := range 100 {
		notes = append(notes, notification(fmt.Sprintf("noise%03d", i), "comments.create", false, false))
	}
	notes = append(notes, notification("late", EventDocumentMentioned, false, false))
	server := newOutlineServer(t, notes)

	got, err := NewClient(server.URL, secret, 5*time.Second).Mentions(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "late" {
		t.Errorf("got %+v, want the mention on the second page", got)
	}
}

func TestFailedPollKeepsLastGoodItemsAndNeverLogsTheKey(t *testing.T) {
	var mu sync.Mutex
	fail := false
	server := newOutlineServer(t, nil)
	good := server.Config.Handler
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		failing := fail
		mu.Unlock()
		if failing {
			// A misbehaving proxy that echoes the credential back.
			http.Error(w, "bad gateway for "+r.Header.Get("Authorization"), http.StatusBadGateway)
			return
		}
		good.ServeHTTP(w, r)
	})
	server.mu.Lock()
	server.notes = []map[string]any{notification("n1", EventDocumentMentioned, false, false)}
	server.mu.Unlock()

	logs := &logBuffer{}
	store := attention.NewStore(discardLogger(), attention.StoreConfig{})
	poller := NewPoller(NewClient(server.URL, secret, 5*time.Second), store,
		PollerConfig{Interval: time.Hour, BaseURL: server.URL, MaxItems: 100},
		slog.New(slog.NewTextHandler(logs, nil)))

	poller.pollOnce(context.Background())
	if got := store.Items(); len(got) != 1 || got[0].ID != "outline:n1" {
		t.Fatalf("items after success = %+v", got)
	}

	mu.Lock()
	fail = true
	mu.Unlock()
	poller.pollOnce(context.Background())

	if got := store.Items(); len(got) != 1 || got[0].ID != "outline:n1" {
		t.Errorf("items after failure = %+v, want the last good result", got)
	}
	status := poller.SourceStatus()
	if status.Healthy || status.LastError == "" || status.Items != 1 {
		t.Errorf("status = %+v, want unhealthy with the error and the old count", status)
	}
	if strings.Contains(status.LastError, secret) || strings.Contains(logs.String(), secret) {
		t.Errorf("the key leaked:\nstatus: %s\nlogs: %s", status.LastError, logs.String())
	}
}

func TestMentionDisappearsOnceRead(t *testing.T) {
	server := newOutlineServer(t, []map[string]any{notification("n1", EventDocumentMentioned, false, false)})
	store := attention.NewStore(discardLogger(), attention.StoreConfig{})
	poller := NewPoller(NewClient(server.URL, secret, 5*time.Second), store,
		PollerConfig{Interval: time.Hour, BaseURL: server.URL, MaxItems: 100}, discardLogger())

	poller.pollOnce(context.Background())
	if len(store.Items()) != 1 {
		t.Fatalf("items = %+v, want the mention", store.Items())
	}

	server.mu.Lock()
	server.notes = []map[string]any{notification("n1", EventDocumentMentioned, true, false)}
	server.mu.Unlock()
	poller.pollOnce(context.Background())
	if got := store.Items(); len(got) != 0 {
		t.Errorf("items after reading = %+v, want none", got)
	}
}

func TestOldServerWithoutNotificationsAPIGetsAClearWarning(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()

	logs := &logBuffer{}
	store := attention.NewStore(discardLogger(), attention.StoreConfig{})
	poller := NewPoller(NewClient(server.URL, secret, 5*time.Second), store,
		PollerConfig{Interval: time.Hour, BaseURL: server.URL, MaxItems: 100},
		slog.New(slog.NewTextHandler(logs, nil)))

	poller.pollOnce(context.Background())
	poller.pollOnce(context.Background())

	if n := strings.Count(logs.String(), "notifications.list"); n != 1 {
		t.Errorf("warning logged %d times, want once:\n%s", n, logs.String())
	}
	if status := poller.SourceStatus(); status.Healthy || !strings.Contains(status.LastError, "upgrade Outline") {
		t.Errorf("status = %+v", status)
	}
	if _, err := NewClient(server.URL, secret, time.Second).Mentions(context.Background(), 1); !errors.Is(err, ErrNotSupported) {
		t.Errorf("err = %v, want ErrNotSupported", err)
	}
}

func TestMissingKeyIsReportedByNameNotValue(t *testing.T) {
	t.Setenv("TEST_OUTLINE_KEY", "")
	_, err := ResolveKey("TEST_OUTLINE_KEY")
	if err == nil || !strings.Contains(err.Error(), "TEST_OUTLINE_KEY") {
		t.Errorf("err = %v, want the variable name", err)
	}

	t.Setenv("TEST_OUTLINE_KEY", " "+secret+" ")
	got, err := ResolveKey("TEST_OUTLINE_KEY")
	if err != nil || got != secret {
		t.Errorf("ResolveKey = %q, %v", got, err)
	}
}

func TestFixtureModeNeedsNoClient(t *testing.T) {
	store := attention.NewStore(discardLogger(), attention.StoreConfig{})
	poller := NewPoller(nil, store, PollerConfig{
		Interval: time.Hour, BaseURL: "https://wiki.example.com", MaxItems: 100,
		Fixture: "../../testdata/outline-notifications.json",
	}, discardLogger())

	poller.pollOnce(context.Background())

	items := store.Items()
	if len(items) != 2 {
		t.Fatalf("got %d items, want the two unread mentions: %s", len(items), titles(items))
	}
	if status := poller.SourceStatus(); status.Mode != "fixture" || !status.Healthy {
		t.Errorf("status = %+v", status)
	}
	for _, item := range items {
		if item.ID == "outline:6f1c0a52-0000-4000-8000-000000000002" {
			want := "https://wiki.example.com/doc/acme-onboarding-XyZ789?commentId=c9"
			if item.Title != "bob mentioned you in Acme onboarding" || item.Context["url"] != want {
				t.Errorf("comment mention = %+v", item)
			}
		}
	}
}

func TestMissingDocumentStillShowsTheMention(t *testing.T) {
	items := Normalize([]Notification{{
		ID: "n1", Event: EventDocumentMentioned, CreatedAt: time.Now(),
	}}, "https://wiki.example.com", 10, time.Now())
	if len(items) != 1 || items[0].Title != "Someone mentioned you in a document" {
		t.Fatalf("items = %+v", items)
	}
	if _, ok := items[0].Context["url"]; ok {
		t.Errorf("context = %v, want no url when the document is unknown", items[0].Context)
	}
}
