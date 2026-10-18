package attention

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// The kinds of change the signal log records.
const (
	SignalAppeared    = "appeared"
	SignalChanged     = "changed"
	SignalDisappeared = "disappeared"
)

// pruneEvery is the longest the log goes between rewrites that drop entries
// past retention.
const pruneEvery = time.Hour

// SignalEntry is one line of the signal log: an item appeared, moved to a new
// state or label, or went away.
//
// State, Label and Severity describe the item after the change. For a
// disappearance that is the last thing the store knew about it. PrevState and
// PrevLabel are set only on a change.
type SignalEntry struct {
	At        time.Time `json:"at"`
	Kind      string    `json:"kind"`
	ItemID    string    `json:"item_id"`
	Source    string    `json:"source"`
	Title     string    `json:"title"`
	State     string    `json:"state"`
	Label     string    `json:"label"`
	Severity  string    `json:"severity"`
	PrevState string    `json:"prev_state,omitempty"`
	PrevLabel string    `json:"prev_label,omitempty"`
}

// signalEntry describes an item at one moment, without a previous reading.
func signalEntry(at time.Time, kind string, item Item) SignalEntry {
	return SignalEntry{
		At:       at.UTC(),
		Kind:     kind,
		ItemID:   item.ID,
		Source:   item.Source,
		Title:    item.Title,
		State:    string(item.State),
		Label:    transitionWord(item),
		Severity: string(item.Severity),
	}
}

// signalLog is the append-only JSON Lines file behind GET /api/log. It opens
// the file for every batch instead of holding a handle, so the rename that
// ends a prune never leaves a writer attached to the replaced file.
type signalLog struct {
	path      string
	retention time.Duration
	now       func() time.Time

	mu        sync.Mutex
	lastPrune time.Time
}

// append adds entries to the end of the file, after dropping what retention
// has expired when the last prune was an hour ago or more.
func (l *signalLog) append(entries []SignalEntry) error {
	if l == nil || len(entries) == 0 {
		return nil
	}
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	for _, entry := range entries {
		if err := encoder.Encode(entry); err != nil {
			return err
		}
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return err
	}
	var pruneErr error
	if now := l.now(); l.retention > 0 && now.Sub(l.lastPrune) >= pruneEvery {
		l.lastPrune = now
		pruneErr = l.prune(now)
	}
	file, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := file.Write(buf.Bytes()); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return pruneErr
}

// prune rewrites the file without entries older than retention, through a
// temporary file and a rename. It leaves the file alone when nothing is old
// enough to drop. The caller holds l.mu.
func (l *signalLog) prune(now time.Time) error {
	entries, err := readSignals(l.path)
	if err != nil {
		return err
	}
	cutoff := now.Add(-l.retention)
	var keep bytes.Buffer
	encoder := json.NewEncoder(&keep)
	dropped := false
	for _, entry := range entries {
		if entry.At.Before(cutoff) {
			dropped = true
			continue
		}
		if err := encoder.Encode(entry); err != nil {
			return err
		}
	}
	if !dropped {
		return nil
	}

	temp, err := os.CreateTemp(filepath.Dir(l.path), filepath.Base(l.path)+".*")
	if err != nil {
		return err
	}
	name := temp.Name()
	if _, err := temp.Write(keep.Bytes()); err != nil {
		temp.Close()
		os.Remove(name)
		return err
	}
	if err := temp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, l.path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// read returns entries at or after since, newest first, at most limit of them.
// A zero since matches everything.
func (l *signalLog) read(since time.Time, limit int) ([]SignalEntry, error) {
	out := []SignalEntry{}
	if l == nil {
		return out, nil
	}
	l.mu.Lock()
	entries, err := readSignals(l.path)
	l.mu.Unlock()
	if err != nil {
		return nil, err
	}

	// File order is the order the store wrote in, so walking backwards gives
	// newest first, and the stable sort keeps that for equal timestamps.
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].At.Before(since) {
			continue
		}
		out = append(out, entries[i])
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].At.After(out[b].At) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// readSignals parses the whole file in file order. A missing file is an empty
// log, and a line that does not parse is skipped: a crash can leave a torn last
// line, and one bad line should not hide the rest.
func readSignals(path string) ([]SignalEntry, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer file.Close()

	var entries []SignalEntry
	reader := bufio.NewReader(file)
	for {
		line, err := reader.ReadBytes('\n')
		if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
			var entry SignalEntry
			if json.Unmarshal(trimmed, &entry) == nil && entry.Kind != "" {
				entries = append(entries, entry)
			}
		}
		if err != nil {
			break
		}
	}
	return entries, nil
}

// DefaultSignalLogPath is where the signal log lives when the file does not
// name a path: beside the decisions file.
func DefaultSignalLogPath() string {
	dir := stateDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "signals.jsonl")
}

// stateDir is the directory attentiond keeps its own files in.
func stateDir() string {
	if dir := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); dir != "" {
		return filepath.Join(dir, "attentiond")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "state", "attentiond")
}

// Signals returns the signal log, newest first. since is inclusive and a zero
// value matches everything; limit caps the count, and zero means no cap.
func (s *Store) Signals(since time.Time, limit int) ([]SignalEntry, error) {
	entries, err := s.signals.read(since, limit)
	if err != nil {
		return nil, fmt.Errorf("read signal log: %w", err)
	}
	return entries, nil
}

// lastByItem maps each item id in the file to its newest entry. File order is
// write order, so the last line for an id wins.
func (l *signalLog) lastByItem() (map[string]SignalEntry, error) {
	out := map[string]SignalEntry{}
	l.mu.Lock()
	entries, err := readSignals(l.path)
	l.mu.Unlock()
	for _, entry := range entries {
		out[entry.ItemID] = entry
	}
	return out, err
}
