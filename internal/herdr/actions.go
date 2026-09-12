package herdr

import (
	"context"
	"fmt"

	"github.com/devanmcgeer/attentiond/internal/attention"
)

// Actions executes attentiond actions against a running Herdr server.
type Actions struct {
	client  *Client
	fixture bool
}

// NewActions returns the Herdr action executor. When fixture is true every call
// fails fast instead of dialing a socket that describes different state.
func NewActions(client *Client, fixture bool) *Actions {
	return &Actions{client: client, fixture: fixture}
}

// Execute runs one action. Herdr exposes no "focus pane by id" method:
// pane.focus_direction is directional, so a specific pane is reached by
// focusing its workspace and tab and then targeting the pane through
// agent.focus, which accepts a pane id.
func (a *Actions) Execute(ctx context.Context, kind, target, action string) error {
	if a.fixture {
		return fmt.Errorf("%w: herdr adapter is reading a fixture, not a live server",
			attention.ErrActionUnavailable)
	}
	if action != "focus" {
		return fmt.Errorf("%w: herdr %s/%s", attention.ErrActionUnsupported, kind, action)
	}

	var err error
	switch kind {
	case "workspace":
		err = a.call(ctx, "workspace.focus", map[string]string{"workspace_id": target}, nil)
	case "tab":
		err = a.focusTab(ctx, target)
	case "pane":
		err = a.focusPane(ctx, target)
	default:
		return fmt.Errorf("%w: herdr %s/%s", attention.ErrActionUnsupported, kind, action)
	}
	return err
}

func (a *Actions) focusTab(ctx context.Context, tabID string) error {
	var result struct {
		Tab Tab `json:"tab"`
	}
	if err := a.call(ctx, "tab.get", map[string]string{"tab_id": tabID}, &result); err != nil {
		return err
	}
	if err := a.call(ctx, "workspace.focus", map[string]string{"workspace_id": result.Tab.WorkspaceID}, nil); err != nil {
		return err
	}
	return a.call(ctx, "tab.focus", map[string]string{"tab_id": tabID}, nil)
}

func (a *Actions) focusPane(ctx context.Context, paneID string) error {
	var result struct {
		Pane struct {
			WorkspaceID string `json:"workspace_id"`
			TabID       string `json:"tab_id"`
			Agent       string `json:"agent"`
		} `json:"pane"`
	}
	if err := a.call(ctx, "pane.get", map[string]string{"pane_id": paneID}, &result); err != nil {
		return err
	}
	if err := a.call(ctx, "workspace.focus", map[string]string{"workspace_id": result.Pane.WorkspaceID}, nil); err != nil {
		return err
	}
	if err := a.call(ctx, "tab.focus", map[string]string{"tab_id": result.Pane.TabID}, nil); err != nil {
		return err
	}
	if result.Pane.Agent == "" {
		// Focusing the workspace and tab is as close as Herdr gets for a pane
		// with no detected agent.
		return nil
	}
	return a.call(ctx, "agent.focus", map[string]string{"target": paneID}, nil)
}

// call translates Herdr transport and protocol failures into the shared action
// vocabulary so the HTTP layer can pick a status code without knowing Herdr.
func (a *Actions) call(ctx context.Context, method string, params, out any) error {
	err := a.client.Call(ctx, method, params, out)
	switch {
	case err == nil:
		return nil
	case NotFound(err):
		return fmt.Errorf("%w: %s", attention.ErrActionTargetMissing, err)
	default:
		return fmt.Errorf("%w: %s", attention.ErrActionUnavailable, err)
	}
}
