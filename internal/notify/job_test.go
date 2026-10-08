package notify

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/devanmcgeer/attentiond/internal/attention"
)

// A tool finishing happens between polls. The store has to report it itself,
// and a snooze made before the tool ran must not swallow the hand-back: the
// new label ends it.
func TestAToolHandingBackNotifiesEvenOnASnoozedItem(t *testing.T) {
	now := time.Date(2026, 9, 13, 9, 0, 0, 0, time.UTC)
	notifier, sender := testNotifier(t, []string{"needs human", "agent failed"}, &now)
	store := attention.NewStore(slog.New(slog.NewTextHandler(io.Discard, nil)), attention.StoreConfig{
		Observer: notifier.Observe,
		Now:      func() time.Time { return now },
	})

	pull := item("github:didx-xyz/tofu#1", "rebase required", attention.ToneAttention)
	pull.Context = map[string]string{"role": "author"}
	store.ReplaceSource("github", []attention.Item{pull})
	if _, err := store.Decide(pull.ID, attention.DecisionSnooze, now.Add(4*time.Hour)); err != nil {
		t.Fatalf("snooze: %v", err)
	}

	if _, err := store.QueueJob(pull.ID, "agent-rebase"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.StartJob(pull.ID, "/tmp/run.log"); err != nil {
		t.Fatal(err)
	}
	drain(notifier)
	if len(sender.sent) != 0 {
		t.Fatalf("a tool starting notified: %+v", sender.sent)
	}

	if _, err := store.FinishJob(pull.ID, attention.ResultNeedsHuman, "conflict in main.tf"); err != nil {
		t.Fatal(err)
	}
	drain(notifier)
	if len(sender.sent) != 1 || sender.sent[0].Title != "Needs human" || sender.sent[0].Sound != "request" {
		t.Fatalf("needs-human did not notify on the snoozed item: %+v", sender.sent)
	}

	// The next run on the same item hands back again, inside the cooldown. A
	// tool starting in between is work starting again, so it is news.
	now = now.Add(time.Minute)
	if _, err := store.QueueJob(pull.ID, "agent-rebase"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FinishJob(pull.ID, attention.ResultFailed, "timed out"); err != nil {
		t.Fatal(err)
	}
	drain(notifier)
	if len(sender.sent) != 2 || sender.sent[1].Title != "Agent failed" {
		t.Fatalf("a second hand-back was swallowed: %+v", sender.sent)
	}
}
