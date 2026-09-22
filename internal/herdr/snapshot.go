package herdr

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/devanmcgeer/attentiond/internal/attention"
)

// SessionSnapshot is the subset of Herdr's session.snapshot result that
// attentiond needs. Herdr's protocol tells clients to ignore unknown fields, so
// layouts, scroll metrics and graphics state are deliberately not decoded.
type SessionSnapshot struct {
	Version    string      `json:"version"`
	Protocol   int         `json:"protocol"`
	Workspaces []Workspace `json:"workspaces"`
	Tabs       []Tab       `json:"tabs"`
	Agents     []Agent     `json:"agents"`
}

// Workspace is Herdr's WorkspaceInfo.
type Workspace struct {
	WorkspaceID string `json:"workspace_id"`
	Label       string `json:"label"`
	Focused     bool   `json:"focused"`
}

// Tab is Herdr's TabInfo.
type Tab struct {
	TabID       string `json:"tab_id"`
	WorkspaceID string `json:"workspace_id"`
	Label       string `json:"label"`
}

// Agent is Herdr's AgentInfo: one detected coding agent and the pane hosting it.
type Agent struct {
	PaneID                string `json:"pane_id"`
	WorkspaceID           string `json:"workspace_id"`
	TabID                 string `json:"tab_id"`
	AgentStatus           string `json:"agent_status"`
	Agent                 string `json:"agent"`
	Name                  string `json:"name"`
	DisplayAgent          string `json:"display_agent"`
	Title                 string `json:"title"`
	TerminalTitleStripped string `json:"terminal_title_stripped"`
	CWD                   string `json:"cwd"`
	ForegroundCWD         string `json:"foreground_cwd"`
	Focused               bool   `json:"focused"`
}

// snapshotResult is the session.snapshot success response body.
type snapshotResult struct {
	Type     string          `json:"type"`
	Snapshot SessionSnapshot `json:"snapshot"`
}

// Snapshot fetches the live session snapshot.
func (c *Client) Snapshot(ctx context.Context) (SessionSnapshot, error) {
	var result snapshotResult
	if err := c.Call(ctx, "session.snapshot", nil, &result); err != nil {
		return SessionSnapshot{}, err
	}
	return result.Snapshot, nil
}

// DecodeFixture reads a snapshot recorded with `herdr api snapshot`. It accepts
// the full response envelope, a bare result object, or a bare snapshot, because
// all three are plausible things to have on disk.
func DecodeFixture(raw []byte) (SessionSnapshot, error) {
	var envelope struct {
		Result   *snapshotResult  `json:"result"`
		Snapshot *SessionSnapshot `json:"snapshot"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return SessionSnapshot{}, fmt.Errorf("decode herdr fixture: %w", err)
	}
	switch {
	case envelope.Result != nil:
		return envelope.Result.Snapshot, nil
	case envelope.Snapshot != nil:
		return *envelope.Snapshot, nil
	}

	var bare SessionSnapshot
	if err := json.Unmarshal(raw, &bare); err != nil {
		return SessionSnapshot{}, fmt.Errorf("decode herdr fixture: %w", err)
	}
	if bare.Version == "" && len(bare.Agents) == 0 && len(bare.Workspaces) == 0 {
		return SessionSnapshot{}, fmt.Errorf("herdr fixture holds no session snapshot")
	}
	return bare, nil
}

// mapStatus translates Herdr's semantic agent status. Herdr never reports a
// failure, so attention.StateFailed only arrives through /api/events.
//
// The label is Herdr's own word rather than a translation of it. The same pane
// appears in Herdr's agent sidebar, and a pane that reads "blocked" there and
// "needs you" on the dashboard costs a human the moment it takes to notice
// they are the same thing.
func mapStatus(status string) (attention.State, attention.Severity, string, attention.Tone) {
	switch status {
	case "working":
		return attention.StateWorking, attention.SeverityInfo, "working", attention.ToneActive
	case "blocked":
		return attention.StateNeedsAttention, attention.SeverityWarning, "blocked", attention.ToneAttention
	case "done":
		return attention.StateDone, attention.SeverityInfo, "done", attention.ToneDone
	case "idle":
		return attention.StateWaiting, attention.SeverityInfo, "idle", attention.ToneNeutral
	default:
		// "unknown" means an agent is present but Herdr cannot classify it, not
		// that anything went wrong.
		return attention.StateWaiting, attention.SeverityInfo, "unknown", attention.ToneNeutral
	}
}

// Normalize turns a session snapshot into attention items, one per detected
// agent. Panes running an ordinary shell are not work that needs attention.
func Normalize(snapshot SessionSnapshot, baseURL string, now time.Time) []attention.Item {
	workspaces := make(map[string]Workspace, len(snapshot.Workspaces))
	for _, workspace := range snapshot.Workspaces {
		workspaces[workspace.WorkspaceID] = workspace
	}
	tabs := make(map[string]Tab, len(snapshot.Tabs))
	for _, tab := range snapshot.Tabs {
		tabs[tab.TabID] = tab
	}

	items := make([]attention.Item, 0, len(snapshot.Agents))
	for _, agent := range snapshot.Agents {
		state, severity, label, tone := mapStatus(agent.AgentStatus)
		workspace := workspaces[agent.WorkspaceID]
		workspaceLabel := firstNonEmpty(workspace.Label, agent.WorkspaceID)
		meta := map[string]string{
			"workspace_id":    agent.WorkspaceID,
			"workspace_label": workspaceLabel,
			"tab_id":          agent.TabID,
			"pane_id":         agent.PaneID,
			"herdr_status":    agent.AgentStatus,
		}
		if tab, ok := tabs[agent.TabID]; ok && tab.Label != "" {
			meta["tab_label"] = tab.Label
		}
		putIfSet(meta, "agent", agent.Agent)
		putIfSet(meta, "agent_name", agent.Name)
		putIfSet(meta, "cwd", firstNonEmpty(agent.ForegroundCWD, agent.CWD))

		items = append(items, attention.Item{
			ID:        attention.Key(SourceName, agent.PaneID),
			Source:    SourceName,
			Title:     workspaceLabel + " · " + agentLabel(agent),
			State:     state,
			Severity:  severity,
			Label:     label,
			Tone:      tone,
			Priority:  attention.DefaultPriority(state),
			Context:   meta,
			UpdatedAt: now,
			Actions: []attention.Action{{
				ID:     "focus",
				Label:  "Open",
				Method: "POST",
				Href:   strings.TrimSuffix(baseURL, "/") + "/api/actions/herdr/pane/" + agent.PaneID + "/focus",
			}},
		})
	}

	sort.Slice(items, func(a, b int) bool { return items[a].ID < items[b].ID })
	return items
}

// agentLabel picks the most specific name Herdr knows for an agent: an explicit
// metadata title, then a name assigned through `herdr agent start`, then the
// display name, the agent kind, and finally whatever the terminal is calling
// itself.
func agentLabel(agent Agent) string {
	return firstNonEmpty(agent.Title, agent.Name, agent.DisplayAgent, agent.Agent, agent.TerminalTitleStripped, "agent")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func putIfSet(target map[string]string, key, value string) {
	if value != "" {
		target[key] = value
	}
}
