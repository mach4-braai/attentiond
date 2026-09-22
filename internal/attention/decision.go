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

// decisionFile is the on-disk form: a version and a list, so a future field
// arrives without guessing at what an older file meant.
type decisionFile struct {
	Version   int        `json:"version"`
	Decisions []Decision `json:"decisions"`
}

const decisionFileVersion = 1

// loadDecisions reads the decisions written by an earlier run. A missing file
// is an empty set: no decisions yet is the normal state of a new machine.
//
// Expired snoozes are dropped here rather than being carried in memory for the
// life of the process. Decisions about items that never come back cost one map
// entry each and are cleared the first time their item is seen again, so they
// are left alone: a pull request absent from one poll because GitHub timed out
// is not a reason to forget you deferred it.
func loadDecisions(path string, now time.Time) (map[string]Decision, error) {
	if path == "" {
		return map[string]Decision{}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]Decision{}, nil
		}
		return nil, err
	}

	var file decisionFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if file.Version != decisionFileVersion {
		return nil, fmt.Errorf("%s: version %d, want %d", path, file.Version, decisionFileVersion)
	}

	out := make(map[string]Decision, len(file.Decisions))
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
		out[decision.Item] = decision
	}
	return out, nil
}

// saveDecisions writes the set through a temporary file and a rename, so a
// crash mid-write leaves the previous file rather than half of this one.
func saveDecisions(path string, decisions map[string]Decision) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	file := decisionFile{Version: decisionFileVersion, Decisions: make([]Decision, 0, len(decisions))}
	for _, decision := range decisions {
		file.Decisions = append(file.Decisions, decision)
	}
	// Sorted so that a file a human opens reads the same way twice, and so a
	// diff of it shows what changed rather than what moved.
	sort.Slice(file.Decisions, func(a, b int) bool {
		return file.Decisions[a].Item < file.Decisions[b].Item
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
