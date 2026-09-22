package attention

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
)

// StoreConfig tunes the store's clocks, its ordering overrides and the
// standing human decisions it keeps.
type StoreConfig struct {
	// TopLabels are labels promoted to PriorityTop, above every rank a source
	// can give itself. Keys are lower case; lookups fold.
	//
	// This is the one place ordering comes from configuration rather than from
	// what the work is, because "this matters more than everything" is a
	// judgement about your week, not a property of a pull request.
	TopLabels map[string]bool
	// EventTTL bounds how long terminal items written by Put survive. Zero
	// keeps them forever.
	EventTTL time.Duration
	// DoneTTL bounds how long a done item stays in the attention queue,
	// whichever source owns it. Finished work nobody has looked at is the
	// point of the daemon, but only for as long as somebody might still be
	// coming back to look: after that it is noise sitting on top of work that
	// still needs doing. It keeps appearing in Items, which is the whole
	// board. Zero keeps done in the queue until its source drops it.
	DoneTTL time.Duration
	// StaleAfter is how long an item can go without anything happening to it
	// before it leaves every list but /api/stale. Zero keeps everything on
	// the board forever, which is the default: dropping work out of the main
	// view is a decision somebody has to make on purpose.
	StaleAfter time.Duration
	// DecisionPath is the file snoozes and bumps are kept in, so they survive
	// a restart. Empty keeps them in memory only, which is what the tests
	// want and what a daemon with no writable state directory falls back to.
	DecisionPath string
	// Now is the clock everything here reads: TTLs, staleness and the moment
	// a snooze lapses. Nil means time.Now.
	Now func() time.Time
}

// Board is the whole live list plus the counts that go beside it.
type Board struct {
	Items     []Item
	Attention int
	Stale     int
}

// Store holds every live item in memory. Adapters own a whole source and
// replace it wholesale on each poll; event producers put one item at a time.
type Store struct {
	mu    sync.Mutex
	items map[string]Item
	// decisions is item id to the standing instruction a human left about it.
	// Separate from items because it outlives them: an adapter rewrites its
	// whole source every poll, and a restart empties the map entirely.
	decisions  map[string]Decision
	generation uint64
	writeMu    sync.Mutex
	written    uint64
	cfg        StoreConfig
	log        *slog.Logger
	now        func() time.Time
}

// NewStore returns a store holding the decisions an earlier run left behind.
//
// A decisions file that cannot be read costs a warning rather than the daemon.
// The file is machine-written, the worst case is a few snoozes to make again,
// and a dashboard that will not start because of them is the more expensive
// failure.
func NewStore(log *slog.Logger, cfg StoreConfig) *Store {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	decisions, err := loadDecisions(cfg.DecisionPath, now())
	if err != nil {
		log.Warn("decisions not restored", "path", cfg.DecisionPath, "error", err)
		decisions = map[string]Decision{}
	} else if len(decisions) > 0 {
		log.Info("decisions restored", "path", cfg.DecisionPath, "count", len(decisions))
	}
	return &Store{
		items:     make(map[string]Item),
		decisions: decisions,
		cfg:       cfg,
		log:       log,
		now:       now,
	}
}

// ReplaceSource swaps the full set of items owned by one adapter. Items the
// adapter no longer reports are dropped.
func (s *Store) ReplaceSource(source string, items []Item) {
	s.mu.Lock()
	defer s.mu.Unlock()

	seen := make(map[string]bool, len(items))
	for _, item := range items {
		seen[item.ID] = true
		s.promote(&item)
		prev, existed := s.items[item.ID]
		if existed && !changed(prev, item) {
			// Nothing a human would notice changed, so keep the original
			// timestamp and let "updated 12m ago" stay meaningful.
			item.UpdatedAt = prev.UpdatedAt
		} else if item.UpdatedAt.IsZero() {
			item.UpdatedAt = s.now()
		}
		s.items[item.ID] = item
		s.logChange(prev, existed, item)
	}

	for id, item := range s.items {
		if item.Source != source || seen[id] {
			continue
		}
		delete(s.items, id)
		s.log.Info("item removed",
			"item_id", id, "source", item.Source, "state", string(item.State))
	}
}

// Put inserts or updates a single item and returns what was stored.
func (s *Store) Put(item Item) Item {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.promote(&item)
	now := s.now()
	if item.UpdatedAt.IsZero() {
		item.UpdatedAt = now
	}
	if s.cfg.EventTTL > 0 && item.State.Terminal() {
		item.expiresAt = now.Add(s.cfg.EventTTL)
	}

	prev, existed := s.items[item.ID]
	s.items[item.ID] = item
	s.logChange(prev, existed, item)
	return item
}

// Items returns every live item, most urgent first, each carrying whatever a
// human decided about it. Stale items are in here too: a consumer that wants
// the board without them filters on Item.Stale, and /api/stale is the list
// that has nothing else.
func (s *Store) Items() []Item {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweep()
	return s.collect(nil)
}

// Attention returns the subset of items that need a human now: work in an
// attention state, minus anything snoozed, stale, or done for longer than
// DoneTTL.
func (s *Store) Attention() []Item {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweep()
	return s.collect(s.queued())
}

// Board returns the live items and the counts that go beside them.
func (s *Store) Board() Board {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweep()

	all, keep := s.collect(nil), s.queued()
	board := Board{Items: make([]Item, 0, len(all))}
	for _, item := range all {
		if item.Stale {
			board.Stale++
			continue
		}
		if keep(item) {
			board.Attention++
		}
		board.Items = append(board.Items, item)
	}
	return board
}

// queued assumes the lock is held.
func (s *Store) queued() func(Item) bool {
	cutoff := time.Time{}
	if s.cfg.DoneTTL > 0 {
		cutoff = s.now().Add(-s.cfg.DoneTTL)
	}
	return func(i Item) bool {
		if !i.State.NeedsAttention() || i.Snoozed || i.Stale {
			return false
		}
		return !(i.State == StateDone && !cutoff.IsZero() && i.UpdatedAt.Before(cutoff))
	}
}

// Decide records what a human said about one item and returns the item as it
// now reads.
//
// The item has to exist: a decision is made about a label, and the label comes
// from the item. Snoozing something the daemon has never seen would be storing
// an instruction with nothing to check it against.
func (s *Store) Decide(key string, kind DecisionKind, until time.Time) (Item, error) {
	s.mu.Lock()
	item, ok := s.items[key]
	if !ok {
		s.mu.Unlock()
		return Item{}, fmt.Errorf("%w: %s", ErrItemMissing, key)
	}

	label, _ := item.Display()
	now := s.now()
	decision := Decision{Item: key, Kind: kind, Label: label, MadeAt: now}
	if kind == DecisionSnooze && !until.IsZero() {
		deadline := until
		decision.Until = &deadline
	}
	s.decisions[key] = decision
	s.apply(&item, now)
	s.generation++
	generation, snapshot := s.generation, s.snapshotDecisions()
	s.mu.Unlock()

	s.log.Info("decision recorded",
		"item_id", key, "decision", string(kind), "label", label,
		"until", untilWord(decision.Until), "title", item.Title)
	s.persist(generation, snapshot)
	return item, nil
}

// Clear drops the decision on one item, which is how a snooze is woken early
// and a bump is taken back.
func (s *Store) Clear(key string) (Item, error) {
	s.mu.Lock()
	item, ok := s.items[key]
	if !ok {
		s.mu.Unlock()
		return Item{}, fmt.Errorf("%w: %s", ErrItemMissing, key)
	}
	delete(s.decisions, key)
	s.apply(&item, s.now())
	s.generation++
	generation, snapshot := s.generation, s.snapshotDecisions()
	s.mu.Unlock()

	s.log.Info("decision cleared", "item_id", key, "title", item.Title)
	s.persist(generation, snapshot)
	return item, nil
}

// Decisions returns the standing instructions, for /health and for tests.
func (s *Store) Decisions() []Decision {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshotDecisions()
}

// snapshotDecisions assumes the lock is held. The file is written outside it.
func (s *Store) snapshotDecisions() []Decision {
	out := make([]Decision, 0, len(s.decisions))
	for _, decision := range s.decisions {
		out = append(out, decision)
	}
	return out
}

// persist writes the decisions out. It runs outside the lock, at the rate a
// human clicks buttons, and a failure to write is logged rather than returned:
// the snooze already applies to the running daemon, and refusing the click
// because a state directory is not writable helps nobody.
func (s *Store) persist(generation uint64, decisions []Decision) {
	if s.cfg.DecisionPath == "" {
		return
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if generation <= s.written {
		return
	}

	set := make(map[string]Decision, len(decisions))
	for _, decision := range decisions {
		set[decision.Item] = decision
	}
	if err := saveDecisions(s.cfg.DecisionPath, set); err != nil {
		s.log.Warn("decisions not saved", "path", s.cfg.DecisionPath, "error", err)
		return
	}
	s.written = generation
}

// CountBySource returns how many live items each source currently owns.
func (s *Store) CountBySource() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweep()

	counts := make(map[string]int)
	for _, item := range s.items {
		counts[item.Source]++
	}
	return counts
}

// collect assumes the lock is held.
//
// Decisions and staleness are resolved here rather than on the write path,
// because both of them turn on a clock: a snooze lapses and an item goes stale
// while nothing writes to the store at all. An event item with no poller
// behind it would otherwise stay hidden for the life of the daemon.
func (s *Store) collect(keep func(Item) bool) []Item {
	now := s.now()
	out := make([]Item, 0, len(s.items))
	for _, item := range s.items {
		s.apply(&item, now)
		if keep == nil || keep(item) {
			out = append(out, item)
		}
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Priority != out[b].Priority {
			return out[a].Priority > out[b].Priority
		}
		ra, rb := out[a].Severity.rank(), out[b].Severity.rank()
		if ra != rb {
			return ra > rb
		}
		if !out[a].UpdatedAt.Equal(out[b].UpdatedAt) {
			return out[a].UpdatedAt.After(out[b].UpdatedAt)
		}
		return out[a].ID < out[b].ID
	})
	return out
}

// sweep assumes the lock is held.
func (s *Store) sweep() {
	now := s.now()
	for id, item := range s.items {
		if item.expiresAt.IsZero() || now.Before(item.expiresAt) {
			continue
		}
		delete(s.items, id)
		s.log.Info("item expired",
			"item_id", id, "source", item.Source, "state", string(item.State))
	}
}

// promote raises an item named in TopLabels above every rank a source can give
// itself. It runs on the write path, not the read path, so the priority the API
// serves is the priority the queue was sorted by: a consumer can see why
// something is first.
func (s *Store) promote(item *Item) {
	if len(s.cfg.TopLabels) == 0 {
		return
	}
	label, _ := item.Display()
	// Folded because both sides of this comparison came from a config file
	// typed by a human.
	if s.cfg.TopLabels[strings.ToLower(strings.TrimSpace(label))] {
		item.Priority = PriorityTop
	}
}

// apply writes the human's standing decision and the staleness verdict onto a
// copy of an item. It assumes the lock is held, and it is the only place
// either one is decided, so every list agrees on both.
//
// A decision that has stopped applying is deleted here. That is not persisted:
// the file is written when somebody makes or clears a decision, and a lapsed
// one left in it is pruned on the next load or on the first poll after it.
func (s *Store) apply(item *Item, now time.Time) {
	if decision, ok := s.decisions[item.ID]; ok {
		label, _ := item.Display()
		switch {
		case decision.spent(now, label):
			delete(s.decisions, item.ID)
			s.log.Info("decision lapsed",
				"item_id", item.ID, "decision", string(decision.Kind),
				"was", decision.Label, "now", label)
		case decision.Kind == DecisionBump:
			item.Bumped = true
			item.Priority = PriorityBumped
		case decision.Kind == DecisionSnooze:
			item.Snoozed = true
			if decision.Until != nil {
				until := *decision.Until
				item.SnoozedUntil = &until
			}
		}
	}

	// A bump is a statement that this one still matters, so it answers the
	// staleness question on its own. Nothing else does: a snoozed item that
	// nobody came back to is exactly what the stale list is for.
	if s.cfg.StaleAfter > 0 && !item.Bumped {
		item.Stale = item.UpdatedAt.Before(now.Add(-s.cfg.StaleAfter))
	}
}

// untilWord is how a snooze deadline reads in a log line. An open-ended snooze
// has no deadline to print, and "0001-01-01" is not what it means.
func untilWord(until *time.Time) string {
	if until == nil {
		return "label change"
	}
	return until.UTC().Format(time.RFC3339)
}

// changed reports whether an item differs in a way a human would notice, which
// is what earns it a fresh timestamp. Priority is left out: it is a function of
// the state and the label, so it never moves on its own.
func changed(prev, next Item) bool {
	return prev.State != next.State ||
		prev.Label != next.Label ||
		prev.Title != next.Title ||
		prev.Severity != next.Severity
}

// logChange assumes the lock is held.
func (s *Store) logChange(prev Item, existed bool, next Item) {
	switch {
	case !existed:
		s.log.Info("item appeared",
			"item_id", next.ID, "source", next.Source, "state", string(next.State),
			"label", transitionWord(next), "severity", string(next.Severity), "title", next.Title)
	case prev.State != next.State || prev.Label != next.Label:
		s.log.Info("state transition",
			"item_id", next.ID, "source", next.Source,
			"from", transitionWord(prev), "to", transitionWord(next),
			"severity", string(next.Severity), "title", next.Title)
	}
}

// transitionWord is what to call an item in a log line: the label when the
// source set one, because "review requested -> ready to merge" says something
// and "needs_attention -> needs_attention" does not. A source that set no
// label gets the state's own word rather than an empty field.
func transitionWord(item Item) string {
	label, _ := item.Display()
	return label
}
