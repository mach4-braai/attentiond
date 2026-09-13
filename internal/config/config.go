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
	Daemon Daemon `toml:"daemon"`
	Events Events `toml:"events"`
	Herdr  Herdr  `toml:"herdr"`
	GitHub GitHub `toml:"github"`
}

// Daemon is the listener and the logs.
type Daemon struct {
	Addr      string `toml:"addr"`
	PublicURL string `toml:"public_url"`
	LogLevel  string `toml:"log_level"`
	LogFormat string `toml:"log_format"`
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
}

// Default is the configuration attentiond runs with when no file exists. The
// daemon has to be useful on a machine that never wrote one.
func Default() Config {
	return Config{
		Daemon: Daemon{
			Addr:      "127.0.0.1:7717",
			LogLevel:  "info",
			LogFormat: "text",
		},
		Events: Events{TTL: Duration(time.Hour)},
		Herdr: Herdr{
			Enabled: true,
			Poll:    Duration(2 * time.Second),
		},
		GitHub: GitHub{
			Enabled:         true,
			API:             "https://api.github.com/graphql",
			Poll:            Duration(time.Minute),
			Limit:           100,
			StaleDraftAfter: Duration(14 * 24 * time.Hour),
		},
	}
}

// DefaultPath is where attentiond looks when nothing says otherwise:
// $ATTENTIOND_CONFIG, then ~/.attn/config.toml.
func DefaultPath() string {
	if path := strings.TrimSpace(os.Getenv("ATTENTIOND_CONFIG")); path != "" {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".attn", "config.toml")
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

	return cfg, true, nil
}

func plural(word string, n int) string {
	if n == 1 {
		return word
	}
	return word + "s"
}
