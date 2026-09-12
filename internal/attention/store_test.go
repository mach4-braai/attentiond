package attention

import (
	"io"
	"log/slog"
	"testing"
	"time"
)

func testStore(t *testing.T, ttl time.Duration, clock *time.Time) *Store {
	t.Helper()
	store := NewStore(slog.New(slog.NewTextHandler(io.Discard, nil)), ttl)
	store.now = func() time.Time { return *clock }
	return store
}

func adapterItem(id string, state State, severity Severity, title string) Item {
	return Item{
		ID:       Key("herdr", id),
		Source:   "herdr",
		Title:    title,
		State:    state,
		Severity: severity,
	}
}

func TestReplaceSourceDropsItemsTheAdapterStoppedReporting(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	store := testStore(t, 0, &now)

	store.ReplaceSource("herdr", []Item{
		adapterItem("w1:p1", StateWorking, SeverityInfo, "attentiond · claude"),
		adapterItem("w1:p2", StateDone, SeverityInfo, "attentiond · codex"),
	})
	store.ReplaceSource("herdr", []Item{
		adapterItem("w1:p1", StateWorking, SeverityInfo, "attentiond · claude"),
	})

	items := store.Items()
	if len(items) != 1 || items[0].ID != "herdr:w1:p1" {
		t.Fatalf("closed pane still reported: %+v", items)
	}
}

func TestReplaceSourceLeavesOtherSourcesAlone(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	store := testStore(t, 0, &now)

	store.Put(Item{ID: Key("tofu", "plan"), Source: "tofu", State: StateWorking, Severity: SeverityInfo})
	store.ReplaceSource("herdr", []Item{adapterItem("w1:p1", StateWorking, SeverityInfo, "a")})
	store.ReplaceSource("herdr", nil)

	items := store.Items()
	if len(items) != 1 || items[0].Source != "tofu" {
		t.Fatalf("a herdr poll disturbed another source: %+v", items)
	}
}

func TestUpdatedAtTracksVisibleChangeOnly(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	store := testStore(t, 0, &now)

	store.ReplaceSource("herdr", []Item{adapterItem("w1:p1", StateWorking, SeverityInfo, "attentiond · claude")})
	first := store.Items()[0].UpdatedAt

	now = now.Add(30 * time.Second)
	store.ReplaceSource("herdr", []Item{adapterItem("w1:p1", StateWorking, SeverityInfo, "attentiond · claude")})
	if got := store.Items()[0].UpdatedAt; !got.Equal(first) {
		t.Fatalf("an unchanged poll moved UpdatedAt from %s to %s", first, got)
	}

	now = now.Add(30 * time.Second)
	store.ReplaceSource("herdr", []Item{adapterItem("w1:p1", StateNeedsAttention, SeverityWarning, "attentiond · claude")})
	if got := store.Items()[0].UpdatedAt; !got.Equal(now) {
		t.Fatalf("a state change left UpdatedAt at %s, want %s", got, now)
	}
}

func TestAttentionSelectsUnacknowledgedWork(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	store := testStore(t, 0, &now)

	store.ReplaceSource("herdr", []Item{
		adapterItem("w1:p1", StateWorking, SeverityInfo, "working"),
		adapterItem("w1:p2", StateWaiting, SeverityInfo, "waiting"),
		adapterItem("w1:p3", StateDone, SeverityInfo, "done"),
		adapterItem("w1:p4", StateNeedsAttention, SeverityWarning, "blocked"),
		adapterItem("w1:p5", StateFailed, SeverityCritical, "failed"),
	})

	var titles []string
	for _, item := range store.Attention() {
		titles = append(titles, item.Title)
	}
	want := []string{"failed", "blocked", "done"}
	if len(titles) != len(want) {
		t.Fatalf("attention returned %v, want %v", titles, want)
	}
	for i := range want {
		if titles[i] != want[i] {
			t.Fatalf("attention returned %v, want %v (severity should order it)", titles, want)
		}
	}
}

func TestTerminalEventItemsExpireButAdapterItemsDoNot(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	store := testStore(t, time.Hour, &now)

	store.Put(Item{ID: Key("tofu", "apply"), Source: "tofu", State: StateFailed, Severity: SeverityCritical})
	store.Put(Item{ID: Key("tofu", "plan"), Source: "tofu", State: StateWorking, Severity: SeverityInfo})
	store.ReplaceSource("herdr", []Item{adapterItem("w1:p1", StateDone, SeverityInfo, "done agent")})

	now = now.Add(61 * time.Minute)
	ids := map[string]bool{}
	for _, item := range store.Items() {
		ids[item.ID] = true
	}

	if ids["tofu:apply"] {
		t.Error("a failed event survived its TTL")
	}
	if !ids["tofu:plan"] {
		t.Error("an in-flight event expired")
	}
	if !ids["herdr:w1:p1"] {
		t.Error("an adapter-owned item expired; the adapter owns its lifecycle")
	}
}

func TestParseStateAndSeverityRejectGarbage(t *testing.T) {
	if _, err := ParseState("finished"); err == nil {
		t.Error("ParseState accepted a state outside the model")
	}
	if _, err := ParseSeverity("urgent"); err == nil {
		t.Error("ParseSeverity accepted an unknown severity")
	}
	if state, err := ParseState("needs_attention"); err != nil || state != StateNeedsAttention {
		t.Errorf("ParseState(needs_attention) = %q, %v", state, err)
	}
}
