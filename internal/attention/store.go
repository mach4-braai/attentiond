package attention

import (
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
)

// StoreConfig tunes the store's clocks and its ordering overrides.
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
	// Now is the clock everything here reads. Nil means time.Now.
	Now func() time.Time
}

// Store holds every live item in memory. Adapters own a whole source and
// replace it wholesale on each poll; event producers put one item at a time.
type Store struct {
	mu    sync.Mutex
	items map[string]Item
	cfg   StoreConfig
	log   *slog.Logger
	now   func() time.Time
}

// NewStore returns an empty store.
func NewStore(log *slog.Logger, cfg StoreConfig) *Store {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Store{
		items: make(map[string]Item),
		cfg:   cfg,
		log:   log,
		now:   now,
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

// Items returns every live item, most urgent first.
func (s *Store) Items() []Item {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweep()
	return s.collect(nil)
}

// Attention returns the subset of items that need a human.
func (s *Store) Attention() []Item {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweep()
	return s.collect(func(i Item) bool { return i.State.NeedsAttention() })
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
func (s *Store) collect(keep func(Item) bool) []Item {
	out := make([]Item, 0, len(s.items))
	for _, item := range s.items {
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
