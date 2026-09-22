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

// Tone is how an item should read on a surface that has colour: a closed set,
// so a consumer maps six values onto its palette instead of restating what
// every state and every source-specific label means.
type Tone string

const (
	// ToneReady is one action away from finished. It is the loudest tone on
	// purpose: a pull request that only needs the merge button is the cheapest
	// thing on the queue to clear.
	ToneReady Tone = "ready"
	// ToneAttention wants a human now.
	ToneAttention Tone = "attention"
	// ToneFailed is broken.
	ToneFailed Tone = "failed"
	// ToneActive is making progress on its own.
	ToneActive Tone = "active"
	// ToneDone is finished and unread.
	ToneDone Tone = "done"
	// ToneNeutral is somebody else's turn.
	ToneNeutral Tone = "neutral"
)

// Priority is the order the queue is read in, which is not the same question as
// how loud an item is. Severity answers "how bad", and almost everything
// actionable is a warning, so sorting on severity alone leaves the queue in
// recency order. These ranks say what to clear first.
//
// Sources that know better override them: see the GitHub and calendar
// adapters. The gaps are deliberate, so a source can slot something between
// two ranks without renumbering the rest.
const (
	// PriorityBumped is an item a human put at the top by hand. It is above
	// PriorityTop because it is the narrower statement of the same kind:
	// top_labels promotes a class of work in a config file written once,
	// this one promotes the item in front of you today.
	PriorityBumped = 110
	// PriorityTop is reserved for labels named in [attention] top_labels. No
	// source sets it: it is the one rank that comes from configuration rather
	// than from what the work is, for the case where you know something
	// outranks the whole table. A held OpenTofu state lock is the example: it
	// costs the rest of the team, not only you.
	PriorityTop = 100
	// PriorityDeadline is for work that expires if ignored. A meeting is the
	// only item in this stack that stops being actionable once it has passed,
	// so it outranks everything a source can rank for itself.
	PriorityDeadline = 50
	// PriorityOneClick is finished work waiting on a single action.
	PriorityOneClick = 40
	// PriorityBlockingOthers is somebody else waiting on you in a repository
	// you have named as important.
	PriorityBlockingOthers = 30
	// PriorityAsked is somebody else waiting on you anywhere else.
	PriorityAsked = 20
	// PriorityActionable is the default for work that wants you: a blocked
	// agent, a failed command, a pull request that needs a rebase.
	PriorityActionable = 10
	// PriorityUnread is finished work nobody has looked at. It belongs in the
	// queue, below everything that still needs doing.
	PriorityUnread = 5
	// PriorityBackground is work that is somebody else's turn or is running by
	// itself.
	PriorityBackground = 0
)

// DefaultDisplay is the label and tone for a state, for sources with no better
// word of their own. A source that has one (a pull request is "ready to merge",
// not "needs you") sets Label and Tone itself.
func DefaultDisplay(s State) (string, Tone) {
	switch s {
	case StateWorking:
		return "working", ToneActive
	case StateWaiting:
		return "waiting", ToneNeutral
	case StateNeedsAttention:
		return "needs you", ToneAttention
	case StateDone:
		return "done", ToneDone
	case StateFailed:
		return "failed", ToneFailed
	}
	return string(s), ToneNeutral
}

// DefaultPriority ranks a state for sources with nothing more specific to say.
func DefaultPriority(s State) int {
	switch s {
	case StateNeedsAttention, StateFailed:
		return PriorityActionable
	case StateDone:
		return PriorityUnread
	}
	return PriorityBackground
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
	ID       string   `json:"id"`
	Source   string   `json:"source"`
	Title    string   `json:"title"`
	State    State    `json:"state"`
	Severity Severity `json:"severity"`

	// Label is the word a human uses for why this item is here, which is not
	// always the lifecycle state: a pull request is "ready to merge" or
	// "rebase required", both of which are StateNeedsAttention. Sources set
	// it; an empty one falls back to the state's own word when marshalled.
	//
	// It is also the field a change is judged against. Two labels sharing a
	// state is exactly the transition worth telling somebody about.
	Label string `json:"label"`
	// Tone is how Label should read on a surface with colour.
	Tone Tone `json:"tone"`
	// Priority is the order to clear the queue in, highest first.
	Priority int `json:"priority"`

	Context   map[string]string `json:"context,omitempty"`
	UpdatedAt time.Time         `json:"updated_at"`
	Actions   []Action          `json:"actions,omitempty"`

	// Snoozed reports that a human deferred this item. It leaves the
	// attention queue and keeps its place on the board, because work that
	// disappears from every view is work you have lost rather than deferred.
	Snoozed bool `json:"snoozed,omitempty"`
	// SnoozedUntil is when it comes back. Absent on a snooze that lasts until
	// the item's label changes.
	SnoozedUntil *time.Time `json:"snoozed_until,omitempty"`
	// Bumped reports that a human raised this item to PriorityBumped.
	Bumped bool `json:"bumped,omitempty"`
	// Stale reports that nothing has happened to this item for longer than
	// [attention] stale_after. It is served by /api/stale and by nothing
	// else: a month-old pull request is archaeology, and leaving it on the
	// board teaches you to skim the board.
	Stale bool `json:"stale,omitempty"`

	// expiresAt is set for terminal event items so a long-lived daemon does
	// not accumulate finished builds. Adapter-owned items leave it zero.
	expiresAt time.Time
}

// Display fills in the label and tone a source left unset, so every item that
// leaves the daemon carries both.
func (i Item) Display() (string, Tone) {
	label, tone := DefaultDisplay(i.State)
	if i.Label != "" {
		label = i.Label
	}
	if i.Tone != "" {
		tone = i.Tone
	}
	return label, tone
}

// MarshalJSON adds the derived attention flag so consumers do not have to
// restate the state rules, and completes the display fields so they are never
// empty on the wire.
func (i Item) MarshalJSON() ([]byte, error) {
	i.Label, i.Tone = i.Display()
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
	// Warning is set when a poll succeeded but the result is known to be
	// incomplete. Healthy stays true: the adapter works, the answer does not
	// cover everything.
	Warning string `json:"warning,omitempty"`
}
