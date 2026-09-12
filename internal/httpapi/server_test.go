package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/devanmcgeer/attentiond/internal/attention"
)

type recordingExecutor struct {
	calls []string
	err   error
}

func (e *recordingExecutor) Execute(_ context.Context, kind, target, action string) error {
	e.calls = append(e.calls, fmt.Sprintf("%s/%s/%s", kind, target, action))
	return e.err
}

func newTestServer(t *testing.T) (http.Handler, *attention.Store, *recordingExecutor) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := attention.NewStore(log, time.Hour)
	executor := &recordingExecutor{}

	handler := New(Config{
		Store: store,
		Sources: map[string]func() attention.SourceStatus{
			"herdr": func() attention.SourceStatus {
				return attention.SourceStatus{Mode: "fixture", Healthy: true, Items: 1}
			},
		},
		Actions: map[string]Executor{"herdr": executor},
		Version: "test",
		Started: time.Now().Add(-time.Minute),
		Log:     log,
	})
	return handler, store, executor
}

func do(t *testing.T, handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, path, reader)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func decodeList(t *testing.T, recorder *httptest.ResponseRecorder) listResponse {
	t.Helper()
	var response listResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode %s: %v", recorder.Body.String(), err)
	}
	return response
}

func TestEventBecomesVisibleWork(t *testing.T) {
	handler, _, _ := newTestServer(t)

	recorder := do(t, handler, http.MethodPost, "/api/events",
		`{"source":"tofu","id":"plan-prod","event":"needs_attention","title":"tofu plan wants approval","context":{"dir":"/repo"}}`)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("POST /api/events = %d: %s", recorder.Code, recorder.Body)
	}

	work := decodeList(t, do(t, handler, http.MethodGet, "/api/work", ""))
	if work.Count != 1 || work.AttentionCount != 1 {
		t.Fatalf("/api/work = %+v", work)
	}
	item := work.Items[0]
	if item.ID != "tofu:plan-prod" || item.State != attention.StateNeedsAttention {
		t.Fatalf("item = %+v", item)
	}
	if item.Severity != attention.SeverityWarning {
		t.Errorf("needs_attention defaulted to severity %q", item.Severity)
	}
	if item.Context["dir"] != "/repo" || item.Context["event"] != "needs_attention" {
		t.Errorf("context = %v", item.Context)
	}

	queue := decodeList(t, do(t, handler, http.MethodGet, "/api/attention", ""))
	if queue.Count != 1 {
		t.Fatalf("/api/attention = %+v", queue)
	}
}

func TestEventLifecycleUpdatesTheSameItem(t *testing.T) {
	handler, _, _ := newTestServer(t)

	for _, verb := range []string{"started", "working", "completed"} {
		body := fmt.Sprintf(`{"source":"ci","id":"build-7","event":%q}`, verb)
		if recorder := do(t, handler, http.MethodPost, "/api/events", body); recorder.Code != http.StatusAccepted {
			t.Fatalf("event %s = %d: %s", verb, recorder.Code, recorder.Body)
		}
	}

	work := decodeList(t, do(t, handler, http.MethodGet, "/api/work", ""))
	if work.Count != 1 {
		t.Fatalf("three events on one id produced %d items", work.Count)
	}
	if work.Items[0].State != attention.StateDone {
		t.Errorf("final state = %q, want done", work.Items[0].State)
	}
	if work.Items[0].Title != "ci build-7" {
		t.Errorf("untitled event got title %q", work.Items[0].Title)
	}
}

func TestEventRejectsUnusableInput(t *testing.T) {
	handler, _, _ := newTestServer(t)

	cases := map[string]struct {
		body string
		want int
	}{
		"unknown verb":     {`{"source":"ci","id":"1","event":"exploded"}`, http.StatusBadRequest},
		"missing id":       {`{"source":"ci","event":"started"}`, http.StatusBadRequest},
		"bad severity":     {`{"source":"ci","id":"1","event":"started","severity":"urgent"}`, http.StatusBadRequest},
		"unknown field":    {`{"source":"ci","id":"1","event":"started","stat":"working"}`, http.StatusBadRequest},
		"adapter's source": {`{"source":"herdr","id":"w1:p1","event":"working"}`, http.StatusConflict},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if recorder := do(t, handler, http.MethodPost, "/api/events", tc.body); recorder.Code != tc.want {
				t.Fatalf("got %d want %d: %s", recorder.Code, tc.want, recorder.Body)
			}
		})
	}
}

func TestEventURLBecomesAnOpenAction(t *testing.T) {
	handler, _, _ := newTestServer(t)
	do(t, handler, http.MethodPost, "/api/events",
		`{"source":"ci","id":"build-7","event":"failed","url":"https://ci.example/build/7"}`)

	work := decodeList(t, do(t, handler, http.MethodGet, "/api/work", ""))
	actions := work.Items[0].Actions
	if len(actions) != 1 || actions[0].Href != "https://ci.example/build/7" {
		t.Fatalf("actions = %+v", actions)
	}
	if work.Items[0].Severity != attention.SeverityCritical {
		t.Errorf("failed defaulted to severity %q", work.Items[0].Severity)
	}
}

func TestItemsCarryTheDerivedAttentionFlag(t *testing.T) {
	handler, _, _ := newTestServer(t)
	do(t, handler, http.MethodPost, "/api/events", `{"source":"ci","id":"a","event":"working"}`)
	do(t, handler, http.MethodPost, "/api/events", `{"source":"ci","id":"b","event":"failed"}`)

	var raw struct {
		Items []struct {
			ID        string `json:"id"`
			Attention bool   `json:"attention"`
		} `json:"items"`
	}
	recorder := do(t, handler, http.MethodGet, "/api/work", "")
	if err := json.Unmarshal(recorder.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}

	flags := map[string]bool{}
	for _, item := range raw.Items {
		flags[item.ID] = item.Attention
	}
	if flags["ci:a"] || !flags["ci:b"] {
		t.Fatalf("attention flags = %v", flags)
	}
}

func TestActionReachesTheRegisteredExecutor(t *testing.T) {
	handler, _, executor := newTestServer(t)

	recorder := do(t, handler, http.MethodPost, "/api/actions/herdr/pane/w1:p1/focus", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("action = %d: %s", recorder.Code, recorder.Body)
	}
	if len(executor.calls) != 1 || executor.calls[0] != "pane/w1:p1/focus" {
		t.Fatalf("executor saw %v", executor.calls)
	}
}

func TestActionFailuresMapToStatusCodes(t *testing.T) {
	cases := map[string]struct {
		err  error
		want int
	}{
		"missing target": {attention.ErrActionTargetMissing, http.StatusNotFound},
		"unsupported":    {attention.ErrActionUnsupported, http.StatusBadRequest},
		"unavailable":    {attention.ErrActionUnavailable, http.StatusServiceUnavailable},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			handler, _, executor := newTestServer(t)
			executor.err = tc.err
			recorder := do(t, handler, http.MethodPost, "/api/actions/herdr/pane/w1:p1/focus", "")
			if recorder.Code != tc.want {
				t.Fatalf("got %d want %d: %s", recorder.Code, tc.want, recorder.Body)
			}
		})
	}
}

func TestActionForUnregisteredSourceIs404(t *testing.T) {
	handler, _, _ := newTestServer(t)
	if recorder := do(t, handler, http.MethodPost, "/api/actions/github/pr/7/merge", ""); recorder.Code != http.StatusNotFound {
		t.Fatalf("unregistered source = %d", recorder.Code)
	}
}

func TestHealthReportsAdapterStateWhileStayingOK(t *testing.T) {
	handler, _, _ := newTestServer(t)
	do(t, handler, http.MethodPost, "/api/events", `{"source":"ci","id":"a","event":"working"}`)

	recorder := do(t, handler, http.MethodGet, "/health", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("/health = %d", recorder.Code)
	}

	var response healthResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Status != "ok" || response.Version != "test" {
		t.Fatalf("health = %+v", response)
	}
	if response.Items != 1 {
		t.Errorf("health item count = %d, want 1", response.Items)
	}
	if response.Sources["herdr"].Mode != "fixture" {
		t.Errorf("health sources = %+v", response.Sources)
	}
	if response.UptimeSeconds < 59 {
		t.Errorf("uptime = %d", response.UptimeSeconds)
	}
}
