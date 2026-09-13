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
orgs = ["didx-xyz", "mach4-braai"]
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

	cfg, _, err := Load(write(t, "[github]\nenabled = true\norgs = [\"didx-xyz\"]\n"), true)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.GitHub.Enabled || len(cfg.GitHub.Orgs) != 1 {
		t.Errorf("github = %+v", cfg.GitHub)
	}
}
