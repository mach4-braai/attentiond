package herdr

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/devanmcgeer/attentiond/internal/attention"
)

// fakeHerdr speaks enough of the Herdr socket protocol to record the method
// sequence attentiond drives.
type fakeHerdr struct {
	t        *testing.T
	listener net.Listener

	mu      sync.Mutex
	methods []string
	replies map[string]any
	fail    map[string]ErrorBody
}

func startFakeHerdr(t *testing.T) *fakeHerdr {
	t.Helper()
	// t.TempDir() embeds the test name, which overruns the 104-byte sun_path
	// limit on macOS, so keep the directory name short.
	dir, err := os.MkdirTemp("", "hd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	listener, err := net.Listen("unix", filepath.Join(dir, "h.sock"))
	if err != nil {
		t.Fatal(err)
	}

	fake := &fakeHerdr{
		t:        t,
		listener: listener,
		replies:  map[string]any{},
		fail:     map[string]ErrorBody{},
	}
	go fake.serve()
	t.Cleanup(func() { _ = listener.Close() })
	return fake
}

func (f *fakeHerdr) socket() string { return f.listener.Addr().String() }

func (f *fakeHerdr) serve() {
	for {
		conn, err := f.listener.Accept()
		if err != nil {
			return
		}
		go f.handle(conn)
	}
}

func (f *fakeHerdr) handle(conn net.Conn) {
	defer conn.Close()

	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return
	}
	var request struct {
		ID     string `json:"id"`
		Method string `json:"method"`
	}
	if err := json.Unmarshal(line, &request); err != nil {
		return
	}

	f.mu.Lock()
	f.methods = append(f.methods, request.Method)
	result, hasResult := f.replies[request.Method]
	failure, hasFailure := f.fail[request.Method]
	f.mu.Unlock()

	response := map[string]any{"id": request.ID}
	switch {
	case hasFailure:
		response["error"] = failure
	case hasResult:
		response["result"] = result
	default:
		response["result"] = map[string]string{"type": "ok"}
	}

	payload, _ := json.Marshal(response)
	_, _ = conn.Write(append(payload, '\n'))
}

func (f *fakeHerdr) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.methods...)
}

func requireCalls(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("herdr saw %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("herdr saw %v, want %v", got, want)
		}
	}
}

func TestFocusPaneWalksWorkspaceTabThenAgent(t *testing.T) {
	fake := startFakeHerdr(t)
	fake.replies["pane.get"] = map[string]any{
		"type": "pane_info",
		"pane": map[string]any{
			"pane_id":      "w2:p1",
			"workspace_id": "w2",
			"tab_id":       "w2:t1",
			"agent":        "codex",
		},
	}

	actions := NewActions(NewClient(fake.socket(), 2*time.Second), false)
	if err := actions.Execute(context.Background(), "pane", "w2:p1", "focus"); err != nil {
		t.Fatalf("focus failed: %v", err)
	}

	requireCalls(t, fake.calls(), []string{"pane.get", "workspace.focus", "tab.focus", "agent.focus"})
}

func TestFocusPaneWithoutAgentStopsAtTheTab(t *testing.T) {
	fake := startFakeHerdr(t)
	fake.replies["pane.get"] = map[string]any{
		"type": "pane_info",
		"pane": map[string]any{"pane_id": "w2:p2", "workspace_id": "w2", "tab_id": "w2:t1"},
	}

	actions := NewActions(NewClient(fake.socket(), 2*time.Second), false)
	if err := actions.Execute(context.Background(), "pane", "w2:p2", "focus"); err != nil {
		t.Fatalf("focus failed: %v", err)
	}

	// agent.focus against a pane with no agent would be rejected by Herdr.
	requireCalls(t, fake.calls(), []string{"pane.get", "workspace.focus", "tab.focus"})
}

func TestFocusClosedPaneReportsTargetMissing(t *testing.T) {
	fake := startFakeHerdr(t)
	fake.fail["pane.get"] = ErrorBody{Code: "not_found", Message: "pane not found"}

	actions := NewActions(NewClient(fake.socket(), 2*time.Second), false)
	err := actions.Execute(context.Background(), "pane", "w9:p9", "focus")
	if !errors.Is(err, attention.ErrActionTargetMissing) {
		t.Fatalf("closed pane reported as %v, want ErrActionTargetMissing", err)
	}
}

func TestUnknownActionIsRejectedWithoutTouchingHerdr(t *testing.T) {
	fake := startFakeHerdr(t)
	actions := NewActions(NewClient(fake.socket(), 2*time.Second), false)

	if err := actions.Execute(context.Background(), "pane", "w1:p1", "kill"); !errors.Is(err, attention.ErrActionUnsupported) {
		t.Fatalf("unknown action reported as %v", err)
	}
	if err := actions.Execute(context.Background(), "printer", "w1:p1", "focus"); !errors.Is(err, attention.ErrActionUnsupported) {
		t.Fatalf("unknown kind reported as %v", err)
	}
	if calls := fake.calls(); len(calls) != 0 {
		t.Fatalf("rejected actions still reached herdr: %v", calls)
	}
}

func TestFixtureModeRefusesToActOnStaleState(t *testing.T) {
	fake := startFakeHerdr(t)
	actions := NewActions(NewClient(fake.socket(), 2*time.Second), true)

	err := actions.Execute(context.Background(), "pane", "w1:p1", "focus")
	if !errors.Is(err, attention.ErrActionUnavailable) {
		t.Fatalf("fixture mode reported %v, want ErrActionUnavailable", err)
	}
	if calls := fake.calls(); len(calls) != 0 {
		t.Fatalf("fixture mode drove a live server: %v", calls)
	}
}

func TestSnapshotDecodesTheLiveResponseEnvelope(t *testing.T) {
	fake := startFakeHerdr(t)
	fake.replies["session.snapshot"] = map[string]any{
		"type": "session_snapshot",
		"snapshot": map[string]any{
			"version":  "0.9.0",
			"protocol": 22,
			"agents": []map[string]any{
				{"pane_id": "w1:p1", "workspace_id": "w1", "tab_id": "w1:t1", "agent_status": "blocked", "agent": "claude"},
			},
		},
	}

	snapshot, err := NewClient(fake.socket(), 2*time.Second).Snapshot(context.Background())
	if err != nil {
		t.Fatalf("snapshot failed: %v", err)
	}
	if len(snapshot.Agents) != 1 || snapshot.Agents[0].AgentStatus != "blocked" {
		t.Fatalf("snapshot decoded to %+v", snapshot)
	}
}
