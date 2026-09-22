package attention

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func pullRequest(id, label string, priority int) Item {
	return Item{
		ID:       Key("github", id),
		Source:   "github",
		Title:    id,
		State:    StateNeedsAttention,
		Severity: SeverityWarning,
		Label:    label,
		Tone:     ToneAttention,
		Priority: priority,
	}
}

func queued(store *Store) []string {
	var titles []string
	for _, item := range store.Attention() {
		titles = append(titles, item.Title)
	}
	return titles
}

func TestSnoozeLeavesTheQueueAndStaysOnTheBoard(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	store := testStore(t, StoreConfig{}, &now)

	store.ReplaceSource("github", []Item{
		pullRequest("didx-xyz/tofu#1", "review requested", PriorityAsked),
		pullRequest("didx-xyz/tofu#2", "review requested", PriorityAsked),
	})

	if _, err := store.Decide("github:didx-xyz/tofu#1", DecisionSnooze, now.Add(4*time.Hour)); err != nil {
		t.Fatalf("snooze: %v", err)
	}

	if got := queued(store); len(got) != 1 || got[0] != "didx-xyz/tofu#2" {
		t.Fatalf("a snoozed review is still in the queue: %v", got)
	}
	if len(store.Items()) != 2 {
		t.Fatal("a snooze dropped the item off the whole board, not just the queue")
	}

	var snoozed Item
	for _, item := range store.Items() {
		if item.ID == "github:didx-xyz/tofu#1" {
			snoozed = item
		}
	}
	if !snoozed.Snoozed || snoozed.SnoozedUntil == nil || !snoozed.SnoozedUntil.Equal(now.Add(4*time.Hour)) {
		t.Fatalf("the board does not say why the item is quiet: %+v", snoozed)
	}
}

// The snooze has to lapse on the clock alone. An item posted to /api/events has
// no poller behind it, so if waking needed a write it would never come back.
func TestSnoozeLapsesWithoutAnyWrite(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	store := testStore(t, StoreConfig{}, &now)

	store.Put(Item{
		ID: Key("tofu", "plan-prod"), Source: "tofu", Title: "tofu plan wants approval",
		State: StateNeedsAttention, Severity: SeverityWarning, Label: "waiting for approval",
	})
	if _, err := store.Decide("tofu:plan-prod", DecisionSnooze, now.Add(time.Hour)); err != nil {
		t.Fatalf("snooze: %v", err)
	}
	if got := queued(store); len(got) != 0 {
		t.Fatalf("snoozed item still queued: %v", got)
	}

	now = now.Add(61 * time.Minute)
	if got := queued(store); len(got) != 1 {
		t.Fatalf("the snooze outlived its deadline: %v", got)
	}
	if len(store.Decisions()) != 0 {
		t.Error("a lapsed snooze is still being carried")
	}
}

// The reason you deferred something is the label it had. When that changes the
// deferral is answering a question nobody asked any more.
func TestSnoozeEndsWhenTheLabelMoves(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	store := testStore(t, StoreConfig{}, &now)

	store.ReplaceSource("github", []Item{pullRequest("didx-xyz/tofu#1", "review requested", PriorityAsked)})
	if _, err := store.Decide("github:didx-xyz/tofu#1", DecisionSnooze, now.Add(7*24*time.Hour)); err != nil {
		t.Fatalf("snooze: %v", err)
	}
	if got := queued(store); len(got) != 0 {
		t.Fatalf("snoozed item still queued: %v", got)
	}

	now = now.Add(time.Hour)
	store.ReplaceSource("github", []Item{pullRequest("didx-xyz/tofu#1", "ready to merge", PriorityOneClick)})

	queue := store.Attention()
	if len(queue) != 1 || queue[0].Snoozed {
		t.Fatalf("a snooze survived the item becoming mergeable: %+v", queue)
	}
}

func TestBumpOutranksEvenATopLabel(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	store := testStore(t, StoreConfig{
		TopLabels: map[string]bool{"waiting for approval": true},
	}, &now)

	store.Put(Item{
		ID: Key("tofu", "plan-prod"), Source: "tofu", Title: "tofu plan wants approval",
		State: StateNeedsAttention, Severity: SeverityWarning, Label: "waiting for approval",
	})
	store.ReplaceSource("github", []Item{pullRequest("didx-xyz/tofu#1", "review requested", PriorityAsked)})

	if _, err := store.Decide("github:didx-xyz/tofu#1", DecisionBump, time.Time{}); err != nil {
		t.Fatalf("bump: %v", err)
	}

	queue := store.Attention()
	if len(queue) != 2 || queue[0].Title != "didx-xyz/tofu#1" {
		t.Fatalf("a bump did not reach the top: %+v", queue)
	}
	if queue[0].Priority != PriorityBumped || !queue[0].Bumped {
		t.Fatalf("the served priority is not the one it was sorted by: %+v", queue[0])
	}

	// Clearing gives the source's own rank back. A bump that permanently
	// overwrote the priority would make the button one-way.
	if _, err := store.Clear("github:didx-xyz/tofu#1"); err != nil {
		t.Fatalf("clear: %v", err)
	}
	queue = store.Attention()
	if queue[0].Title != "tofu plan wants approval" || queue[1].Priority != PriorityAsked {
		t.Fatalf("clearing a bump did not restore the ranking: %+v", queue)
	}
}

func TestStaleLeavesEveryListButItsOwn(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	store := testStore(t, StoreConfig{StaleAfter: 30 * 24 * time.Hour}, &now)

	old := pullRequest("didx-xyz/mono#7", "review requested", PriorityAsked)
	old.UpdatedAt = now.Add(-40 * 24 * time.Hour)
	fresh := pullRequest("didx-xyz/tofu#1", "review requested", PriorityAsked)
	fresh.UpdatedAt = now.Add(-time.Hour)
	store.ReplaceSource("github", []Item{old, fresh})

	if got := queued(store); len(got) != 1 || got[0] != "didx-xyz/tofu#1" {
		t.Fatalf("a forty day old review is still in the queue: %v", got)
	}

	var stale, live int
	for _, item := range store.Items() {
		if item.Stale {
			stale++
			continue
		}
		live++
	}
	if stale != 1 || live != 1 {
		t.Fatalf("the board did not split: %d stale, %d live", stale, live)
	}

	// Bumping is a statement that this one still matters, which is the answer
	// to the staleness question.
	if _, err := store.Decide("github:didx-xyz/mono#7", DecisionBump, time.Time{}); err != nil {
		t.Fatalf("bump: %v", err)
	}
	queue := store.Attention()
	if len(queue) != 2 || queue[0].Title != "didx-xyz/mono#7" || queue[0].Stale {
		t.Fatalf("a bumped item stayed stale: %+v", queue)
	}
}

func TestDecisionsSurviveARestart(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "decisions.json")

	first := testStore(t, StoreConfig{DecisionPath: path}, &now)
	first.ReplaceSource("github", []Item{
		pullRequest("didx-xyz/tofu#1", "review requested", PriorityAsked),
		pullRequest("didx-xyz/tofu#2", "review requested", PriorityAsked),
	})
	if _, err := first.Decide("github:didx-xyz/tofu#1", DecisionSnooze, now.Add(4*time.Hour)); err != nil {
		t.Fatalf("snooze: %v", err)
	}
	if _, err := first.Decide("github:didx-xyz/tofu#2", DecisionBump, time.Time{}); err != nil {
		t.Fatalf("bump: %v", err)
	}

	now = now.Add(time.Minute)
	second := testStore(t, StoreConfig{DecisionPath: path}, &now)
	second.ReplaceSource("github", []Item{
		pullRequest("didx-xyz/tofu#1", "review requested", PriorityAsked),
		pullRequest("didx-xyz/tofu#2", "review requested", PriorityAsked),
	})

	queue := second.Attention()
	if len(queue) != 1 || queue[0].ID != "github:didx-xyz/tofu#2" || !queue[0].Bumped {
		t.Fatalf("decisions did not survive the restart: %+v", queue)
	}

	// Past the deadline, the same file restores nothing: a snooze that lapsed
	// while the daemon was down is not one you have to wake by hand.
	now = now.Add(5 * time.Hour)
	third := testStore(t, StoreConfig{DecisionPath: path}, &now)
	third.ReplaceSource("github", []Item{pullRequest("didx-xyz/tofu#1", "review requested", PriorityAsked)})
	if got := queued(third); len(got) != 1 {
		t.Fatalf("an expired snooze was restored: %v", got)
	}
}

func TestConcurrentDecisionsAllReachTheFile(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "decisions.json")

	const count = 24
	items := make([]Item, 0, count)
	for i := range count {
		items = append(items, pullRequest(fmt.Sprintf("didx-xyz/tofu#%d", i), "review requested", PriorityAsked))
	}

	store := testStore(t, StoreConfig{DecisionPath: path}, &now)
	store.ReplaceSource("github", items)

	var wg sync.WaitGroup
	for _, item := range items {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			if _, err := store.Decide(id, DecisionBump, time.Time{}); err != nil {
				t.Errorf("bump %s: %v", id, err)
			}
		}(item.ID)
	}
	wg.Wait()

	restored, err := loadDecisions(path, now)
	if err != nil {
		t.Fatalf("loadDecisions: %v", err)
	}
	if len(restored) != count {
		t.Fatalf("the file holds %d of %d decisions: a write landed after a newer one", len(restored), count)
	}
}

func TestDecidingOnSomethingTheDaemonDoesNotHold(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	store := testStore(t, StoreConfig{}, &now)

	if _, err := store.Decide("github:didx-xyz/tofu#404", DecisionSnooze, now.Add(time.Hour)); !errors.Is(err, ErrItemMissing) {
		t.Fatalf("err = %v, want ErrItemMissing", err)
	}
	if _, err := store.Clear("github:didx-xyz/tofu#404"); !errors.Is(err, ErrItemMissing) {
		t.Fatalf("err = %v, want ErrItemMissing", err)
	}
}

func TestACorruptDecisionsFileCostsAWarningNotTheDaemon(t *testing.T) {
	path := filepath.Join(t.TempDir(), "decisions.json")
	if err := os.WriteFile(path, []byte("{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	store := NewStore(slog.New(slog.NewTextHandler(io.Discard, nil)), StoreConfig{DecisionPath: path})
	if store == nil || len(store.Decisions()) != 0 {
		t.Fatal("a corrupt file should leave an empty set, not a dead daemon")
	}
}

func TestParseDecisionKindRejectsGarbage(t *testing.T) {
	if _, err := ParseDecisionKind("postpone"); err == nil {
		t.Error("ParseDecisionKind accepted a decision outside the model")
	}
	if kind, err := ParseDecisionKind("snooze"); err != nil || kind != DecisionSnooze {
		t.Errorf("ParseDecisionKind(snooze) = %q, %v", kind, err)
	}
}
