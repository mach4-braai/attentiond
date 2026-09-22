package herdr

import (
	"os"
	"testing"
	"time"

	"github.com/devanmcgeer/attentiond/internal/attention"
)

func loadFixture(t *testing.T) SessionSnapshot {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/session-snapshot.json")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := DecodeFixture(raw)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestMapStatus(t *testing.T) {
	cases := []struct {
		herdr    string
		state    attention.State
		severity attention.Severity
		label    string
		tone     attention.Tone
	}{
		{"working", attention.StateWorking, attention.SeverityInfo, "working", attention.ToneActive},
		{"idle", attention.StateWaiting, attention.SeverityInfo, "idle", attention.ToneNeutral},
		{"blocked", attention.StateNeedsAttention, attention.SeverityWarning, "blocked", attention.ToneAttention},
		{"done", attention.StateDone, attention.SeverityInfo, "done", attention.ToneDone},
		{"unknown", attention.StateWaiting, attention.SeverityInfo, "unknown", attention.ToneNeutral},
		{"something-herdr-added-later", attention.StateWaiting, attention.SeverityInfo, "unknown", attention.ToneNeutral},
	}

	for _, tc := range cases {
		state, severity, label, tone := mapStatus(tc.herdr)
		if state != tc.state || severity != tc.severity {
			t.Errorf("mapStatus(%q) = %q/%q, want %q/%q",
				tc.herdr, state, severity, tc.state, tc.severity)
		}
		// The label is Herdr's own word so that the same pane reads the same
		// in Herdr's sidebar and on the dashboard.
		if label != tc.label || tone != tc.tone {
			t.Errorf("mapStatus(%q) displayed %q/%q, want %q/%q",
				tc.herdr, label, tone, tc.label, tc.tone)
		}
	}
}

func TestNormalizeReportsAgentsNotPanes(t *testing.T) {
	snapshot := loadFixture(t)
	items := Normalize(snapshot, "http://127.0.0.1:7717", time.Now())

	if len(items) != len(snapshot.Agents) {
		t.Fatalf("normalized %d items from %d agents and %d panes; plain shells must not become work",
			len(items), len(snapshot.Agents), 4)
	}
	for _, item := range items {
		if item.Source != SourceName {
			t.Errorf("item %s has source %q", item.ID, item.Source)
		}
	}
}

func TestNormalizeCarriesHerdrIdentityAndFocusAction(t *testing.T) {
	items := Normalize(loadFixture(t), "http://127.0.0.1:7717/", time.Now())

	byID := map[string]attention.Item{}
	for _, item := range items {
		byID[item.ID] = item
	}

	blocked, ok := byID["herdr:w2:p1"]
	if !ok {
		t.Fatalf("blocked agent missing from %v", byID)
	}
	if blocked.State != attention.StateNeedsAttention || blocked.Severity != attention.SeverityWarning {
		t.Errorf("blocked agent normalized to %s/%s", blocked.State, blocked.Severity)
	}
	if !blocked.State.NeedsAttention() {
		t.Error("blocked agent is not in the attention queue")
	}

	// The identifiers needed to return to the Herdr context must survive.
	for key, want := range map[string]string{
		"workspace_id":    "w2",
		"workspace_label": "tofu",
		"tab_id":          "w2:t1",
		"tab_label":       "plan",
		"pane_id":         "w2:p1",
		"agent":           "codex",
		"herdr_status":    "blocked",
	} {
		if got := blocked.Context[key]; got != want {
			t.Errorf("context[%s] = %q, want %q", key, got, want)
		}
	}

	if len(blocked.Actions) != 1 {
		t.Fatalf("expected one action, got %+v", blocked.Actions)
	}
	action := blocked.Actions[0]
	want := "http://127.0.0.1:7717/api/actions/herdr/pane/w2:p1/focus"
	if action.Href != want {
		t.Errorf("action href = %q, want %q", action.Href, want)
	}
	if action.Method != "POST" {
		t.Errorf("action method = %q, want POST", action.Method)
	}
}

func TestNormalizeTitlePrefersTheMostSpecificName(t *testing.T) {
	items := Normalize(loadFixture(t), "http://127.0.0.1:7717", time.Now())
	titles := map[string]string{}
	for _, item := range items {
		titles[item.ID] = item.Title
	}

	cases := map[string]string{
		// reported metadata title wins over the agent kind
		"herdr:w1:p2": "attentiond · Port the snapshot decoder",
		// an explicitly named agent wins over its kind and terminal title
		"herdr:w1:p1": "attentiond · reviewer",
		// display_agent wins over the bare kind
		"herdr:w2:p1": "tofu · Codex: plan",
	}
	for id, want := range cases {
		if titles[id] != want {
			t.Errorf("title for %s = %q, want %q", id, titles[id], want)
		}
	}
}

func TestNormalizeFallsBackToWorkspaceIDWhenUnlabelled(t *testing.T) {
	snapshot := SessionSnapshot{
		Agents: []Agent{{PaneID: "w9:p1", WorkspaceID: "w9", TabID: "w9:t1", AgentStatus: "idle"}},
	}
	items := Normalize(snapshot, "http://127.0.0.1:7717", time.Now())
	if len(items) != 1 {
		t.Fatalf("got %d items", len(items))
	}
	if items[0].Title != "w9 · agent" {
		t.Errorf("title = %q, want %q", items[0].Title, "w9 · agent")
	}
}

func TestDecodeFixtureAcceptsEnvelopeAndBareSnapshot(t *testing.T) {
	bare := []byte(`{"version":"0.9.0","protocol":22,"workspaces":[{"workspace_id":"w1","label":"x"}],"agents":[]}`)
	snapshot, err := DecodeFixture(bare)
	if err != nil {
		t.Fatalf("bare snapshot rejected: %v", err)
	}
	if len(snapshot.Workspaces) != 1 {
		t.Errorf("bare snapshot decoded to %+v", snapshot)
	}

	if _, err := DecodeFixture([]byte(`{"unrelated":true}`)); err == nil {
		t.Error("a file with no snapshot in it was accepted")
	}
}
