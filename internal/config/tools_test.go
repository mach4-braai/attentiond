package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestToolsAreOffUntilAFileTurnsThemOn(t *testing.T) {
	tools := Default().Tools
	if tools.Enabled {
		t.Error("an unconfigured daemon would start scripts that push to your branches")
	}
	if tools.CommentQuiet.Std() != 2*time.Minute || tools.Timeouts["agent-comments"] == 0 {
		t.Errorf("defaults = %+v", tools)
	}
}

// `go run` puts the binary in a temporary directory, so a tools directory next
// to the binary would move every build. Relative paths follow the file.
func TestToolPathsResolveAgainstTheConfigFile(t *testing.T) {
	path := write(t, `
[tools]
enabled = true
dir = "tools"
timeouts = { rebase = "2m" }

[tools.checkouts]
"Didx-XYZ/Tofu" = "~/didx.projects/tofu"
"didx-xyz/mono" = "../mono"
`)
	cfg, _, err := Load(path, true)
	if err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	base := filepath.Dir(path)

	if cfg.Tools.Dir != filepath.Join(base, "tools") {
		t.Errorf("dir = %q, want it beside the config file", cfg.Tools.Dir)
	}
	if got := cfg.Tools.Checkouts["didx-xyz/tofu"]; got != filepath.Join(home, "didx.projects", "tofu") {
		t.Errorf("checkout = %q: ~ not expanded or key not folded (%v)", got, cfg.Tools.Checkouts)
	}
	if got := cfg.Tools.Checkouts["didx-xyz/mono"]; got != filepath.Join(filepath.Dir(base), "mono") {
		t.Errorf("relative checkout = %q", got)
	}
	if cfg.Tools.Timeouts["rebase"].Std() != 2*time.Minute || cfg.Tools.Timeouts["agent-rebase"] == 0 {
		t.Errorf("timeouts = %v: a tool left out has to keep its default", cfg.Tools.Timeouts)
	}
}

func TestToolsRejectWhatTheRunnerCannotUse(t *testing.T) {
	cases := map[string]string{
		"enabled with no dir": "[tools]\nenabled = true\n",
		"unknown tool":        "[tools]\ntimeouts = { rebsae = \"1m\" }\n",
		"zero timeout":        "[tools]\ntimeouts = { rebase = \"0s\" }\n",
		"bad checkout key":    "[tools.checkouts]\ntofu = \"/src/tofu\"\n",
		"negative quiet":      "[tools]\ncomment_quiet = \"-1m\"\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := Load(write(t, body), true)
			if err == nil || !strings.Contains(err.Error(), "tools.") {
				t.Fatalf("err = %v, want a tools.* error", err)
			}
		})
	}
}
