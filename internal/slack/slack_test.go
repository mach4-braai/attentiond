package slack

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/devanmcgeer/attentiond/internal/attention"
)

const fixturePath = "../../testdata/slack-snapshot.json"

const lookback = 72 * time.Hour

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func fixtureSnapshot(t *testing.T) Snapshot {
	t.Helper()
	snap, err := ReadFixture(fixturePath)
	if err != nil {
		t.Fatalf("ReadFixture: %v", err)
	}
	return snap
}

func byID(items []attention.Item, id string) (attention.Item, bool) {
	for _, item := range items {
		if item.ID == id {
			return item, true
		}
	}
	return attention.Item{}, false
}

func TestMentionAppearsWithItsContext(t *testing.T) {
	items := Normalize(fixtureSnapshot(t), time.Now(), lookback)

	item, ok := byID(items, "slack:C0GENERAL:1759990000.000100")
	if !ok {
		t.Fatalf("mention missing from %d items", len(items))
	}
	if item.Title != "alice in #general: <@U0SELF> can you review the rollout plan?" {
		t.Errorf("title = %q", item.Title)
	}
	if item.Label != "mentioned you" || item.State != attention.StateNeedsAttention || item.Source != "slack" {
		t.Errorf("label/state/source = %q/%q/%q", item.Label, item.State, item.Source)
	}
	want := map[string]string{
		"channel":   "C0GENERAL",
		"ts":        "1759990000.000100",
		"permalink": "https://example.slack.com/archives/C0GENERAL/p1759990000000100",
		"sender":    "alice",
		"kind":      "mention",
	}
	for key, value := range want {
		if item.Context[key] != value {
			t.Errorf("context[%s] = %q, want %q", key, item.Context[key], value)
		}
	}
}

func TestMentionDisappearsOnceYouReplyAfterIt(t *testing.T) {
	snap := fixtureSnapshot(t)
	const id = "slack:C0RANDOM:1759980000.000200"

	if _, ok := byID(Normalize(snap, time.Now(), lookback), id); ok {
		t.Fatalf("a mention you answered in the channel is still on the board")
	}

	snap.Mine = nil
	if _, ok := byID(Normalize(snap, time.Now(), lookback), id); !ok {
		t.Errorf("without your reply the mention should be on the board")
	}

	// A reply that came before the mention does not answer it.
	snap.Mine = []Match{{Channel: Channel{ID: "C0RANDOM"}, TS: "1759970000.000100", User: "U0SELF"}}
	if _, ok := byID(Normalize(snap, time.Now(), lookback), id); !ok {
		t.Errorf("a reply from before the mention hid it")
	}
}

func TestDirectMessagesWaitOnlyUntilYouAnswer(t *testing.T) {
	items := Normalize(fixtureSnapshot(t), time.Now(), lookback)

	bob, ok := byID(items, "slack:D0BOB:1759995000.000100")
	if !ok {
		t.Fatalf("bob's DM is missing from %d items", len(items))
	}
	if bob.Title != "bob (DM): ping, got a minute?" || bob.Label != "messaged you" || bob.Context["kind"] != "dm" {
		t.Errorf("title/label/kind = %q/%q/%q", bob.Title, bob.Label, bob.Context["kind"])
	}
	if bob.Context["permalink"] != "https://example.slack.com/archives/D0BOB/p1759995000000100" {
		t.Errorf("permalink = %q", bob.Context["permalink"])
	}

	if _, ok := byID(items, "slack:D0ALICE:1759970000.000100"); ok {
		t.Errorf("alice's DM is on the board although you answered last")
	}
}

func TestMessagesBeyondTheLookbackAreLeftOut(t *testing.T) {
	snap := fixtureSnapshot(t)
	if _, ok := byID(Normalize(snap, time.Now(), lookback), "slack:C0GENERAL:1759000000.000100"); ok {
		t.Errorf("an eleven day old mention is on a 72h board")
	}
	if got := len(Normalize(snap, time.Now(), 30*24*time.Hour)); got != 3 {
		t.Errorf("a 30 day lookback gave %d items, want 3 (old mention included)", got)
	}
	if got := len(Normalize(snap, time.Now(), time.Hour)); got != 0 {
		t.Errorf("a one hour lookback gave %d items, want 0", got)
	}
}

func TestYourOwnMessagesAreNeverItems(t *testing.T) {
	snap := fixtureSnapshot(t)
	snap.Mentions = append(snap.Mentions, Match{
		Channel: Channel{ID: "C0GENERAL", Name: "general"}, TS: "1759999000.000100", User: "U0SELF", Text: "note to self <@U0SELF>",
	})
	for _, item := range Normalize(snap, time.Now(), lookback) {
		if item.Context["sender"] == "me" {
			t.Errorf("own message on the board: %s", item.Title)
		}
	}
}

func TestTitleKeepsTheFirstHundredCharacters(t *testing.T) {
	snap := fixtureSnapshot(t)
	snap.Conversations = []Conversation{{ID: "D0BOB", Messages: []Message{
		{User: "U0BOB", TS: "1759995000.000100", Text: strings.Repeat("x", 150) + "\nsecond line"},
	}}}
	items := Normalize(snap, time.Now(), lookback)
	item, ok := byID(items, "slack:D0BOB:1759995000.000100")
	if !ok {
		t.Fatal("DM missing")
	}
	if want := "bob (DM): " + strings.Repeat("x", 100) + "…"; item.Title != want {
		t.Errorf("title = %q", item.Title)
	}
}

func TestMissingTokenIsReportedByVariableNameOnly(t *testing.T) {
	t.Setenv("TEST_SLACK_TOKEN", "  ")
	_, err := ResolveToken("TEST_SLACK_TOKEN")
	if err == nil || !strings.Contains(err.Error(), "TEST_SLACK_TOKEN") {
		t.Fatalf("err = %v, want the variable named", err)
	}
	t.Setenv("TEST_SLACK_TOKEN", "present")
	if token, err := ResolveToken("TEST_SLACK_TOKEN"); err != nil || token != "present" {
		t.Errorf("token = %q, err = %v", token, err)
	}
}

// fakeSlack serves a recorded workspace for the live path. Timestamps are
// relative to now so the lookback holds whenever the test runs.
type fakeSlack struct {
	*httptest.Server
	token   string
	mode    atomic.Value // "ok", "error" or "limited"
	replied atomic.Bool
	calls   atomic.Int64
	badAuth atomic.Int64
	now     time.Time
}

func ts(t time.Time) string { return fmt.Sprintf("%d.%06d", t.Unix(), t.Nanosecond()/1000) }

func newFakeSlack(t *testing.T, token string) *fakeSlack {
	t.Helper()
	f := &fakeSlack{token: token, now: time.Now()}
	f.mode.Store("ok")
	mention := ts(f.now.Add(-2 * time.Hour))
	bobAt := ts(f.now.Add(-time.Hour))
	reply := ts(f.now.Add(-30 * time.Minute))

	mux := http.NewServeMux()
	mux.HandleFunc("/auth.test", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ok":true,"url":"https://example.slack.com/","user_id":"U0SELF"}`)
	})
	mux.HandleFunc("/search.messages", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("query") {
		case "<@U0SELF>":
			fmt.Fprintf(w, `{"ok":true,"messages":{"matches":[{"channel":{"id":"C0GENERAL","name":"general"},
			  "permalink":"https://example.slack.com/archives/C0GENERAL/p1","text":"<@U0SELF> status?","ts":%q,"user":"U0ALICE","username":"alice"}]}}`, mention)
		case "from:<@U0SELF>":
			if f.replied.Load() {
				fmt.Fprintf(w, `{"ok":true,"messages":{"matches":[{"channel":{"id":"C0GENERAL","name":"general"},"text":"on it","ts":%q,"user":"U0SELF"}]}}`, reply)
				return
			}
			fmt.Fprint(w, `{"ok":true,"messages":{"matches":[]}}`)
		default:
			fmt.Fprint(w, `{"ok":false,"error":"unexpected query"}`)
		}
	})
	mux.HandleFunc("/conversations.list", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("types") != "im,mpim" {
			fmt.Fprint(w, `{"ok":false,"error":"wrong types"}`)
			return
		}
		fmt.Fprint(w, `{"ok":true,"channels":[{"id":"D0BOB","is_im":true,"updated":2},{"id":"D0ALICE","is_im":true,"updated":1}],"response_metadata":{"next_cursor":""}}`)
	})
	mux.HandleFunc("/conversations.history", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("oldest") == "" {
			fmt.Fprint(w, `{"ok":false,"error":"no oldest"}`)
			return
		}
		switch r.URL.Query().Get("channel") {
		case "D0BOB":
			fmt.Fprintf(w, `{"ok":true,"messages":[{"user":"U0BOB","text":"got a minute?","ts":%q}]}`, bobAt)
		case "D0ALICE":
			fmt.Fprintf(w, `{"ok":true,"messages":[{"user":"U0SELF","text":"yes","ts":%q},{"user":"U0ALICE","text":"around?","ts":%q}]}`,
				ts(f.now.Add(-3*time.Hour)), ts(f.now.Add(-4*time.Hour)))
		}
	})
	mux.HandleFunc("/users.info", func(w http.ResponseWriter, r *http.Request) {
		name := map[string]string{"U0ALICE": "alice", "U0BOB": "bob"}[r.URL.Query().Get("user")]
		fmt.Fprintf(w, `{"ok":true,"user":{"name":%q,"profile":{"display_name":""}}}`, name)
	})

	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+f.token {
			f.badAuth.Add(1)
		}
		switch f.mode.Load() {
		case "error":
			http.Error(w, "boom, token was "+f.token, http.StatusInternalServerError)
			return
		case "limited":
			w.Header().Set("Retry-After", "900")
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.Close)
	return f
}

func newLivePoller(t *testing.T, f *fakeSlack) (*Poller, *attention.Store) {
	t.Helper()
	store := attention.NewStore(discardLogger(), attention.StoreConfig{})
	cfg := PollerConfig{Interval: 5 * time.Minute, Lookback: lookback, MaxConversations: 30}
	return NewPoller(NewClient(f.URL, f.token, 5*time.Second), store, cfg, discardLogger()), store
}

func TestLivePollAnswersThroughTheAPIAndHonoursYourReply(t *testing.T) {
	f := newFakeSlack(t, "test-token-value")
	poller, store := newLivePoller(t, f)

	poller.pollOnce(context.Background())
	items := store.Items()
	if len(items) != 2 {
		t.Fatalf("items = %+v, want the mention and bob's DM", items)
	}
	titles := items[0].Title + "|" + items[1].Title
	if !strings.Contains(titles, "alice in #general: <@U0SELF> status?") || !strings.Contains(titles, "bob (DM): got a minute?") {
		t.Errorf("titles = %s", titles)
	}
	if f.badAuth.Load() != 0 {
		t.Errorf("%d requests went out without the bearer token", f.badAuth.Load())
	}

	f.replied.Store(true)
	poller.pollOnce(context.Background())
	items = store.Items()
	if len(items) != 1 || items[0].Context["kind"] != "dm" {
		t.Errorf("after replying in #general items = %+v, want only bob's DM", items)
	}
}

func TestAPIErrorKeepsTheLastResultAndNeverPrintsTheToken(t *testing.T) {
	const token = "test-token-value"
	f := newFakeSlack(t, token)
	poller, store := newLivePoller(t, f)

	poller.pollOnce(context.Background())
	if len(store.Items()) != 2 {
		t.Fatalf("setup: items = %+v", store.Items())
	}

	f.mode.Store("error")
	poller.pollOnce(context.Background())

	if got := len(store.Items()); got != 2 {
		t.Errorf("items after an API error = %d, want the last good 2", got)
	}
	status := poller.SourceStatus()
	if status.Healthy || status.LastError == "" {
		t.Errorf("status = %+v, want unhealthy with an error", status)
	}
	if strings.Contains(status.LastError, token) {
		t.Errorf("the token reached /health: %s", status.LastError)
	}

	f.mode.Store("ok")
	poller.pollOnce(context.Background())
	if status := poller.SourceStatus(); !status.Healthy || status.LastError != "" {
		t.Errorf("status after recovery = %+v", status)
	}
}

func TestRateLimitKeepsTheLastResultAndWaitsForRetryAfter(t *testing.T) {
	f := newFakeSlack(t, "test-token-value")
	poller, store := newLivePoller(t, f)

	if wait := poller.pollOnce(context.Background()); wait != 5*time.Minute {
		t.Errorf("wait after success = %s, want the interval", wait)
	}

	f.mode.Store("limited")
	wait := poller.pollOnce(context.Background())
	if wait != 15*time.Minute {
		t.Errorf("wait after a 429 = %s, want Retry-After (15m)", wait)
	}
	if got := len(store.Items()); got != 2 {
		t.Errorf("items after a 429 = %d, want the last good 2", got)
	}
}

func TestFixtureModeNeedsNoTokenAndReplaysTheSnapshot(t *testing.T) {
	store := attention.NewStore(discardLogger(), attention.StoreConfig{})
	poller := NewPoller(nil, store, PollerConfig{
		Interval: time.Minute, Lookback: lookback, MaxConversations: 30, Fixture: fixturePath,
	}, discardLogger())

	poller.pollOnce(context.Background())

	if got := len(store.Items()); got != 2 {
		t.Errorf("fixture produced %d items, want 2", got)
	}
	if status := poller.SourceStatus(); status.Mode != "fixture" || !status.Healthy || status.Items != 2 {
		t.Errorf("status = %+v", status)
	}
}
