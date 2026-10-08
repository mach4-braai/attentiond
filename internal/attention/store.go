package attention

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
)

// Transition is one observed change to an item: it appeared, its state moved,
// or its label moved. Label matters on its own because a source can keep a
// state and still change what it is waiting for: "review requested" and
// "ready to merge" are both StateNeedsAttention, and moving between them is
// the change somebody wants to hear about.
type Transition struct {
	// Prev is the item as it was. It is the zero Item when Existed is false.
	Prev Item
	// Next is the item as it now is.
	Next Item
	// Existed reports whether the store already held this item. A first
	// sighting is not the same event as a change, and a daemon that has just
	// started sees everything for the first time.
	Existed bool
}

// StoreConfig tunes the store's clocks, its ordering overrides, its standing
// human decisions and its observer.
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
	// DecisionPath is the file snoozes, bumps and watches are kept in, so
	// they survive a restart. Empty keeps them in memory only, which is what
	// the tests want and what a daemon with no writable state directory
	// falls back to.
	DecisionPath string
	// Observer is called with every transition, outside the store lock, after
	// the write that produced it. It must not block.
	Observer func([]Transition)
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
	decisions map[string]Decision
	// watches is item id to a human's request that attentiond act on it,
	// kept apart from decisions so a watched item can still be snoozed.
	watches map[string]Watch
	// jobs is item id to the latest tool run on it.
	jobs       map[string]Job
	generation uint64
	writeMu    sync.Mutex
	written    uint64
	cfg        StoreConfig
	log        *slog.Logger
	now        func() time.Time
}

// NewStore returns a store holding the decisions, watches and jobs an earlier
// run left behind.
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
	saved, err := loadState(cfg.DecisionPath, now())
	if err != nil {
		log.Warn("decisions not restored", "path", cfg.DecisionPath, "error", err)
		saved = emptyState()
	} else if len(saved.decisions) > 0 || len(saved.watches) > 0 || len(saved.jobs) > 0 {
		log.Info("decisions restored", "path", cfg.DecisionPath,
			"count", len(saved.decisions), "watches", len(saved.watches), "jobs", len(saved.jobs))
	}
	return &Store{
		items:     make(map[string]Item),
		decisions: saved.decisions,
		watches:   saved.watches,
		jobs:      saved.jobs,
		cfg:       cfg,
		log:       log,
		now:       now,
	}
}

// ReplaceSource swaps the full set of items owned by one adapter. Items the
// adapter no longer reports are dropped.
func (s *Store) ReplaceSource(source string, items []Item) {
	var transitions []Transition

	s.mu.Lock()
	now := s.now()
	seen := make(map[string]bool, len(items))
	saveSeen := false
	for _, item := range items {
		saveSeen = s.see(item.ID, now) || saveSeen
		seen[item.ID] = true
		s.promote(&item)
		prev, existed := s.items[item.ID]
		if existed && !changed(prev, item) {
			// Nothing a human would notice changed, so keep the original
			// timestamp and let "updated 12m ago" stay meaningful.
			item.UpdatedAt = prev.UpdatedAt
		} else if item.UpdatedAt.IsZero() {
			item.UpdatedAt = now
		}
		s.items[item.ID] = item
		transitions = s.record(transitions, prev, existed, item)
	}

	for id, item := range s.items {
		if item.Source != source || seen[id] {
			continue
		}
		delete(s.items, id)
		s.log.Info("item removed",
			"item_id", id, "source", item.Source, "state", string(item.State))
	}
	generation, snapshot := s.stageIf(saveSeen)
	s.mu.Unlock()

	s.persist(generation, snapshot)
	s.observe(transitions)
}

// Put inserts or updates a single item and returns what was stored.
func (s *Store) Put(item Item) Item {
	var transitions []Transition

	s.mu.Lock()
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
	transitions = s.record(transitions, prev, existed, item)
	generation, snapshot := s.stageIf(s.see(item.ID, now))
	s.mu.Unlock()

	s.persist(generation, snapshot)
	s.observe(transitions)
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

// Get returns one item as every list would show it.
func (s *Store) Get(key string) (Item, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweep()
	item, ok := s.items[key]
	if ok {
		s.apply(&item, s.now())
	}
	return item, ok
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

	// The label a decision is made against is the one the human is looking
	// at, which a job may have replaced.
	shown := item
	s.overlay(&shown)
	label, _ := shown.Display()
	now := s.now()
	decision := Decision{Item: key, Kind: kind, Label: label, MadeAt: now}
	if kind == DecisionSnooze && !until.IsZero() {
		deadline := until
		decision.Until = &deadline
	}
	s.decisions[key] = decision
	s.apply(&item, now)
	generation, snapshot := s.stage()
	s.mu.Unlock()

	s.log.Info("decision recorded",
		"item_id", key, "decision", string(kind), "label", label,
		"until", untilWord(decision.Until), "title", item.Title)
	s.persist(generation, snapshot)
	return item, nil
}

// Clear drops the decision on one item, which is how a snooze is woken early
// and a bump is taken back. It also dismisses a finished job; a pending one
// stays, because its tool is still going to report.
func (s *Store) Clear(key string) (Item, error) {
	s.mu.Lock()
	item, ok := s.items[key]
	if !ok {
		s.mu.Unlock()
		return Item{}, fmt.Errorf("%w: %s", ErrItemMissing, key)
	}
	delete(s.decisions, key)
	if job, ok := s.jobs[key]; ok && !job.Pending() {
		delete(s.jobs, key)
	}
	s.apply(&item, s.now())
	generation, snapshot := s.stage()
	s.mu.Unlock()

	s.log.Info("decision cleared", "item_id", key, "title", item.Title)
	s.persist(generation, snapshot)
	return item, nil
}

// Watch asks attentiond to act on one item. Which items qualify is the
// caller's rule; the store keeps the watch and writes it to disk. Watching an
// item twice keeps the first watch.
func (s *Store) Watch(key string) (Item, error) {
	s.mu.Lock()
	item, ok := s.items[key]
	if !ok {
		s.mu.Unlock()
		return Item{}, fmt.Errorf("%w: %s", ErrItemMissing, key)
	}
	now := s.now()
	if _, watched := s.watches[key]; !watched {
		s.watches[key] = Watch{Item: key, MadeAt: now, SeenAt: now}
	}
	s.apply(&item, now)
	generation, snapshot := s.stage()
	s.mu.Unlock()

	s.log.Info("watch recorded", "item_id", key, "title", item.Title)
	s.persist(generation, snapshot)
	return item, nil
}

// Unwatch takes a watch back.
func (s *Store) Unwatch(key string) (Item, error) {
	s.mu.Lock()
	item, ok := s.items[key]
	if !ok {
		s.mu.Unlock()
		return Item{}, fmt.Errorf("%w: %s", ErrItemMissing, key)
	}
	delete(s.watches, key)
	s.apply(&item, s.now())
	generation, snapshot := s.stage()
	s.mu.Unlock()

	s.log.Info("watch cleared", "item_id", key, "title", item.Title)
	s.persist(generation, snapshot)
	return item, nil
}

// Watches returns every watch, ordered by item id.
func (s *Store) Watches() []Watch {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Watch, 0, len(s.watches))
	for _, watch := range s.watches {
		out = append(out, watch)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Item < out[b].Item })
	return out
}

// Decisions returns the standing instructions, for /health and for tests.
func (s *Store) Decisions() []Decision {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Decision, 0, len(s.decisions))
	for _, decision := range s.decisions {
		out = append(out, decision)
	}
	return out
}

// QueueJob records that a tool is waiting to run on an item, replacing the
// job the item had. The item has to exist: a tool runs on something a source
// reported.
func (s *Store) QueueJob(key, tool string) (Job, error) {
	return s.changeJob(key, true, func(job *Job, now time.Time) error {
		*job = Job{Item: key, Tool: tool, Status: JobQueued, QueuedAt: now}
		return nil
	})
}

// StartJob records that the queued job on an item is running, writing its
// output to log.
func (s *Store) StartJob(key, log string) (Job, error) {
	return s.changeJob(key, false, func(job *Job, now time.Time) error {
		if job.Status != JobQueued {
			return fmt.Errorf("%w: %s has no queued job", ErrJobMissing, key)
		}
		job.Status, job.Log, job.StartedAt = JobRunning, log, &now
		return nil
	})
}

// FinishJob records what the pending job on an item returned. The item may
// have left its source while the tool ran; the result is kept all the same.
func (s *Store) FinishJob(key, result, detail string) (Job, error) {
	return s.changeJob(key, false, func(job *Job, now time.Time) error {
		if !job.Pending() {
			return fmt.Errorf("%w: %s has no pending job", ErrJobMissing, key)
		}
		job.Status, job.Result, job.Detail, job.FinishedAt = JobFinished, result, detail, &now
		return nil
	})
}

// changeJob applies one job change, then records and observes the transition
// it makes to how the item reads. A tool finishing happens between polls, so
// waiting for the next poll to notice would hold back the notification.
func (s *Store) changeJob(key string, needItem bool, change func(job *Job, now time.Time) error) (Job, error) {
	s.mu.Lock()
	item, exists := s.items[key]
	if needItem && !exists {
		s.mu.Unlock()
		return Job{}, fmt.Errorf("%w: %s", ErrItemMissing, key)
	}
	before, had := s.jobs[key]
	job := before
	if err := change(&job, s.now()); err != nil {
		s.mu.Unlock()
		return Job{}, err
	}
	s.jobs[key] = job

	var transitions []Transition
	if exists {
		prev, next := item, item
		if had {
			overlayJob(&prev, before)
			s.promote(&prev)
		}
		s.overlay(&next)
		if prev.State != next.State || transitionWord(prev) != transitionWord(next) {
			s.log.Info("state transition",
				"item_id", key, "source", item.Source,
				"from", transitionWord(prev), "to", transitionWord(next),
				"severity", string(next.Severity), "title", item.Title)
			transitions = s.transition(transitions, prev, true, next)
		}
	}
	generation, snapshot := s.stage()
	s.mu.Unlock()

	s.log.Info("job "+string(job.Status),
		"item_id", key, "tool", job.Tool, "result", job.Result, "detail", job.Detail, "log", job.Log)
	s.persist(generation, snapshot)
	s.observe(transitions)
	return job, nil
}

// Jobs returns every job the store holds, newest first, for the runner to
// pick up what a restart interrupted.
func (s *Store) Jobs() []Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Job, 0, len(s.jobs))
	for _, job := range s.jobs {
		out = append(out, job)
	}
	sort.Slice(out, func(a, b int) bool {
		if !out[a].QueuedAt.Equal(out[b].QueuedAt) {
			return out[a].QueuedAt.After(out[b].QueuedAt)
		}
		return out[a].Item < out[b].Item
	})
	return out
}

// JobItems returns the items that carry a job, newest job first. Stale items
// are included: a tool that finished on a quiet pull request still reported.
func (s *Store) JobItems() []Item {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweep()
	out := s.collect(func(item Item) bool { return item.Job != nil })
	sort.SliceStable(out, func(a, b int) bool {
		return out[a].Job.QueuedAt.After(out[b].Job.QueuedAt)
	})
	return out
}

// see moves a watched item's SeenAt forward, and reports whether that is worth
// a write. It assumes the lock is held.
func (s *Store) see(id string, now time.Time) bool {
	watch, ok := s.watches[id]
	if !ok || now.Sub(watch.SeenAt) < seenEvery {
		return false
	}
	watch.SeenAt = now
	s.watches[id] = watch
	return true
}

// stage numbers a write and copies what it will save. It assumes the lock is
// held; the file is written outside it.
func (s *Store) stage() (uint64, state) {
	s.generation++
	saved := state{
		decisions: make(map[string]Decision, len(s.decisions)),
		watches:   make(map[string]Watch, len(s.watches)),
		jobs:      make(map[string]Job, len(s.jobs)),
	}
	for key, decision := range s.decisions {
		saved.decisions[key] = decision
	}
	for key, watch := range s.watches {
		saved.watches[key] = watch
	}
	for key, job := range s.jobs {
		saved.jobs[key] = job
	}
	return s.generation, saved
}

// stageIf is stage for a write path that usually has nothing to save. A zero
// generation tells persist to skip.
func (s *Store) stageIf(dirty bool) (uint64, state) {
	if !dirty {
		return 0, state{}
	}
	return s.stage()
}

// persist writes the decisions out. It runs outside the lock, at the rate a
// human clicks buttons, and a failure to write is logged rather than returned:
// the snooze already applies to the running daemon, and refusing the click
// because a state directory is not writable helps nobody.
func (s *Store) persist(generation uint64, saved state) {
	if s.cfg.DecisionPath == "" || generation == 0 {
		return
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if generation <= s.written {
		return
	}

	if err := saveState(s.cfg.DecisionPath, saved); err != nil {
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
	// Folded the same way the notifier folds its own label list: both sides of
	// this comparison came from a config file typed by a human.
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
	// The job goes first, so a snooze is judged against the label the human
	// sees. A tool that gives up changes that label, which ends the snooze.
	s.overlay(item)

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

	_, item.Watched = s.watches[item.ID]

	// A bump is a statement that this one still matters, so it answers the
	// staleness question on its own. Nothing else does: a snoozed item that
	// nobody came back to is exactly what the stale list is for.
	if s.cfg.StaleAfter > 0 && !item.Bumped {
		item.Stale = item.UpdatedAt.Before(now.Add(-s.cfg.StaleAfter))
	}
}

// overlay writes an item's job onto a copy of it. It assumes the lock is held.
func (s *Store) overlay(item *Item) {
	job, ok := s.jobs[item.ID]
	if !ok {
		return
	}
	overlayJob(item, job)
	s.promote(item)
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
// is what earns it a fresh timestamp and a transition. Priority is left out: it
// is a function of the state and the label, so it never moves on its own.
func changed(prev, next Item) bool {
	return prev.State != next.State ||
		prev.Label != next.Label ||
		prev.Title != next.Title ||
		prev.Severity != next.Severity
}

// record logs a change and appends it to transitions. It assumes the lock is
// held, and allocates nothing while a poll finds everything as it left it.
//
// The transition's copy of the item is marked snoozed when a live snooze
// covers it, so the notifier can stay quiet about work a human already
// deferred. Only the copy: applying a decision to the stored item would
// overwrite the priority a source computed, and a snooze that lapses could
// never give it back. Both copies carry the item's job for the same reason.
func (s *Store) record(transitions []Transition, prev Item, existed bool, next Item) []Transition {
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
	default:
		return transitions
	}
	if existed {
		s.overlay(&prev)
	}
	s.overlay(&next)
	return s.transition(transitions, prev, existed, next)
}

// transition appends one change, marking the copy snoozed when a live snooze
// covers how it now reads. It assumes the lock is held.
func (s *Store) transition(transitions []Transition, prev Item, existed bool, next Item) []Transition {
	if decision, ok := s.decisions[next.ID]; ok && decision.Kind == DecisionSnooze {
		if !decision.spent(s.now(), transitionWord(next)) {
			next.Snoozed = true
		}
	}
	return append(transitions, Transition{Prev: prev, Next: next, Existed: existed})
}

// transitionWord is what to call an item in a log line: the label when the
// source set one, because "review requested -> ready to merge" says something
// and "needs_attention -> needs_attention" does not. A source that set no
// label gets the state's own word rather than an empty field.
func transitionWord(item Item) string {
	label, _ := item.Display()
	return label
}

// observe hands transitions to the observer outside the lock. The observer
// reaches another process, so calling it while holding the lock would stall
// every poll and every HTTP read behind a socket dial.
func (s *Store) observe(transitions []Transition) {
	if len(transitions) == 0 || s.cfg.Observer == nil {
		return
	}
	s.cfg.Observer(transitions)
}
