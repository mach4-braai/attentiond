package desktop

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/devanmcgeer/attentiond/internal/notify"
)

type call struct {
	name string
	args []string
}

// testNotifier returns a notifier whose commands are recorded instead of run.
// present names the binaries LookPath resolves; fail names the ones whose run
// returns an error.
func testNotifier(present map[string]bool, fail map[string]bool) (*Notifier, *[]call) {
	calls := []call{}
	notifier := &Notifier{
		lookPath: func(file string) (string, error) {
			if present[file] {
				return "/usr/local/bin/" + file, nil
			}
			return "", exec.ErrNotFound
		},
		run: func(_ context.Context, name string, args ...string) error {
			calls = append(calls, call{name: name, args: args})
			if fail[strings.TrimPrefix(name, "/usr/local/bin/")] {
				return errors.New("exit status 1")
			}
			return nil
		},
	}
	return notifier, &calls
}

func TestFallsBackToOsascriptWhenTerminalNotifierFails(t *testing.T) {
	notifier, calls := testNotifier(
		map[string]bool{preferred: true, fallback: true},
		map[string]bool{preferred: true},
	)

	err := notifier.Notify(context.Background(), notify.Notification{
		Title: "Ready to merge",
		Body:  "didx-xyz/tofu#1 · Move the RDS credentials",
		Sound: "request",
	})
	if err != nil {
		t.Fatalf("Notify: %v", err)
	}

	if len(*calls) != 2 {
		t.Fatalf("calls = %d, want 2: %+v", len(*calls), *calls)
	}
	if got := (*calls)[1].name; !strings.HasSuffix(got, fallback) {
		t.Fatalf("second call = %q, want %s", got, fallback)
	}
	script := (*calls)[1].args[1]
	if !strings.Contains(script, `with title "Ready to merge"`) {
		t.Fatalf("script = %q, want the title", script)
	}
	if !strings.Contains(script, `sound name "Ping"`) {
		t.Fatalf("script = %q, want the request sound", script)
	}
}

func TestQuotesAreEscapedIntoTheAppleScriptLiteral(t *testing.T) {
	notifier, calls := testNotifier(
		map[string]bool{fallback: true},
		nil,
	)

	err := notifier.Notify(context.Background(), notify.Notification{
		Title: "Ready to merge",
		Body:  `tofu#1 · Quote the "name" and the \path`,
	})
	if err != nil {
		t.Fatalf("Notify: %v", err)
	}

	script := (*calls)[0].args[1]
	want := `display notification "tofu#1 · Quote the \"name\" and the \\path" with title "Ready to merge"`
	if script != want {
		t.Fatalf("script = %q, want %q", script, want)
	}
}

func TestUnavailableWhenNeitherBinaryExists(t *testing.T) {
	notifier, calls := testNotifier(nil, nil)

	err := notifier.Notify(context.Background(), notify.Notification{Title: "Ready to merge"})
	if !errors.Is(err, notify.ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if len(*calls) != 0 {
		t.Fatalf("calls = %+v, want none", *calls)
	}
}

func TestSilentNotificationPassesNoSound(t *testing.T) {
	notifier, calls := testNotifier(map[string]bool{preferred: true}, nil)

	err := notifier.Notify(context.Background(), notify.Notification{
		Title: "Checks running",
		Body:  "didx-xyz/tofu#1",
		Sound: "none",
	})
	if err != nil {
		t.Fatalf("Notify: %v", err)
	}

	for _, arg := range (*calls)[0].args {
		if arg == "-sound" {
			t.Fatalf("args = %+v, want no sound flag", (*calls)[0].args)
		}
	}
}
