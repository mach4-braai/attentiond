package attention

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func logStore(t *testing.T, path string, retention time.Duration, clock *time.Time) *Store {
	t.Helper()
	return testStore(t, StoreConfig{SignalLogPath: path, SignalRetention: retention}, clock)
}

func kinds(entries []SignalEntry) []string {
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		out = append(out, entry.Kind)
	}
	return out
}

func TestSignalLogRecordsAppearedChangedAndDisappeared(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "state", "signals.jsonl")
	store := logStore(t, path, 0, &now)

	store.ReplaceSource("herdr", []Item{adapterItem("w1:p1", StateWorking, SeverityInfo, "claude")})
	now = now.Add(time.Minute)
	store.ReplaceSource("herdr", []Item{adapterItem("w1:p1", StateNeedsAttention, SeverityWarning, "claude")})
	now = now.Add(time.Minute)
	store.ReplaceSource("herdr", nil)

	entries, err := store.Signals(time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(kinds(entries), ","); got != "disappeared,changed,appeared" {
		t.Fatalf("kinds = %s, want disappeared,changed,appeared", got)
	}

	disappeared, changed, appeared := entries[0], entries[1], entries[2]
	if appeared.ItemID != "herdr:w1:p1" || appeared.Source != "herdr" || appeared.Title != "claude" ||
		appeared.State != "working" || appeared.Severity != "info" || !appeared.At.Equal(now.Add(-2*time.Minute)) {
		t.Errorf("appeared = %+v", appeared)
	}
	if appeared.PrevState != "" || appeared.PrevLabel != "" {
		t.Errorf("an appearance has no previous reading: %+v", appeared)
	}
	if changed.State != "needs_attention" || changed.PrevState != "working" ||
		changed.Label == changed.PrevLabel || changed.PrevLabel == "" {
		t.Errorf("changed = %+v", changed)
	}
	if disappeared.State != "needs_attention" || disappeared.PrevState != "" || !disappeared.At.Equal(now) {
		t.Errorf("disappeared = %+v", disappeared)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 3 {
		t.Fatalf("file has %d lines, want one per entry:\n%s", len(lines), raw)
	}
	if strings.Contains(lines[0], "prev_") {
		t.Errorf("appeared line carries prev_ fields: %s", lines[0])
	}
	if !strings.Contains(lines[1], `"prev_state":"working"`) {
		t.Errorf("changed line lacks prev_state: %s", lines[1])
	}
}

func TestSignalLogIgnoresPollsThatChangeNothingAndOtherFields(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	store := logStore(t, filepath.Join(t.TempDir(), "signals.jsonl"), 0, &now)

	item := adapterItem("w1:p1", StateWorking, SeverityInfo, "claude")
	store.ReplaceSource("herdr", []Item{item})
	store.ReplaceSource("herdr", []Item{item})
	item.Title = "claude renamed"
	store.ReplaceSource("herdr", []Item{item})

	entries, err := store.Signals(time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Kind != SignalAppeared {
		t.Fatalf("entries = %v, want only the appearance: a title change keeps state and label", kinds(entries))
	}
}

func TestSignalLogRecordsAnExpiredEventAsDisappeared(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	store := testStore(t, StoreConfig{
		EventTTL:      time.Minute,
		SignalLogPath: filepath.Join(t.TempDir(), "signals.jsonl"),
	}, &now)

	store.Put(Item{ID: Key("tofu", "plan"), Source: "tofu", Title: "plan", State: StateDone, Severity: SeverityInfo})
	now = now.Add(2 * time.Minute)
	if items := store.Items(); len(items) != 0 {
		t.Fatalf("event outlived its ttl: %+v", items)
	}

	entries, err := store.Signals(time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(kinds(entries), ","); got != "disappeared,appeared" {
		t.Fatalf("kinds = %s, want disappeared,appeared", got)
	}
}

func TestSignalsFilterBySinceAndLimit(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	store := logStore(t, filepath.Join(t.TempDir(), "signals.jsonl"), 0, &now)

	start := now
	for _, id := range []string{"a", "b", "c", "d"} {
		store.Put(Item{ID: Key("tofu", id), Source: "tofu", Title: id, State: StateWorking, Severity: SeverityInfo})
		now = now.Add(time.Minute)
	}

	all, err := store.Signals(time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	var titles []string
	for _, entry := range all {
		titles = append(titles, entry.Title)
	}
	if got := strings.Join(titles, ""); got != "dcba" {
		t.Fatalf("order = %s, want newest first: dcba", got)
	}

	since, err := store.Signals(start.Add(2*time.Minute), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(since) != 2 || since[0].Title != "d" || since[1].Title != "c" {
		t.Fatalf("since is inclusive and keeps newest first: %+v", since)
	}

	limited, err := store.Signals(time.Time{}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 3 || limited[0].Title != "d" || limited[2].Title != "b" {
		t.Fatalf("limit keeps the newest entries: %+v", limited)
	}
}

func TestSignalLogSurvivesARestart(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "signals.jsonl")

	first := logStore(t, path, 720*time.Hour, &now)
	first.Put(Item{ID: Key("tofu", "plan"), Source: "tofu", Title: "plan", State: StateWorking, Severity: SeverityInfo})

	now = now.Add(time.Minute)
	second := logStore(t, path, 720*time.Hour, &now)
	entries, err := second.Signals(time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].ItemID != "tofu:plan" {
		t.Fatalf("a new store lost the old log: %+v", entries)
	}

	second.Put(Item{ID: Key("tofu", "apply"), Source: "tofu", Title: "apply", State: StateWorking, Severity: SeverityInfo})
	entries, _ = second.Signals(time.Time{}, 0)
	if len(entries) != 2 || entries[0].ItemID != "tofu:apply" {
		t.Fatalf("the new store did not append after the old entries: %+v", entries)
	}
}

func TestSignalsWithoutALogAreEmpty(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	store := testStore(t, StoreConfig{}, &now)
	store.Put(Item{ID: Key("tofu", "plan"), Source: "tofu", State: StateWorking, Severity: SeverityInfo})

	entries, err := store.Signals(time.Time{}, 0)
	if err != nil || entries == nil || len(entries) != 0 {
		t.Fatalf("entries = %v, err = %v, want an empty non-nil list", entries, err)
	}
}

func TestSignalLogSkipsTornLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "signals.jsonl")
	good := `{"at":"2026-09-12T10:00:00Z","kind":"appeared","item_id":"a","source":"s","title":"a","state":"working","label":"working","severity":"info"}`
	if err := os.WriteFile(path, []byte(good+"\n{\"at\":\"2026-09-12T10:0"), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 12, 10, 5, 0, 0, time.UTC)
	entries, err := logStore(t, path, 0, &now).Signals(time.Time{}, 0)
	if err != nil || len(entries) != 1 || entries[0].ItemID != "a" {
		t.Fatalf("entries = %+v, err = %v, want the one intact line", entries, err)
	}
}

func entryAt(id string, at time.Time) SignalEntry {
	return SignalEntry{At: at, Kind: SignalAppeared, ItemID: id, Source: "s", Title: id, State: "working", Label: "working", Severity: "info"}
}

func ids(t *testing.T, log *signalLog) string {
	t.Helper()
	entries, err := readSignals(log.path)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, entry := range entries {
		out = append(out, entry.ItemID)
	}
	return strings.Join(out, ",")
}

func TestSignalLogPrunesPastRetentionAtMostOncePerHour(t *testing.T) {
	start := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	now := start
	log := &signalLog{
		path:      filepath.Join(t.TempDir(), "signals.jsonl"),
		retention: 24 * time.Hour,
		now:       func() time.Time { return now },
	}
	write := func(entries ...SignalEntry) {
		t.Helper()
		if err := log.append(entries); err != nil {
			t.Fatal(err)
		}
	}

	write(entryAt("old", start))
	if got := ids(t, log); got != "old" {
		t.Fatalf("after the first write = %s", got)
	}

	// 25 hours on, "old" is past retention and the last prune was long ago.
	now = start.Add(25 * time.Hour)
	write(entryAt("new", now))
	if got := ids(t, log); got != "new" {
		t.Fatalf("after the prune = %s, want only new", got)
	}

	// A stale entry written half an hour after the prune is not dropped: the
	// next prune is not due.
	now = now.Add(30 * time.Minute)
	write(entryAt("late-stale", start))
	if got := ids(t, log); got != "new,late-stale" {
		t.Fatalf("within the hour = %s, want no prune", got)
	}

	now = now.Add(31 * time.Minute)
	write(entryAt("later", now))
	if got := ids(t, log); got != "new,later" {
		t.Fatalf("an hour after the prune = %s, want late-stale dropped", got)
	}

	leftovers, err := filepath.Glob(log.path + ".*")
	if err != nil || len(leftovers) != 0 {
		t.Fatalf("temporary files left behind: %v (%v)", leftovers, err)
	}
}

func TestSignalLogWithZeroRetentionKeepsEverything(t *testing.T) {
	start := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	now := start
	log := &signalLog{path: filepath.Join(t.TempDir(), "signals.jsonl"), now: func() time.Time { return now }}

	if err := log.append([]SignalEntry{entryAt("old", start)}); err != nil {
		t.Fatal(err)
	}
	now = start.Add(10000 * time.Hour)
	if err := log.append([]SignalEntry{entryAt("new", now)}); err != nil {
		t.Fatal(err)
	}
	if got := ids(t, log); got != "old,new" {
		t.Fatalf("ids = %s, want everything kept", got)
	}
}
