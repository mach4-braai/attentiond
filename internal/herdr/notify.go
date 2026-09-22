package herdr

import (
	"context"
	"fmt"

	"github.com/devanmcgeer/attentiond/internal/notify"
)

// Notifier shows a notification through a running Herdr server.
//
// Herdr is the delivery route rather than a macOS API call because it already
// owns this decision: its [ui.toast] delivery setting chooses between an in-app
// toast and a system notification, and it knows whether the human is looking at
// the terminal right now. attentiond deciding that for itself would mean a
// second, conflicting answer to the same question, and a dependency on
// osascript or terminal-notifier that only works on one operating system.
type Notifier struct {
	client  *Client
	fixture bool
}

// NewNotifier returns the Herdr notification sender. When fixture is true it
// refuses to send, because a daemon replaying a recorded snapshot is describing
// panes that are not there and should not interrupt anybody about them.
func NewNotifier(client *Client, fixture bool) *Notifier {
	return &Notifier{client: client, fixture: fixture}
}

// Notify implements notify.Sender.
func (n *Notifier) Notify(ctx context.Context, notification notify.Notification) error {
	if n.fixture {
		return fmt.Errorf("herdr: %w", notify.ErrUnavailable)
	}

	params := map[string]string{
		"title": notification.Title,
		"body":  notification.Body,
		"sound": notification.Sound,
	}
	return n.client.Call(ctx, "notification.show", params, nil)
}
