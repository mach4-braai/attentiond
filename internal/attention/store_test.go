package attention

import (
	"io"
	"log/slog"
	"testing"
	"time"
)

func testStore(t *testing.T, cfg StoreConfig, clock *time.Time) *Store {
	t.Helper()
	cfg.Now = func() time.Time { return *clock }
	return NewStore(slog.New(slog.NewTextHandler(io.Discard, nil)), cfg)
}

func adapterItem(id string, state State, severity Severity, title string) Item {
	label, tone := DefaultDisplay(state)
	return Item{
		ID:       Key("herdr", id),
		Source:   "herdr",
		Title:    title,
		State:    state,
		Severity: severity,
		Label:    label,
		Tone:     tone,
		Priority: DefaultPriority(state),
	}
}

func TestReplaceSourceDropsItemsTheAdapterStoppedReporting(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	store := testStore(t, StoreConfig{}, &now)

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
	store := testStore(t, StoreConfig{}, &now)

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
	store := testStore(t, StoreConfig{}, &now)

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
	store := testStore(t, StoreConfig{}, &now)

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
			t.Fatalf("attention returned %v, want %v (priority puts done last, severity breaks the rest)", titles, want)
		}
	}
}

func TestTerminalEventItemsExpireButAdapterItemsDoNot(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	store := testStore(t, StoreConfig{EventTTL: time.Hour}, &now)

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

func TestPriorityOutranksSeverityAndRecency(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	store := testStore(t, StoreConfig{}, &now)

	// A mergeable pull request is the oldest and least severe thing here, and
	// it is still what to do first.
	ready := adapterItem("pr", StateNeedsAttention, SeverityInfo, "ready to merge")
	ready.Priority = PriorityOneClick
	ready.UpdatedAt = now.Add(-2 * time.Hour)

	shouted := adapterItem("blocked", StateNeedsAttention, SeverityCritical, "blocked agent")
	shouted.UpdatedAt = now

	store.ReplaceSource("herdr", []Item{shouted, ready})

	queue := store.Attention()
	if len(queue) != 2 || queue[0].Title != "ready to merge" {
		t.Fatalf("priority did not win: %+v", queue)
	}
}

func TestDisplayFillsInWhatASourceLeftOut(t *testing.T) {
	label, tone := Item{State: StateFailed}.Display()
	if label != "failed" || tone != ToneFailed {
		t.Fatalf("Display() = %q, %q for a bare failed item", label, tone)
	}

	label, tone = Item{State: StateNeedsAttention, Label: "ready to merge", Tone: ToneReady}.Display()
	if label != "ready to merge" || tone != ToneReady {
		t.Fatalf("Display() overrode what the source said: %q, %q", label, tone)
	}
}

func TestTopLabelsOutrankEveryRankASourceCanGiveItself(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	store := testStore(t, StoreConfig{
		TopLabels: map[string]bool{"waiting for approval": true},
	}, &now)

	// A meeting about to start is the highest rank any source sets.
	meeting := adapterItem("meeting", StateNeedsAttention, SeverityWarning, "Platform standup")
	meeting.Label = "starting soon"
	meeting.Priority = PriorityDeadline

	store.ReplaceSource("calendar", []Item{meeting})
	store.Put(Item{
		ID:       Key("shell", "tofu"),
		Source:   "shell",
		Title:    "didx-org · tofu plan -out=tfplan",
		State:    StateNeedsAttention,
		Severity: SeverityWarning,
		// Matching folds case and space, because this came from a config file.
		Label:    "  Waiting For Approval ",
		Priority: DefaultPriority(StateNeedsAttention),
	})

	queue := store.Attention()
	if len(queue) != 2 || queue[0].Source != "shell" {
		t.Fatalf("a top label did not reach the top: %+v", queue)
	}
	// The served priority has to be the one it was sorted by, or a consumer
	// cannot see why this is first.
	if queue[0].Priority != PriorityTop {
		t.Errorf("priority = %d, want %d", queue[0].Priority, PriorityTop)
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
