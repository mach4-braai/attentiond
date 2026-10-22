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
	Notify    Notify    `toml:"notify"`
	Herdr     Herdr     `toml:"herdr"`
	GitHub    GitHub    `toml:"github"`
	Calendar  Calendar  `toml:"calendar"`
	Spend     Spend     `toml:"spend"`
	Log       Log       `toml:"log"`
	Datadog   Datadog   `toml:"datadog"`
	Notes     Notes     `toml:"notes"`
}

// Log is the signal log: one JSON line for every item that appears, changes
// state or label, or disappears. GET /api/log serves it.
type Log struct {
	// Path is the file. Empty resolves to
	// $XDG_STATE_HOME/attentiond/signals.jsonl, beside the decisions file.
	Path string `toml:"path"`
	// Retention is how long an entry is kept. Older entries are dropped by a
	// rewrite at most once an hour. Zero keeps everything.
	Retention Duration `toml:"retention"`
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

// Notify is the interruption path: which changes are worth telling a human
// about while they are looking at something else, and who paints the popup.
type Notify struct {
	Enabled bool `toml:"enabled"`
	// Route is the delivery path: "herdr" hands the notification to a running
	// Herdr server, "system" calls the operating system's own notification
	// service. Herdr knows whether anybody is looking at the terminal, so it
	// is the better route right up until you want Herdr itself silent.
	Route string `toml:"route"`
	// Labels are the item labels worth a notification when an item moves onto
	// one. Labels, not states: "review requested" and "ready to merge" are
	// both needs_attention, and moving between them is the news.
	Labels []string `toml:"labels"`
}

// Notification routes.
const (
	RouteHerdr  = "herdr"
	RouteSystem = "system"
)

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
	// PriorityRepos are repositories whose review requests rank above the
	// same request elsewhere. A review you owe in the repository that runs
	// the infrastructure is not the same errand as one in a side project, and
	// severity cannot say so: both are warnings.
	PriorityRepos []string `toml:"priority_repos"`
}

// Datadog is the container vulnerability source. It reads the findings
// endpoint with an API key and an application key, each named by an
// environment variable so neither lands in a file.
type Datadog struct {
	Enabled bool `toml:"enabled"`
	// Site is the Datadog site, such as datadoghq.eu. The API host is
	// api.<site>.
	Site  string   `toml:"site"`
	Poll  Duration `toml:"poll"`
	Query string   `toml:"query"`
	// PageLimit is findings per request. Datadog allows at most 1000.
	PageLimit int    `toml:"page_limit"`
	APIKeyEnv string `toml:"api_key_env"`
	AppKeyEnv string `toml:"app_key_env"`
	// Fixture replays a recorded response instead of calling the API, and
	// needs no keys.
	Fixture string `toml:"fixture"`
}

// Notes is the markdown notes source. It watches the top level of each
// [[notes.dirs]] directory and makes one item per *.md file.
type Notes struct {
	Enabled bool       `toml:"enabled"`
	Poll    Duration   `toml:"poll"`
	Dirs    []NotesDir `toml:"dirs"`
}

// NotesDir is one watched directory. Label prefixes the id of every note in
// it, so two directories can hold files with the same name.
type NotesDir struct {
	// Path is absolute once loaded: a leading ~ is expanded.
	Path  string `toml:"path"`
	Label string `toml:"label"`
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

// Spend is the omp cost source. It reads omp's stats.db for each omp session
// Herdr has open, so it needs [herdr] on.
type Spend struct {
	Enabled bool     `toml:"enabled"`
	Poll    Duration `toml:"poll"`
	// DB is omp's usage index. Empty is ~/.omp/stats.db.
	DB string `toml:"db"`
	// Sync is the command that brings the index up to date before each
	// read, as argv. omp indexes transcripts only when asked, so an empty
	// list reads whatever the last `omp stats` left.
	Sync []string `toml:"sync"`
	// AdvisorCost is the cost in dollars at which one advisor transcript
	// wants a human. Zero turns it off.
	AdvisorCost float64 `toml:"advisor_cost"`
	// LookupsPerNote is the read, grep and glob calls per advise call at
	// which an advisor wants a human. Zero turns it off.
	LookupsPerNote float64 `toml:"lookups_per_note"`
	// MinLookups is how many lookups an advisor makes before the ratio
	// counts.
	MinLookups int `toml:"min_lookups"`
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
		Notify: Notify{
			// On, unlike GitHub and the calendar: delivery is a local socket
			// call to Herdr, which either answers or does not. Both labels are
			// changes somebody waiting on a build wants to hear without
			// watching a tab.
			Enabled: true,
			Route:   RouteHerdr,
			Labels:  []string{"ready to merge", "checks running"},
		},
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
		Spend: Spend{
			// Off: it runs omp every poll, which a machine without omp
			// does not have.
			Enabled: false,
			Poll:    Duration(5 * time.Minute),
			Sync:    []string{"omp", "stats", "--summary"},
			// 27 of the first 344 advisor transcripts passed $25; the
			// costliest reached $683.
			AdvisorCost: 25,
			// A normal month runs about 4 lookups per note. The $683
			// transcript made 171.
			LookupsPerNote: 20,
			MinLookups:     100,
		},
		Log: Log{Retention: Duration(720 * time.Hour)},
		Datadog: Datadog{
			// Off: it reaches the network and needs two keys.
			Enabled:   false,
			Site:      "datadoghq.eu",
			Poll:      Duration(30 * time.Minute),
			Query:     "@finding_type:host_and_container_vulnerability @status:open",
			PageLimit: 100,
			APIKeyEnv: "DD_API_KEY",
			AppKeyEnv: "DD_APP_KEY",
		},
		Notes: Notes{
			// Off: it reads directories the user has to name.
			Enabled: false,
			Poll:    Duration(time.Minute),
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

	// A misspelled route would otherwise pick the default and deliver
	// somewhere the file does not name, which is the same silent surprise
	// unknown keys are rejected for.
	switch cfg.Notify.Route {
	case RouteHerdr, RouteSystem:
	default:
		return Default(), false, fmt.Errorf("%s: notify.route %q: want %q or %q",
			path, cfg.Notify.Route, RouteHerdr, RouteSystem)
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
		{"log.retention", cfg.Log.Retention},
	} {
		if check.value < 0 {
			return Default(), false, fmt.Errorf("%s: %s %s: want zero or a period",
				path, check.key, check.value)
		}
	}

	if cfg.Spend.Enabled && cfg.Spend.Poll <= 0 {
		return Default(), false, fmt.Errorf("%s: spend.poll %s: want a period", path, cfg.Spend.Poll)
	}
	if cfg.Spend.AdvisorCost < 0 || cfg.Spend.LookupsPerNote < 0 || cfg.Spend.MinLookups < 0 {
		return Default(), false, fmt.Errorf("%s: spend limits: want zero (off) or a positive number", path)
	}
	if cfg.Datadog.Enabled {
		if cfg.Datadog.Poll <= 0 {
			return Default(), false, fmt.Errorf("%s: datadog.poll %s: want a period", path, cfg.Datadog.Poll)
		}
		if cfg.Datadog.PageLimit < 1 || cfg.Datadog.PageLimit > 1000 {
			return Default(), false, fmt.Errorf("%s: datadog.page_limit %d: want 1 to 1000",
				path, cfg.Datadog.PageLimit)
		}
		if strings.TrimSpace(cfg.Datadog.Site) == "" && cfg.Datadog.Fixture == "" {
			return Default(), false, fmt.Errorf("%s: datadog.site: want a site such as datadoghq.eu", path)
		}
	}
	if err := validateNotes(&cfg.Notes); err != nil {
		return Default(), false, fmt.Errorf("%s: %w", path, err)
	}

	return cfg, true, nil
}

func plural(word string, n int) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// validateNotes expands ~ in every directory and checks the table. A relative
// path would depend on where the daemon was started, so it is refused.
func validateNotes(n *Notes) error {
	for i := range n.Dirs {
		p := strings.TrimSpace(n.Dirs[i].Path)
		if p == "~" || strings.HasPrefix(p, "~/") {
			home, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("notes.dirs[%d].path %q: cannot expand ~: %w", i, p, err)
			}
			p = filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
		n.Dirs[i].Path = p
	}
	if !n.Enabled {
		return nil
	}
	if n.Poll <= 0 {
		return fmt.Errorf("notes.poll %s: want a period", n.Poll)
	}
	if len(n.Dirs) == 0 {
		return errors.New("notes.dirs: want at least one [[notes.dirs]] entry when notes is enabled")
	}
	labels := make(map[string]bool, len(n.Dirs))
	for i, d := range n.Dirs {
		if !filepath.IsAbs(d.Path) {
			return fmt.Errorf("notes.dirs[%d].path %q: want an absolute path or one starting with ~", i, d.Path)
		}
		if strings.TrimSpace(d.Label) == "" {
			return fmt.Errorf("notes.dirs[%d].label: want a label", i)
		}
		if labels[d.Label] {
			return fmt.Errorf("notes.dirs[%d].label %q: already used by another directory", i, d.Label)
		}
		labels[d.Label] = true
	}
	return nil
}
