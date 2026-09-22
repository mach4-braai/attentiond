// Package desktop delivers notifications through macOS's own notification
// service, for a setup where no other tool is allowed to paint the popup.
//
// It exists because Herdr's delivery is one switch: turning off Herdr's own
// agent toasts also turns off everything handed to it over the socket. A
// machine that wants attentiond's interruptions without Herdr's needs a route
// that does not go through Herdr at all.
package desktop

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/devanmcgeer/attentiond/internal/notify"
)

// terminal-notifier is preferred over osascript because a notification it
// posts is attributed to terminal-notifier rather than to Script Editor, and
// it accepts title and message as arguments instead of as a quoted AppleScript
// string. osascript is the fallback: it ships with the operating system.
const (
	preferred = "terminal-notifier"
	fallback  = "osascript"
)

// Notifier posts notifications to Notification Center.
type Notifier struct {
	// lookPath and run are fields so a test can drive the command choice and
	// the arguments without a notification appearing on somebody's screen.
	lookPath func(file string) (string, error)
	run      func(ctx context.Context, name string, args ...string) error
}

// NewNotifier returns the system notification sender.
func NewNotifier() *Notifier {
	return &Notifier{lookPath: exec.LookPath, run: runCommand}
}

// Notify implements notify.Sender.
func (n *Notifier) Notify(ctx context.Context, notification notify.Notification) error {
	if path, err := n.lookPath(preferred); err == nil {
		if err := n.run(ctx, path, terminalNotifierArgs(notification)...); err == nil {
			return nil
		}
	}

	path, err := n.lookPath(fallback)
	if err != nil {
		return fmt.Errorf("desktop: no %s and no %s: %w", preferred, fallback, notify.ErrUnavailable)
	}
	if err := n.run(ctx, path, "-e", appleScript(notification)); err != nil {
		return fmt.Errorf("desktop: %s: %w", fallback, err)
	}
	return nil
}

func terminalNotifierArgs(notification notify.Notification) []string {
	args := []string{"-title", notification.Title, "-message", notification.Body}
	if name := soundName(notification.Sound); name != "" {
		args = append(args, "-sound", name)
	}
	return args
}

func appleScript(notification notify.Notification) string {
	script := "display notification " + quote(notification.Body) +
		" with title " + quote(notification.Title)
	if name := soundName(notification.Sound); name != "" {
		script += " sound name " + quote(name)
	}
	return script
}

// soundName maps the tool-neutral sound to a name Notification Center knows.
// Empty means silent, which is what a progress report gets.
func soundName(sound string) string {
	switch sound {
	case "request":
		return "Ping"
	case "done":
		return "Glass"
	default:
		return ""
	}
}

// quote wraps text as an AppleScript string literal. A pull request title is
// somebody else's text and routinely contains a double quote or a backslash,
// either of which would otherwise end the literal early and turn the rest of
// the title into AppleScript.
func quote(text string) string {
	escaped := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(text)
	return `"` + escaped + `"`
}

func runCommand(ctx context.Context, name string, args ...string) error {
	return exec.CommandContext(ctx, name, args...).Run()
}
