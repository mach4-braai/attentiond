// Package notify decides which changes are worth interrupting a human for and
// hands them to a delivery route.
//
// It is deliberately not a source: nothing here ends up in /api/work. The
// dashboard answers "what is waiting on me", which needs you to be looking at
// it. A notification is the other half, for the change you want to hear about
// while you are looking at something else.
package notify

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/devanmcgeer/attentiond/internal/attention"
)

// ErrUnavailable is returned by a Sender that cannot deliver right now. It is
// logged and dropped: a notification is worth nothing late.
var ErrUnavailable = errors.New("notification delivery unavailable")

// Notification is one message, in the terms every delivery route understands.
type Notification struct {
	Title string
	Body  string
	// Sound is "none", "done" or "request". Each route maps it onto whatever
	// its own notification service calls that.
	Sound string
}

// Sender delivers a notification.
type Sender interface {
	Notify(ctx context.Context, notification Notification) error
}

// cooldown is how long the same item is spared a repeat notification about the
// same label, unless it does more work in the meantime.
//
// This is not a preference, it is protection against GitHub. mergeStateStatus
// is computed asynchronously and reports UNKNOWN while it is thinking, which
// drops a pull request from "ready to merge" to "approved" and back on the next
// poll. Without this, one branch sitting still would announce itself as
// mergeable every couple of minutes.
const cooldown = 15 * time.Minute

// queueDepth is how many pending notifications to hold. Transitions arrive from
// a poll loop that must not block on a socket, so the queue absorbs a burst
// (the first GitHub poll after a network outage can move a dozen items at
// once) and drops anything beyond it rather than growing without limit.
const queueDepth = 64

// Config is what to notify about.
type Config struct {
	// Labels are the item labels whose arrival is worth an interruption,
	// matched case-insensitively against attention.Item.Label.
	Labels []string
}

// Notifier watches transitions and sends the ones that match.
type Notifier struct {
	labels map[string]bool
	sender Sender
	log    *slog.Logger
	queue  chan Notification

	mu sync.Mutex
	// sent is item id to label to when that label was last announced. Nested
	// rather than flat so work starting again drops one item's whole history
	// without walking everybody else's.
	sent map[string]map[string]time.Time
	now  func() time.Time
}

// New returns a notifier. It sends nothing until Run is called.
func New(cfg Config, sender Sender, log *slog.Logger) *Notifier {
	labels := make(map[string]bool, len(cfg.Labels))
	for _, label := range cfg.Labels {
		labels[normalize(label)] = true
	}
	return &Notifier{
		labels: labels,
		sender: sender,
		log:    log,
		queue:  make(chan Notification, queueDepth),
		sent:   make(map[string]map[string]time.Time),
		now:    time.Now,
	}
}

// Labels reports what this notifier is watching, for the startup line.
func (n *Notifier) Labels() []string {
	out := make([]string, 0, len(n.labels))
	for label := range n.labels {
		out = append(out, label)
	}
	sort.Strings(out)
	return out
}

// Observe is the attention.Store observer. It runs on the caller's goroutine,
// inside a poll, so it only ever does map lookups and a non-blocking send.
func (n *Notifier) Observe(transitions []attention.Transition) {
	for _, transition := range transitions {
		// Work starting again is the line between a cycle and a flap. An
		// agent that finishes, runs once more and finishes again has two
		// pieces of news inside a quarter hour. A pull request bouncing
		// between "ready to merge" and "approved" has none, and never passes
		// through a working state on its way round.
		if transition.Next.State == attention.StateWorking {
			n.forget(transition.Next.ID)
		}

		notification, ok := n.match(transition)
		if !ok {
			continue
		}
		select {
		case n.queue <- notification:
		default:
			n.log.Warn("notification dropped, queue full", "title", notification.Title)
		}
	}
}

// match decides whether one transition is worth sending, and what to say.
func (n *Notifier) match(transition attention.Transition) (Notification, bool) {
	// A first sighting is not a change. Every item is new in the first poll
	// after a restart, and announcing the whole board on startup would train
	// somebody to ignore the notifications that matter.
	if !transition.Existed {
		return Notification{}, false
	}

	// A snooze is somebody saying "not now" to this exact item. A popup about
	// it is the one interruption guaranteed to be unwelcome. The snooze ends
	// as soon as the item's label moves, so the change that matters still
	// arrives.
	if transition.Next.Snoozed {
		return Notification{}, false
	}

	label, tone := transition.Next.Display()
	if !n.labels[normalize(label)] {
		return Notification{}, false
	}
	previous, _ := transition.Prev.Display()
	if normalize(previous) == normalize(label) {
		return Notification{}, false
	}
	if n.cooling(transition.Next.ID, label) {
		return Notification{}, false
	}

	return Notification{
		Title: capitalize(label),
		Body:  transition.Next.Title,
		Sound: sound(tone),
	}, true
}

// cooling reports whether this item already announced this label recently, and
// records the send when it has not.
func (n *Notifier) cooling(id, label string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()

	now := n.now()
	if last, ok := n.sent[id][normalize(label)]; ok && now.Sub(last) < cooldown {
		return true
	}
	// Anything past its cooldown can never suppress again, so this is also
	// where the map is kept from growing for the life of the daemon.
	for other, labels := range n.sent {
		for name, last := range labels {
			if now.Sub(last) >= cooldown {
				delete(labels, name)
			}
		}
		if len(labels) == 0 {
			delete(n.sent, other)
		}
	}
	if n.sent[id] == nil {
		n.sent[id] = make(map[string]time.Time, 1)
	}
	n.sent[id][normalize(label)] = now
	return false
}

// forget drops an item's notification history, so the next arrival at a label
// it already announced counts as news again.
func (n *Notifier) forget(id string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.sent, id)
}

// Run delivers queued notifications until ctx is done. Delivery reaches another
// process, so it happens here rather than on the poll goroutine that produced
// the transition.
func (n *Notifier) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case notification := <-n.queue:
			n.deliver(ctx, notification)
		}
	}
}

func (n *Notifier) deliver(ctx context.Context, notification Notification) {
	send, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if err := n.sender.Notify(send, notification); err != nil {
		// Best effort by design. A route that is not there (Herdr not
		// running, no notifier binary installed) is the normal state of a
		// machine nobody is sitting at, and a daemon that logged this as an
		// error would cry wolf every time.
		n.log.Info("notification not delivered",
			"title", notification.Title, "error", err)
		return
	}
	n.log.Info("notification sent", "title", notification.Title, "body", notification.Body)
}

// sound picks the sound for a tone: anything that wants a human to act earns
// the one that sounds like a request, and progress reports stay silent.
func sound(tone attention.Tone) string {
	switch tone {
	case attention.ToneReady, attention.ToneAttention, attention.ToneFailed:
		return "request"
	default:
		return "none"
	}
}

func normalize(label string) string {
	return strings.ToLower(strings.TrimSpace(label))
}

func capitalize(label string) string {
	if label == "" {
		return label
	}
	return strings.ToUpper(label[:1]) + label[1:]
}
