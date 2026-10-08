package attention

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// DecisionKind is what a human said about one item, as opposed to what a
// source observed about it.
type DecisionKind string

const (
	// DecisionSnooze hides an item from the attention queue. It keeps its
	// place on the board, because hiding work from every view is how you lose
	// it.
	DecisionSnooze DecisionKind = "snooze"
	// DecisionBump puts an item above everything else, including a label
	// named in top_labels. Both are judgements rather than properties of the
	// work; this one is about one item instead of a class of them, so it wins.
	DecisionBump DecisionKind = "bump"
)

// ParseDecisionKind validates a decision received over the wire.
func ParseDecisionKind(s string) (DecisionKind, error) {
	switch DecisionKind(s) {
	case DecisionSnooze, DecisionBump:
		return DecisionKind(s), nil
	}
	return "", fmt.Errorf("unknown decision %q", s)
}

// Decision is a standing instruction about one item, outliving the polls that
// rewrite it and the restarts that empty the store.
//
// It is the only state in the daemon no source can rebuild. A pull request's
// mergeability comes back from GitHub within a minute of a restart; the fact
// that you decided to leave it until Monday exists nowhere else.
type Decision struct {
	// Item is the globally unique item id, as built by Key.
	Item string       `json:"item"`
	Kind DecisionKind `json:"kind"`
	// Label is the word the item carried when the decision was made.
	//
	// A snooze answers a question about that word ("do I care about this
	// review today"), so it stops applying when the word changes: a pull
	// request snoozed as "review requested" that becomes "ready to merge" is
	// new information, and hiding it would be answering a question nobody
	// asked. A bump ignores this, because "this one matters to me" survives
	// the work moving on.
	Label string `json:"label"`
	// Until is when a snooze lapses. Absent means it lasts until the label
	// changes, which is the open-ended form: hide this until something
	// actually happens to it. A bump never has one.
	Until  *time.Time `json:"until,omitempty"`
	MadeAt time.Time  `json:"made_at"`
}

// spent reports whether a decision has stopped applying, either because its
// clock ran out or because the item it was made about now reads differently.
func (d Decision) spent(now time.Time, label string) bool {
	if d.Kind == DecisionSnooze {
		if d.Until != nil && !now.Before(*d.Until) {
			return true
		}
		return !strings.EqualFold(strings.TrimSpace(d.Label), strings.TrimSpace(label))
	}
	return false
}

// Watch is a human asking attentiond to act on one item: run tools on a pull
// request when it falls into conflict or collects review comments. It is kept
// beside decisions rather than as one, because a watched pull request can also
// be snoozed or bumped.
type Watch struct {
	Item   string    `json:"item"`
	MadeAt time.Time `json:"made_at"`
	// SeenAt is when a source last reported the item, to within seenEvery.
	SeenAt time.Time `json:"seen_at"`
}

// watchTTL is how long a watched item can go unreported before its watch is
// dropped at load. GitHub only reports open pull requests, so this is how a
// watch ends once its pull request merges or closes.
const watchTTL = 14 * 24 * time.Hour

// seenEvery is how far SeenAt may lag behind the last poll. Writing the file on
// every poll to move a timestamp measured in weeks would be churn.
const seenEvery = 24 * time.Hour

// state is everything the store keeps on disk.
type state struct {
	decisions map[string]Decision
	watches   map[string]Watch
	jobs      map[string]Job
}

func emptyState() state {
	return state{decisions: map[string]Decision{}, watches: map[string]Watch{}, jobs: map[string]Job{}}
}

// decisionFile is the on-disk form: a version and a list, so a future field
// arrives without guessing at what an older file meant. Version 1 had no
// watches or jobs and still loads.
type decisionFile struct {
	Version   int        `json:"version"`
	Decisions []Decision `json:"decisions"`
	Watches   []Watch    `json:"watches,omitempty"`
	Jobs      []Job      `json:"jobs,omitempty"`
}

const decisionFileVersion = 2

// loadState reads the decisions and watches written by an earlier run. A
// missing file is an empty set: no decisions yet is the normal state of a new
// machine.
//
// Expired snoozes are dropped here rather than being carried in memory for the
// life of the process. Decisions about items that never come back cost one map
// entry each and are cleared the first time their item is seen again, so they
// are left alone: a pull request absent from one poll because GitHub timed out
// is not a reason to forget you deferred it. A watch is the exception, dropped
// once its item has been gone for watchTTL, and so is a finished job that old.
// A job still queued or running is kept: the runner starts it again.
func loadState(path string, now time.Time) (state, error) {
	out := emptyState()
	if path == "" {
		return out, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return state{}, err
	}

	var file decisionFile
	if err := json.Unmarshal(data, &file); err != nil {
		return state{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if file.Version != 1 && file.Version != decisionFileVersion {
		return state{}, fmt.Errorf("%s: version %d, want 1 or %d", path, file.Version, decisionFileVersion)
	}

	for _, decision := range file.Decisions {
		if decision.Item == "" {
			continue
		}
		if _, err := ParseDecisionKind(string(decision.Kind)); err != nil {
			continue
		}
		if decision.Kind == DecisionSnooze && decision.Until != nil && !now.Before(*decision.Until) {
			continue
		}
		out.decisions[decision.Item] = decision
	}
	for _, watch := range file.Watches {
		if watch.Item == "" || now.Sub(watch.SeenAt) > watchTTL {
			continue
		}
		out.watches[watch.Item] = watch
	}
	for _, job := range file.Jobs {
		if job.Item == "" || (!job.Pending() && now.Sub(job.latest()) > watchTTL) {
			continue
		}
		out.jobs[job.Item] = job
	}
	return out, nil
}

// saveState writes the set through a temporary file and a rename, so a crash
// mid-write leaves the previous file rather than half of this one.
func saveState(path string, saved state) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	file := decisionFile{Version: decisionFileVersion, Decisions: make([]Decision, 0, len(saved.decisions))}
	for _, decision := range saved.decisions {
		file.Decisions = append(file.Decisions, decision)
	}
	for _, watch := range saved.watches {
		file.Watches = append(file.Watches, watch)
	}
	for _, job := range saved.jobs {
		file.Jobs = append(file.Jobs, job)
	}
	// Sorted so that a file a human opens reads the same way twice, and so a
	// diff of it shows what changed rather than what moved.
	sort.Slice(file.Decisions, func(a, b int) bool {
		return file.Decisions[a].Item < file.Decisions[b].Item
	})
	sort.Slice(file.Watches, func(a, b int) bool {
		return file.Watches[a].Item < file.Watches[b].Item
	})
	sort.Slice(file.Jobs, func(a, b int) bool {
		return file.Jobs[a].Item < file.Jobs[b].Item
	})

	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	temp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	name := temp.Name()
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		os.Remove(name)
		return err
	}
	if err := temp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// DefaultDecisionPath is where decisions live when the file does not name a
// path: beside the pid and the log that attn-daemon already writes.
func DefaultDecisionPath() string {
	if dir := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); dir != "" {
		return filepath.Join(dir, "attentiond", "decisions.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "state", "attentiond", "decisions.json")
}
