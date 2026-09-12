// Package attention holds the source-agnostic model of work that may need a
// human's attention, plus the in-memory store that adapters write into.
package attention

import (
	"encoding/json"
	"fmt"
	"time"
)

// State is the lifecycle position of a piece of work.
type State string

const (
	StateWorking        State = "working"
	StateWaiting        State = "waiting"
	StateNeedsAttention State = "needs_attention"
	StateDone           State = "done"
	StateFailed         State = "failed"
)

// ParseState validates a state received over the wire.
func ParseState(s string) (State, error) {
	switch State(s) {
	case StateWorking, StateWaiting, StateNeedsAttention, StateDone, StateFailed:
		return State(s), nil
	}
	return "", fmt.Errorf("unknown state %q", s)
}

// NeedsAttention reports whether work in this state belongs in the attention
// queue. Done counts: finished work nobody has looked at yet is the whole point
// of the daemon, and adapters clear it once the human has seen it.
func (s State) NeedsAttention() bool {
	switch s {
	case StateNeedsAttention, StateFailed, StateDone:
		return true
	}
	return false
}

// Terminal reports whether more progress is expected.
func (s State) Terminal() bool {
	return s == StateDone || s == StateFailed
}

// Severity ranks how loudly an item should ask for attention.
type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityWarning  Severity = "warning"
	SeverityCritical Severity = "critical"
)

// ParseSeverity validates a severity received over the wire.
func ParseSeverity(s string) (Severity, error) {
	switch Severity(s) {
	case SeverityInfo, SeverityWarning, SeverityCritical:
		return Severity(s), nil
	}
	return "", fmt.Errorf("unknown severity %q", s)
}

func (s Severity) rank() int {
	switch s {
	case SeverityCritical:
		return 2
	case SeverityWarning:
		return 1
	default:
		return 0
	}
}

// Action is a local endpoint that sends the human back into the execution
// context the item came from.
type Action struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Method string `json:"method"`
	Href   string `json:"href"`
}

// Item is one piece of work, normalized away from whatever produced it.
// Source-specific identifiers live in Context so that consumers can round-trip
// back to the originating tool without the core model knowing about it.
type Item struct {
	ID        string            `json:"id"`
	Source    string            `json:"source"`
	Title     string            `json:"title"`
	State     State             `json:"state"`
	Severity  Severity          `json:"severity"`
	Context   map[string]string `json:"context,omitempty"`
	UpdatedAt time.Time         `json:"updated_at"`
	Actions   []Action          `json:"actions,omitempty"`

	// expiresAt is set for terminal event items so a long-lived daemon does
	// not accumulate finished builds. Adapter-owned items leave it zero.
	expiresAt time.Time
}

// MarshalJSON adds the derived attention flag so consumers do not have to
// restate the state rules.
func (i Item) MarshalJSON() ([]byte, error) {
	type item Item
	return json.Marshal(struct {
		item
		Attention bool `json:"attention"`
	}{item(i), i.State.NeedsAttention()})
}

// Key builds the globally unique item id for a source-local id.
func Key(source, id string) string {
	return source + ":" + id
}

// SourceStatus is the health of one adapter, reported through /health.
type SourceStatus struct {
	Mode        string     `json:"mode"`
	Healthy     bool       `json:"healthy"`
	Items       int        `json:"items"`
	LastSuccess *time.Time `json:"last_success,omitempty"`
	LastError   string     `json:"last_error,omitempty"`
}
