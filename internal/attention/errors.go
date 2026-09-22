package attention

import "errors"

// Action failures an adapter can report. They live here, next to Action, so
// that adapters and transports agree on what went wrong without either one
// importing the other.
var (
	// ErrActionUnsupported means the adapter has no such action.
	ErrActionUnsupported = errors.New("action not supported")
	// ErrActionTargetMissing means the target no longer exists.
	ErrActionTargetMissing = errors.New("action target not found")
	// ErrActionUnavailable means the adapter cannot reach whatever executes
	// the action right now.
	ErrActionUnavailable = errors.New("action executor unavailable")
)

// ErrItemMissing means the store has no item with that id. A decision is made
// about a live item, so this is the answer to snoozing something that has
// already been merged, closed or dropped by its source.
var ErrItemMissing = errors.New("item not found")
