package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The example is documentation that can go stale silently, except that Load
// rejects unknown keys, so loading it here turns a renamed or deleted setting
// into a failing test instead of a file that lies.
func TestExampleConfigStaysValid(t *testing.T) {
	cfg, found, err := Load("../../config.example.toml", true)
	if err != nil {
		t.Fatalf("config.example.toml: %v", err)
	}
	if !found {
		t.Fatal("config.example.toml is missing")
	}

	// It shows every key, so every table has to survive the round trip.
	if cfg.Daemon.Addr == "" || cfg.Events.TTL == 0 || cfg.Herdr.Poll == 0 {
		t.Errorf("example decoded to %+v", cfg)
	}
	if cfg.GitHub.Limit == 0 || cfg.GitHub.StaleDraftAfter == 0 {
		t.Errorf("example github decoded to %+v", cfg.GitHub)
	}
	if len(cfg.GitHub.Repos) == 0 || len(cfg.GitHub.Orgs) == 0 {
		t.Error("the example stopped showing how to scope repositories")
	}
}

func TestLoadOnlyChangesWhatTheFileSays(t *testing.T) {
	path := write(t, `
[daemon]
addr = "127.0.0.1:9000"

[github]
orgs = ["example-org", "mach4-braai"]
poll = "5m"
`)

	cfg, found, err := Load(path, true)
	if err != nil || !found {
		t.Fatalf("Load = %v, %v", found, err)
	}

	if cfg.Daemon.Addr != "127.0.0.1:9000" {
		t.Errorf("addr = %q", cfg.Daemon.Addr)
	}
	if cfg.GitHub.Poll.Std() != 5*time.Minute {
		t.Errorf("poll = %s", cfg.GitHub.Poll)
	}
	if len(cfg.GitHub.Orgs) != 2 {
		t.Errorf("orgs = %v", cfg.GitHub.Orgs)
	}

	// Everything the file left alone keeps the default, including the fields
	// next to the ones it did set.
	defaults := Default()
	if cfg.Daemon.LogLevel != defaults.Daemon.LogLevel {
		t.Errorf("log_level = %q, want the default %q", cfg.Daemon.LogLevel, defaults.Daemon.LogLevel)
	}
	if cfg.GitHub.Limit != defaults.GitHub.Limit {
		t.Errorf("limit = %d, want the default %d", cfg.GitHub.Limit, defaults.GitHub.Limit)
	}
	if !cfg.Herdr.Enabled || cfg.Herdr.Poll != defaults.Herdr.Poll {
		t.Errorf("herdr = %+v, want the defaults", cfg.Herdr)
	}
	if cfg.Events.TTL != defaults.Events.TTL {
		t.Errorf("event ttl = %s", cfg.Events.TTL)
	}
}

func TestLoadTurnsASourceOff(t *testing.T) {
	cfg, _, err := Load(write(t, "[herdr]\nenabled = false\n"), true)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Herdr.Enabled {
		t.Error("enabled = false did not take; a present false must beat the default true")
	}
}

func TestLoadRejectsAKeyItDoesNotKnow(t *testing.T) {
	// A silently ignored key is the worst outcome: the file says one thing and
	// the daemon does another.
	_, _, err := Load(write(t, "[github]\nrepose = [\"a/b\"]\n"), true)
	if err == nil {
		t.Fatal("a misspelled key was accepted")
	}
	if !strings.Contains(err.Error(), "repose") {
		t.Errorf("error = %v, want the offending key named", err)
	}
}

func TestLoadRejectsADurationItCannotParse(t *testing.T) {
	_, _, err := Load(write(t, "[github]\npoll = \"every minute\"\n"), true)
	if err == nil {
		t.Fatal("a nonsense duration was accepted")
	}
}

func TestLoadRejectsARouteThatDoesNotExist(t *testing.T) {
	// Falling back to the default here would deliver somewhere the file does
	// not name, which on a machine that silenced Herdr means silence.
	_, _, err := Load(write(t, "[notify]\nroute = \"osascript\"\n"), true)
	if err == nil {
		t.Fatal("an unknown route was accepted")
	}
	if !strings.Contains(err.Error(), "osascript") {
		t.Errorf("error = %v, want the offending route named", err)
	}
}

func TestStalePeriodIsOffUntilAFileNamesOne(t *testing.T) {
	if got := Default().Attention.StaleAfter.Std(); got != 0 {
		t.Fatalf("stale_after defaults to %s: work must not leave the board unasked", got)
	}

	cfg, _, err := Load(write(t, "[attention]\nstale_after = \"720h\"\nsnooze_for = \"90m\"\n"), true)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.Attention.StaleAfter.Std(); got != 720*time.Hour {
		t.Errorf("stale_after = %s, want 720h", got)
	}
	if got := cfg.Attention.SnoozeFor.Std(); got != 90*time.Minute {
		t.Errorf("snooze_for = %s, want 90m", got)
	}
}

func TestLoadRejectsANegativePeriod(t *testing.T) {
	// stale_after = "-720h" marks the whole board stale the moment it
	// appears, and nothing on screen would explain the empty dashboard.
	_, _, err := Load(write(t, "[attention]\nstale_after = \"-720h\"\n"), true)
	if err == nil {
		t.Fatal("a negative period was accepted")
	}
	if !strings.Contains(err.Error(), "attention.stale_after") {
		t.Errorf("error = %v, want the offending key named", err)
	}
}

func TestLoadTreatsAMissingFileAsAbsentOnlyWhenItWasNotAskedFor(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.toml")

	cfg, found, err := Load(missing, false)
	if err != nil || found {
		t.Fatalf("implicit missing file: found=%v err=%v", found, err)
	}
	if cfg.Daemon.Addr != Default().Daemon.Addr {
		t.Errorf("defaults not returned: %+v", cfg.Daemon)
	}

	if _, _, err := Load(missing, true); err == nil {
		t.Error("--config pointing at a missing file was accepted")
	}
}

func TestResolvePathTreatsAnEnvironmentPathAsAskedFor(t *testing.T) {
	t.Setenv("ATTENTIOND_CONFIG", "/tmp/somewhere.toml")
	path, explicit := ResolvePath("")
	if path != "/tmp/somewhere.toml" || !explicit {
		t.Errorf("ResolvePath() = %q, %v", path, explicit)
	}

	path, explicit = ResolvePath("/tmp/flag.toml")
	if path != "/tmp/flag.toml" || !explicit {
		t.Errorf("--config lost to the environment: %q, %v", path, explicit)
	}

	t.Setenv("ATTENTIOND_CONFIG", "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	path, explicit = ResolvePath("")
	if want := filepath.Join(home, ".attn", "config.toml"); path != want || explicit {
		t.Errorf("ResolvePath() = %q, %v, want %q, false", path, explicit, want)
	}
}

func TestAMissingEnvironmentPathFailsInsteadOfFallingBack(t *testing.T) {
	// Falling back here would drop the repository scope and start watching
	// every repository the token can see, which is the opposite of what the
	// missing file said.
	missing := filepath.Join(t.TempDir(), "gone.toml")
	t.Setenv("ATTENTIOND_CONFIG", missing)

	path, explicit := ResolvePath("")
	if !explicit {
		t.Fatal("an environment path was treated as implicit")
	}
	if _, _, err := Load(path, explicit); err == nil {
		t.Error("a missing $ATTENTIOND_CONFIG file was accepted")
	}
}

func TestGitHubIsOffUntilAFileTurnsItOn(t *testing.T) {
	if Default().GitHub.Enabled {
		t.Error("an unconfigured daemon would watch every repository the token can see")
	}

	cfg, _, err := Load(write(t, "[github]\nenabled = true\norgs = [\"example-org\"]\n"), true)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.GitHub.Enabled || len(cfg.GitHub.Orgs) != 1 {
		t.Errorf("github = %+v", cfg.GitHub)
	}
}

func TestSignalLogKeepsThirtyDaysUntilAFileSaysOtherwise(t *testing.T) {
	log := Default().Log
	if log.Path != "" || log.Retention.Std() != 720*time.Hour {
		t.Fatalf("defaults = %+v, want an empty path and 720h", log)
	}

	cfg, _, err := Load(write(t, "[log]\npath = \"/tmp/signals.jsonl\"\nretention = \"48h\"\n"), true)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Log.Path != "/tmp/signals.jsonl" || cfg.Log.Retention.Std() != 48*time.Hour {
		t.Fatalf("log = %+v", cfg.Log)
	}

	if _, _, err := Load(write(t, "[log]\nretention = \"-1h\"\n"), true); err == nil {
		t.Fatal("a negative retention was accepted")
	}
}

func TestDatadogDefaultsAndValidation(t *testing.T) {
	cfg, _, err := Load("", false)
	if err != nil {
		t.Fatal(err)
	}
	d := cfg.Datadog
	if d.Enabled || d.Site != "datadoghq.eu" || d.Poll.Std() != 30*time.Minute ||
		d.Query != "@finding_type:host_and_container_vulnerability @status:open" ||
		d.PageLimit != 100 || d.APIKeyEnv != "DD_API_KEY" || d.AppKeyEnv != "DD_APP_KEY" || d.Fixture != "" {
		t.Errorf("datadog defaults = %+v", d)
	}

	for body, want := range map[string]string{
		"[datadog]\nenabled = true\npage_limit = 0\n":    "datadog.page_limit",
		"[datadog]\nenabled = true\npage_limit = 1001\n": "datadog.page_limit",
		"[datadog]\nenabled = true\npoll = \"0s\"\n":     "datadog.poll",
		"[datadog]\nenabled = true\nsite = \"\"\n":       "datadog.site",
	} {
		if _, _, err := Load(write(t, body), true); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Load(%q) err = %v, want mention of %s", body, err, want)
		}
	}

	if _, _, err := Load(write(t, "[datadog]\nenabled = true\nsite = \"\"\nfixture = \"f.json\"\n"), true); err != nil {
		t.Errorf("a fixture needs no site: %v", err)
	}
}

func TestSlackDefaultsAndValidation(t *testing.T) {
	cfg, _, err := Load("", false)
	if err != nil {
		t.Fatal(err)
	}
	s := cfg.Slack
	if s.Enabled || s.TokenEnv != "SLACK_USER_TOKEN" || s.Poll.Std() != 5*time.Minute ||
		s.Lookback.Std() != 72*time.Hour || s.MaxConversations != 30 || s.Fixture != "" {
		t.Errorf("slack defaults = %+v", s)
	}

	for body, want := range map[string]string{
		"[slack]\nenabled = true\npoll = \"0s\"\n":         "slack.poll",
		"[slack]\nenabled = true\nlookback = \"0s\"\n":     "slack.lookback",
		"[slack]\nenabled = true\nmax_conversations = 0\n": "slack.max_conversations",
		"[slack]\nenabled = true\ntoken_env = \"\"\n":      "slack.token_env",
	} {
		if _, _, err := Load(write(t, body), true); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Load(%q) err = %v, want mention of %s", body, err, want)
		}
	}

	if _, _, err := Load(write(t, "[slack]\nenabled = true\ntoken_env = \"\"\nfixture = \"f.json\"\n"), true); err != nil {
		t.Errorf("a fixture needs no token variable: %v", err)
	}
}
