package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/devanmcgeer/attentiond/internal/attention"
	"github.com/devanmcgeer/attentiond/internal/config"
)

func TestDatadogSourceIsOffWithoutKeysAndLogsNoKeyData(t *testing.T) {
	t.Setenv("TEST_DD_API_KEY", "")
	t.Setenv("TEST_DD_APP_KEY", "never-logged")

	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	store := attention.NewStore(log, attention.StoreConfig{})

	cfg := config.Default().Datadog
	cfg.Enabled = true
	cfg.APIKeyEnv = "TEST_DD_API_KEY"
	cfg.AppKeyEnv = "TEST_DD_APP_KEY"

	poller, err := newDatadogPoller(cfg, store, log)
	if err != nil || poller != nil {
		t.Fatalf("poller = %v, err = %v, want a disabled source and no error", poller, err)
	}
	if !strings.Contains(logs.String(), "datadog adapter disabled") {
		t.Errorf("no warning that the source is off: %s", logs.String())
	}
	for _, leak := range []string{"never-logged", "TEST_DD_API_KEY", "TEST_DD_APP_KEY"} {
		if strings.Contains(logs.String(), leak) {
			t.Errorf("%q reached the log: %s", leak, logs.String())
		}
	}

	// A fixture needs no keys at all.
	t.Setenv("TEST_DD_APP_KEY", "")
	cfg.Fixture = "../../testdata/datadog-findings.json"
	poller, err = newDatadogPoller(cfg, store, log)
	if err != nil || poller == nil {
		t.Fatalf("fixture poller = %v, err = %v, want a poller without keys", poller, err)
	}

	cfg.Enabled = false
	if poller, _ := newDatadogPoller(cfg, store, log); poller != nil {
		t.Error("a disabled source built a poller")
	}
}

func TestSlackSourceIsOffWithoutTokenAndWarnsByName(t *testing.T) {
	t.Setenv("TEST_SLACK_TOKEN", "")

	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	store := attention.NewStore(log, attention.StoreConfig{})

	cfg := config.Default().Slack
	cfg.Enabled = true
	cfg.TokenEnv = "TEST_SLACK_TOKEN"

	poller, err := newSlackPoller(cfg, store, log)
	if err != nil || poller != nil {
		t.Fatalf("poller = %v, err = %v, want a disabled source and no error", poller, err)
	}
	if !strings.Contains(logs.String(), "TEST_SLACK_TOKEN") {
		t.Errorf("warning does not name the missing variable: %s", logs.String())
	}

	t.Setenv("TEST_SLACK_TOKEN", "never-logged")
	poller, err = newSlackPoller(cfg, store, log)
	if err != nil || poller == nil {
		t.Fatalf("poller = %v, err = %v, want a poller with a token", poller, err)
	}
	if strings.Contains(logs.String(), "never-logged") {
		t.Errorf("the token reached the log: %s", logs.String())
	}

	cfg.Enabled = false
	if poller, _ := newSlackPoller(cfg, store, log); poller != nil {
		t.Error("a disabled source built a poller")
	}
}

func TestOutlineSourceIsOffWithoutKeyAndWarnsByName(t *testing.T) {
	t.Setenv("TEST_OUTLINE_KEY", "")

	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	store := attention.NewStore(log, attention.StoreConfig{})

	cfg := config.Default().Outline
	cfg.Enabled = true
	cfg.BaseURL = "https://wiki.example.com"
	cfg.TokenEnv = "TEST_OUTLINE_KEY"

	poller, err := newOutlinePoller(cfg, store, log)
	if err != nil || poller != nil {
		t.Fatalf("poller = %v, err = %v, want a disabled source and no error", poller, err)
	}
	if !strings.Contains(logs.String(), "TEST_OUTLINE_KEY") {
		t.Errorf("warning does not name the missing variable: %s", logs.String())
	}

	cfg.Fixture = "../../testdata/outline-notifications.json"
	if poller, err := newOutlinePoller(cfg, store, log); err != nil || poller == nil {
		t.Fatalf("fixture poller = %v, err = %v, want a poller without a key", poller, err)
	}

	cfg.Enabled = false
	if poller, _ := newOutlinePoller(cfg, store, log); poller != nil {
		t.Error("a disabled source built a poller")
	}
}
