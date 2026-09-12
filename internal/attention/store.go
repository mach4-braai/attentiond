package attention

import (
	"log/slog"
	"sort"
	"sync"
	"time"
)

// Store holds every live item in memory. Adapters own a whole source and
// replace it wholesale on each poll; event producers put one item at a time.
type Store struct {
	mu       sync.Mutex
	items    map[string]Item
	eventTTL time.Duration
	log      *slog.Logger
	now      func() time.Time
}

// NewStore returns an empty store. eventTTL bounds how long terminal items
// written by Put survive; zero keeps them forever.
func NewStore(log *slog.Logger, eventTTL time.Duration) *Store {
	return &Store{
		items:    make(map[string]Item),
		eventTTL: eventTTL,
		log:      log,
		now:      time.Now,
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
		prev, existed := s.items[item.ID]
		if existed && prev.State == item.State && prev.Title == item.Title && prev.Severity == item.Severity {
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

	now := s.now()
	if item.UpdatedAt.IsZero() {
		item.UpdatedAt = now
	}
	if s.eventTTL > 0 && item.State.Terminal() {
		item.expiresAt = now.Add(s.eventTTL)
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

// logChange assumes the lock is held.
func (s *Store) logChange(prev Item, existed bool, next Item) {
	switch {
	case !existed:
		s.log.Info("item appeared",
			"item_id", next.ID, "source", next.Source, "state", string(next.State),
			"severity", string(next.Severity), "title", next.Title)
	case prev.State != next.State:
		s.log.Info("state transition",
			"item_id", next.ID, "source", next.Source,
			"from", string(prev.State), "to", string(next.State),
			"severity", string(next.Severity), "title", next.Title)
	}
}
