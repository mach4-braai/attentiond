package notify

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/devanmcgeer/attentiond/internal/attention"
)

type recordingSender struct {
	sent []Notification
	err  error
}

func (r *recordingSender) Notify(_ context.Context, notification Notification) error {
	r.sent = append(r.sent, notification)
	return r.err
}

// testNotifier returns a notifier with a controllable clock, plus the sender it
// writes to. Run is not started: drain() delivers synchronously so a test never
// waits on a goroutine.
func testNotifier(t *testing.T, labels []string, clock *time.Time) (*Notifier, *recordingSender) {
	t.Helper()
	sender := &recordingSender{}
	notifier := New(Config{Labels: labels}, sender, slog.New(slog.NewTextHandler(io.Discard, nil)))
	notifier.now = func() time.Time { return *clock }
	return notifier, sender
}

func drain(notifier *Notifier) {
	for {
		select {
		case notification := <-notifier.queue:
			notifier.deliver(context.Background(), notification)
		default:
			return
		}
	}
}

func item(id, label string, tone attention.Tone) attention.Item {
	return attention.Item{
		ID:       id,
		Source:   "github",
		Title:    "didx-xyz/tofu#1 · Move the RDS credentials",
		State:    attention.StateNeedsAttention,
		Severity: attention.SeverityWarning,
		Label:    label,
		Tone:     tone,
	}
}

func TestNotifiesOnlyOnAChangeIntoAWatchedLabel(t *testing.T) {
	now := time.Date(2026, 9, 13, 9, 0, 0, 0, time.UTC)
	notifier, sender := testNotifier(t, []string{"ready to merge"}, &now)

	mergeable := item("github:didx-xyz/tofu#1", "ready to merge", attention.ToneReady)
	asked := item("github:didx-xyz/tofu#1", "review requested", attention.ToneAttention)

	// A first sighting is not a change. Every item is new in the first poll
	// after a restart, and announcing the board on startup is how somebody
	// learns to ignore these.
	notifier.Observe([]attention.Transition{{Next: mergeable, Existed: false}})
	drain(notifier)
	if len(sender.sent) != 0 {
		t.Fatalf("a first sighting notified: %+v", sender.sent)
	}

	// A label nobody asked about stays quiet.
	notifier.Observe([]attention.Transition{{Prev: mergeable, Next: asked, Existed: true}})
	drain(notifier)
	if len(sender.sent) != 0 {
		t.Fatalf("an unwatched label notified: %+v", sender.sent)
	}

	notifier.Observe([]attention.Transition{{Prev: asked, Next: mergeable, Existed: true}})
	drain(notifier)
	if len(sender.sent) != 1 {
		t.Fatalf("the watched transition did not notify: %+v", sender.sent)
	}
	if sender.sent[0].Title != "Ready to merge" {
		t.Errorf("title = %q, want the label", sender.sent[0].Title)
	}
	if sender.sent[0].Body != mergeable.Title {
		t.Errorf("body = %q, want the item title", sender.sent[0].Body)
	}
	if sender.sent[0].Sound != "request" {
		t.Errorf("sound = %q, want a noise for something that wants an action", sender.sent[0].Sound)
	}
}

func TestFlappingUpstreamStateNotifiesOnce(t *testing.T) {
	now := time.Date(2026, 9, 13, 9, 0, 0, 0, time.UTC)
	notifier, sender := testNotifier(t, []string{"ready to merge"}, &now)

	mergeable := item("github:didx-xyz/tofu#1", "ready to merge", attention.ToneReady)
	approved := item("github:didx-xyz/tofu#1", "approved", attention.ToneNeutral)

	// GitHub computes mergeStateStatus asynchronously and reports UNKNOWN while
	// it is thinking, which drops a mergeable pull request to "approved" and
	// back on the next poll. One branch sitting still must not announce itself
	// every minute.
	for range 5 {
		notifier.Observe([]attention.Transition{{Prev: approved, Next: mergeable, Existed: true}})
		drain(notifier)
		notifier.Observe([]attention.Transition{{Prev: mergeable, Next: approved, Existed: true}})
		drain(notifier)
		now = now.Add(time.Minute)
	}

	if len(sender.sent) != 1 {
		t.Fatalf("a flapping pull request sent %d notifications, want 1", len(sender.sent))
	}

	// Past the cooldown it is news again: the same pull request going green a
	// day later is a real event.
	now = now.Add(cooldown)
	notifier.Observe([]attention.Transition{{Prev: approved, Next: mergeable, Existed: true}})
	drain(notifier)
	if len(sender.sent) != 2 {
		t.Fatalf("the cooldown never expired: %d notifications", len(sender.sent))
	}

	// A different pull request reaching the same state is its own news.
	other := item("github:didx-xyz/mono#7", "ready to merge", attention.ToneReady)
	notifier.Observe([]attention.Transition{{Prev: approved, Next: other, Existed: true}})
	drain(notifier)
	if len(sender.sent) != 3 {
		t.Fatalf("the cooldown suppressed a different item: %d notifications", len(sender.sent))
	}
}

func TestAnAgentCycleNotifiesEveryTimeItFinishes(t *testing.T) {
	now := time.Date(2026, 9, 13, 9, 0, 0, 0, time.UTC)
	notifier, sender := testNotifier(t, []string{"done"}, &now)

	pane := func(label string, state attention.State, tone attention.Tone) attention.Item {
		return attention.Item{
			ID:     "herdr:w1:p1",
			Source: "herdr",
			Title:  "dotfiles · omp",
			State:  state,
			Label:  label,
			Tone:   tone,
		}
	}
	finished := pane("done", attention.StateDone, attention.ToneDone)
	resting := pane("idle", attention.StateWaiting, attention.ToneNeutral)
	running := pane("working", attention.StateWorking, attention.ToneActive)

	// Two turns in the same pane, four minutes apart. Both finishes are news:
	// waiting a quarter of an hour to be told the second one landed is how a
	// notifier becomes furniture.
	notifier.Observe([]attention.Transition{{Prev: running, Next: finished, Existed: true}})
	drain(notifier)
	now = now.Add(2 * time.Minute)
	notifier.Observe([]attention.Transition{{Prev: finished, Next: resting, Existed: true}})
	notifier.Observe([]attention.Transition{{Prev: resting, Next: running, Existed: true}})
	drain(notifier)
	now = now.Add(2 * time.Minute)
	notifier.Observe([]attention.Transition{{Prev: running, Next: finished, Existed: true}})
	drain(notifier)

	if len(sender.sent) != 2 {
		t.Fatalf("two finished turns sent %d notifications, want 2", len(sender.sent))
	}

	// Without work in between it is the same news twice: the pane settling
	// from done to idle and back must stay quiet.
	now = now.Add(time.Minute)
	notifier.Observe([]attention.Transition{{Prev: finished, Next: resting, Existed: true}})
	notifier.Observe([]attention.Transition{{Prev: resting, Next: finished, Existed: true}})
	drain(notifier)

	if len(sender.sent) != 2 {
		t.Fatalf("a pane settling notified again: %d notifications", len(sender.sent))
	}
}

func TestCheckedLabelsAreMatchedLoosely(t *testing.T) {
	now := time.Date(2026, 9, 13, 9, 0, 0, 0, time.UTC)
	notifier, sender := testNotifier(t, []string{"  Checks Running "}, &now)

	running := item("github:didx-xyz/tofu#1", "checks running", attention.ToneActive)
	running.State = attention.StateWorking
	before := item("github:didx-xyz/tofu#1", "awaiting review", attention.ToneNeutral)

	notifier.Observe([]attention.Transition{{Prev: before, Next: running, Existed: true}})
	drain(notifier)

	if len(sender.sent) != 1 {
		t.Fatalf("a config file's spacing and capitalization changed the outcome: %+v", sender.sent)
	}
	// Checks starting is a progress report, not a request. It should not make
	// a noise.
	if sender.sent[0].Sound != "none" {
		t.Errorf("sound = %q, want silence for a progress report", sender.sent[0].Sound)
	}
}

func TestAnUndeliverableNotificationIsDropped(t *testing.T) {
	now := time.Date(2026, 9, 13, 9, 0, 0, 0, time.UTC)
	notifier, sender := testNotifier(t, []string{"ready to merge"}, &now)
	sender.err = ErrUnavailable

	mergeable := item("github:didx-xyz/tofu#1", "ready to merge", attention.ToneReady)
	approved := item("github:didx-xyz/tofu#1", "approved", attention.ToneNeutral)

	// Herdr not running is the normal state of a machine nobody is sitting at.
	// It must not panic, block, or retry forever.
	notifier.Observe([]attention.Transition{{Prev: approved, Next: mergeable, Existed: true}})
	drain(notifier)

	if len(sender.sent) != 1 {
		t.Fatalf("delivery was not attempted: %+v", sender.sent)
	}
	if got := len(notifier.queue); got != 0 {
		t.Errorf("a failed notification stayed queued: %d", got)
	}
}

// A snooze is somebody saying "not now" to this item. The popup is the one
// interruption they have already refused.
func TestASnoozedItemDoesNotInterrupt(t *testing.T) {
	now := time.Date(2026, 9, 13, 9, 0, 0, 0, time.UTC)
	notifier, sender := testNotifier(t, []string{"ready to merge"}, &now)

	asked := item("github:didx-xyz/tofu#1", "review requested", attention.ToneAttention)
	mergeable := item("github:didx-xyz/tofu#1", "ready to merge", attention.ToneReady)
	mergeable.Snoozed = true

	notifier.Observe([]attention.Transition{{Prev: asked, Next: mergeable, Existed: true}})
	drain(notifier)
	if len(sender.sent) != 0 {
		t.Fatalf("a snoozed item interrupted anyway: %+v", sender.sent)
	}

	// The store clears the snooze when the label moves, so the same change
	// arriving awake is still news.
	mergeable.Snoozed = false
	notifier.Observe([]attention.Transition{{Prev: asked, Next: mergeable, Existed: true}})
	drain(notifier)
	if len(sender.sent) != 1 {
		t.Fatalf("the woken change did not notify: %+v", sender.sent)
	}
}
