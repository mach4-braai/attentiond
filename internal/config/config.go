// Package config loads attentiond's on-disk configuration. One file describes
// the daemon and every source, so adding a tool means adding a table rather
// than lengthening a command line.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Duration is a time.Duration written the way a human writes one: "2s", "1m",
// "336h".
type Duration time.Duration

// UnmarshalText implements encoding.TextUnmarshaler for TOML strings.
func (d *Duration) UnmarshalText(text []byte) error {
	parsed, err := time.ParseDuration(string(text))
	if err != nil {
		return err
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) String() string { return time.Duration(d).String() }

// Std returns the standard library duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// Config is the whole file. Each source owns one table.
type Config struct {
	Daemon    Daemon    `toml:"daemon"`
	Attention Attention `toml:"attention"`
	Events    Events    `toml:"events"`
	Herdr     Herdr     `toml:"herdr"`
	GitHub    GitHub    `toml:"github"`
	Calendar  Calendar  `toml:"calendar"`
}

// Attention tunes the queue itself rather than any one source.
type Attention struct {
	// DoneTTL is how long finished work stays in /api/attention. It keeps
	// appearing in /api/work, which is the whole board.
	DoneTTL Duration `toml:"done_ttl"`
	// StaleAfter is how long an item can sit with nothing happening to it
	// before it leaves every list but /api/stale. Zero, the default, keeps
	// the board complete: moving work out of sight is a decision to make on
	// purpose, in a file, with a period written next to it.
	StaleAfter Duration `toml:"stale_after"`
	// SnoozeFor is the length of the snooze the dashboard offers on each
	// item. The button says the number, so this is what it says.
	SnoozeFor Duration `toml:"snooze_for"`
	// TopLabels are item labels that go above every rank a source can give
	// itself. This is the one ordering decision that belongs in a file:
	// whether something outranks the whole table is a judgement about your
	// week, not a property of the work.
	TopLabels []string `toml:"top_labels"`
}

// Daemon is the listener and the logs.
type Daemon struct {
	Addr      string `toml:"addr"`
	PublicURL string `toml:"public_url"`
	LogLevel  string `toml:"log_level"`
	LogFormat string `toml:"log_format"`
	// StateFile is where snoozes and bumps are kept across restarts. Empty
	// resolves to $XDG_STATE_HOME/attentiond/decisions.json, beside the pid
	// and the log. Items are not in it: every source rebuilds those, and no
	// source can rebuild a decision you made.
	StateFile string `toml:"state_file"`
}

// Events configures the source every local process writes to, POST
// /api/events. It has no poller, so the only knob is how long finished work
// stays on screen.
type Events struct {
	TTL Duration `toml:"ttl"`
}

// Herdr is the terminal workspace source.
type Herdr struct {
	Enabled bool     `toml:"enabled"`
	Socket  string   `toml:"socket"`
	Fixture string   `toml:"fixture"`
	Poll    Duration `toml:"poll"`
}

// GitHub is the pull request source.
type GitHub struct {
	Enabled         bool     `toml:"enabled"`
	API             string   `toml:"api"`
	Poll            Duration `toml:"poll"`
	Repos           []string `toml:"repos"`
	Orgs            []string `toml:"orgs"`
	Limit           int      `toml:"limit"`
	StaleDraftAfter Duration `toml:"stale_draft_after"`
	// PriorityRepos are repositories whose review requests go to the top of
	// the queue. A review you owe in the repository that runs the
	// infrastructure is not the same errand as one in a side project, and
	// severity cannot say so: both are warnings.
	PriorityRepos []string `toml:"priority_repos"`
}

// Calendar is the meetings source. It reads iCalendar feeds, one per
// [[calendar.feeds]] entry, so several calendars can be watched at once.
type Calendar struct {
	Enabled bool     `toml:"enabled"`
	Poll    Duration `toml:"poll"`
	// Horizon is how far ahead to look.
	Horizon Duration `toml:"horizon"`
	// Lead is how long before a meeting starts it wants your attention.
	Lead  Duration       `toml:"lead"`
	Feeds []CalendarFeed `toml:"feeds"`
}

// CalendarFeed is one calendar. The address comes from url, from the
// environment variable named by url_env, or from the email, which resolves to
// the Google public address for that account.
type CalendarFeed struct {
	Email  string `toml:"email"`
	URL    string `toml:"url"`
	URLEnv string `toml:"url_env"`
	Label  string `toml:"label"`
}

// Default is the configuration attentiond runs with when no file exists.
//
// Herdr is on: it is local, it costs one socket call, and it is the reason the
// daemon exists. GitHub is off. An unconfigured GitHub source watches every
// repository the token can see, which is a fan-out nobody asked for, and it
// would be exactly what a machine falls back to when its config file goes
// missing. Turning it on is a sentence in a file.
func Default() Config {
	return Config{
		Daemon: Daemon{
			Addr:      "127.0.0.1:7717",
			LogLevel:  "info",
			LogFormat: "text",
		},
		Attention: Attention{
			// Ten minutes is how long "I just finished that" stays useful. A
			// pane you have not looked at by then is not news any more, and
			// leaving it in the queue puts finished work on top of work that
			// still needs doing. Herdr only retires its own done items when
			// you focus the pane, so without this they never leave.
			DoneTTL: Duration(10 * time.Minute),
			// Four hours is the rest of a working day from mid-morning: long
			// enough that the item is gone while you do the thing you chose
			// instead, short enough that it is back before you go home.
			// StaleAfter stays zero, so nothing leaves the board until a file
			// says how old is too old.
			SnoozeFor: Duration(4 * time.Hour),
		},
		Events: Events{TTL: Duration(time.Hour)},
		Herdr: Herdr{
			Enabled: true,
			Poll:    Duration(2 * time.Second),
		},
		GitHub: GitHub{
			Enabled:         false,
			API:             "https://api.github.com/graphql",
			Poll:            Duration(time.Minute),
			Limit:           100,
			StaleDraftAfter: Duration(14 * 24 * time.Hour),
		},
		Calendar: Calendar{
			// Off like GitHub: it reaches the network, and with no feeds
			// configured there is nothing for it to read anyway.
			Enabled: false,
			Poll:    Duration(5 * time.Minute),
			Horizon: Duration(12 * time.Hour),
			Lead:    Duration(10 * time.Minute),
		},
	}
}

// ResolvePath decides which file to read. A path named by --config or by
// $ATTENTIOND_CONFIG is explicit: somebody pointed at a file, so its absence
// is a mistake, not a machine that has not been configured yet. Only the
// implicit ~/.attn/config.toml is allowed to be missing.
func ResolvePath(flagPath string) (path string, explicit bool) {
	if trimmed := strings.TrimSpace(flagPath); trimmed != "" {
		return trimmed, true
	}
	if env := strings.TrimSpace(os.Getenv("ATTENTIOND_CONFIG")); env != "" {
		return env, true
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false
	}
	return filepath.Join(home, ".attn", "config.toml"), false
}

// Load reads path on top of the defaults, so a file only has to say what it
// changes. A missing file is only an error when the path was asked for
// explicitly: not having written one yet is not a mistake, but pointing at a
// file that is not there is.
//
// Unknown keys are rejected. A misspelled key would otherwise be silently
// ignored and leave the daemon running settings the file appears to change.
func Load(path string, explicit bool) (cfg Config, found bool, err error) {
	cfg = Default()
	if path == "" {
		return cfg, false, nil
	}

	meta, err := toml.DecodeFile(path, &cfg)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) && !explicit {
			return Default(), false, nil
		}
		return Default(), false, fmt.Errorf("read %s: %w", path, err)
	}

	if undecoded := meta.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, 0, len(undecoded))
		for _, key := range undecoded {
			keys = append(keys, key.String())
		}
		sort.Strings(keys)
		return Default(), false, fmt.Errorf("%s: unknown %s: %s",
			path, plural("key", len(keys)), strings.Join(keys, ", "))
	}

	// A negative period is not a shorter one: stale_after = "-720h" marks
	// every item on the board stale the moment it appears, and the daemon
	// would look empty for a reason nothing on screen explains.
	for _, check := range []struct {
		key   string
		value Duration
	}{
		{"attention.done_ttl", cfg.Attention.DoneTTL},
		{"attention.stale_after", cfg.Attention.StaleAfter},
		{"attention.snooze_for", cfg.Attention.SnoozeFor},
		{"events.ttl", cfg.Events.TTL},
	} {
		if check.value < 0 {
			return Default(), false, fmt.Errorf("%s: %s %s: want zero or a period",
				path, check.key, check.value)
		}
	}

	return cfg, true, nil
}

func plural(word string, n int) string {
	if n == 1 {
		return word
	}
	return word + "s"
}
