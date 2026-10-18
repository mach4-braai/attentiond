package attention

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func authored(id, label string) Item {
	item := pullRequest(id, label, PriorityActionable)
	item.Context = map[string]string{"role": "author"}
	return item
}

// A watch is a second standing instruction beside the decision, so snoozing a
// watched pull request must not unwatch it, and both have to come back after a
// restart.
func TestWatchSurvivesARestartBesideASnooze(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "decisions.json")

	first := testStore(t, StoreConfig{DecisionPath: path}, &now)
	first.ReplaceSource("github", []Item{authored("didx-xyz/tofu#1", "rebase required")})
	if _, err := first.Watch("github:didx-xyz/tofu#1"); err != nil {
		t.Fatalf("watch: %v", err)
	}
	if _, err := first.Decide("github:didx-xyz/tofu#1", DecisionSnooze, now.Add(4*time.Hour)); err != nil {
		t.Fatalf("snooze: %v", err)
	}

	now = now.Add(time.Minute)
	second := testStore(t, StoreConfig{DecisionPath: path}, &now)
	second.ReplaceSource("github", []Item{authored("didx-xyz/tofu#1", "rebase required")})

	item, ok := second.Get("github:didx-xyz/tofu#1")
	if !ok || !item.Watched || !item.Snoozed {
		t.Fatalf("watch and snooze did not both survive the restart: %+v", item)
	}

	if _, err := second.Unwatch("github:didx-xyz/tofu#1"); err != nil {
		t.Fatalf("unwatch: %v", err)
	}
	third := testStore(t, StoreConfig{DecisionPath: path}, &now)
	third.ReplaceSource("github", []Item{authored("didx-xyz/tofu#1", "rebase required")})
	if item, _ := third.Get("github:didx-xyz/tofu#1"); item.Watched || !item.Snoozed {
		t.Fatalf("unwatch did not reach the file, or took the snooze with it: %+v", item)
	}
}

func TestAVersionOneDecisionsFileStillLoads(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "decisions.json")
	old := `{"version":1,"decisions":[{"item":"github:didx-xyz/tofu#1","kind":"bump","label":"review requested","made_at":"2026-09-12T09:00:00Z"}]}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}

	store := testStore(t, StoreConfig{DecisionPath: path}, &now)
	if got := store.Decisions(); len(got) != 1 || got[0].Kind != DecisionBump {
		t.Fatalf("a version 1 file lost its decisions: %+v", got)
	}
}

// GitHub only reports open pull requests, so a watched one that merged stops
// being seen. Its watch goes at the first load after watchTTL, while a pull
// request that is still polled keeps its watch however old it is.
func TestAWatchOnWorkThatStoppedAppearingIsDroppedAtLoad(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "decisions.json")

	store := testStore(t, StoreConfig{DecisionPath: path}, &now)
	store.ReplaceSource("github", []Item{
		authored("didx-xyz/tofu#1", "awaiting review"),
		authored("didx-xyz/tofu#2", "awaiting review"),
	})
	for _, key := range []string{"github:didx-xyz/tofu#1", "github:didx-xyz/tofu#2"} {
		if _, err := store.Watch(key); err != nil {
			t.Fatalf("watch %s: %v", key, err)
		}
	}

	// #1 merges and leaves the poll; #2 keeps being reported for weeks.
	for range 20 {
		now = now.Add(24 * time.Hour)
		store.ReplaceSource("github", []Item{authored("didx-xyz/tofu#2", "awaiting review")})
	}

	restarted := testStore(t, StoreConfig{DecisionPath: path}, &now)
	watches := restarted.Watches()
	if len(watches) != 1 || watches[0].Item != "github:didx-xyz/tofu#2" {
		t.Fatalf("watches after the restart = %+v, want only the pull request still polled", watches)
	}
}
