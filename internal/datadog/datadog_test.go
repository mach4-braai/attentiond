package datadog

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/devanmcgeer/attentiond/internal/attention"
)

const fixturePath = "../../testdata/datadog-findings.json"

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func byTitle(t *testing.T, items []attention.Item, fragment string) attention.Item {
	t.Helper()
	for _, item := range items {
		if strings.Contains(item.Title, fragment) {
			return item
		}
	}
	t.Fatalf("no item titled like %q in %d items", fragment, len(items))
	return attention.Item{}
}

func fixtureItems(t *testing.T) []attention.Item {
	t.Helper()
	result, err := ReadFixture(fixturePath)
	if err != nil {
		t.Fatalf("ReadFixture: %v", err)
	}
	return Normalize(result.Findings, time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC))
}

func TestMapsEveryContractField(t *testing.T) {
	items := fixtureItems(t)
	if len(items) != 4 {
		t.Fatalf("got %d items, want 4", len(items))
	}

	item := byTitle(t, items, "CVE-2024-45337")
	if item.ID != "datadog:AZxkQ2mFAAC8vN0dRt4PqwAA" || item.Source != "datadog" {
		t.Errorf("id/source = %q/%q", item.ID, item.Source)
	}
	if item.Title != "Authorization bypass in golang.org/x/crypto/ssh (CVE-2024-45337)" {
		t.Errorf("title = %q", item.Title)
	}
	if item.Label != "vulnerability" || item.State != attention.StateNeedsAttention {
		t.Errorf("label/state = %q/%q", item.Label, item.State)
	}
	if item.Priority != attention.DefaultPriority(attention.StateNeedsAttention) {
		t.Errorf("priority = %d, want the state default", item.Priority)
	}
	if item.Severity != attention.SeverityCritical {
		t.Errorf("severity = %q", item.Severity)
	}
	if len(item.Actions) != 0 {
		t.Errorf("actions = %+v, want none", item.Actions)
	}

	want := map[string]string{
		"finding_id":                "AZxkQ2mFAAC8vN0dRt4PqwAA",
		"finding_type":              "host_and_container_vulnerability",
		"severity":                  "critical",
		"image":                     "123456789012.dkr.ecr.eu-west-1.amazonaws.com/web-api:1.2.3",
		"repo_digest":               "sha256:3f1d5c0a9b7e42d68c1f0e7a5b9d2c4e8a6f1b3d5c7e9a0b2d4f6e8c1a3b5d7f",
		"package":                   "golang.org/x/crypto",
		"package_version":           "0.29.0",
		"fix_available":             "true",
		"fixed_version":             "0.31.0",
		"inherited_from_base_image": "false",
		"ecosystem":                 "gobinary",
		"status":                    "open",
		"first_seen_at":             time.UnixMilli(1759480000000).UTC().Format(time.RFC3339),
	}
	if len(item.Context) != len(want) {
		t.Errorf("context has %d keys, want %d: %v", len(item.Context), len(want), item.Context)
	}
	for key, value := range want {
		if got := item.Context[key]; got != value {
			t.Errorf("context[%s] = %q, want %q", key, got, value)
		}
	}
}

func TestFixedVersionFallsBackAndFixAbsent(t *testing.T) {
	items := fixtureItems(t)

	// No remediation.package.base, so the closest clean version answers.
	inherited := byTitle(t, items, "CVE-2024-56171")
	if got := inherited.Context["fixed_version"]; got != "2.9.14+dfsg-1.3~deb12u2" {
		t.Errorf("fixed_version = %q, want the closest_no_vulnerabilities fallback", got)
	}
	if got := inherited.Context["inherited_from_base_image"]; got != "true" {
		t.Errorf("inherited_from_base_image = %q", got)
	}

	// Same image as the first finding, no fix: fix_available is false and
	// there is no fixed_version key at all.
	noFix := byTitle(t, items, "CVE-2025-22871")
	if got := noFix.Context["fix_available"]; got != "false" {
		t.Errorf("fix_available = %q, want false", got)
	}
	if _, ok := noFix.Context["fixed_version"]; ok {
		t.Errorf("fixed_version present without a fix: %v", noFix.Context)
	}
	if noFix.Context["image"] != byTitle(t, items, "CVE-2024-45337").Context["image"] {
		t.Errorf("the two findings should share an image")
	}

	other := byTitle(t, items, "CVE-2025-27789")
	if other.Context["image"] == noFix.Context["image"] {
		t.Errorf("fourth finding should be in a different image")
	}
}

func TestOmitsWhatDatadogDidNotSay(t *testing.T) {
	resp, err := Decode([]byte(`{"data":[{"id":"envelope-id","type":"finding",
	  "attributes":{"attributes":{"title":"bare","severity":"unknown"}}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	items := Normalize(resp.Findings(), now)
	if len(items) != 1 {
		t.Fatalf("got %d items", len(items))
	}
	item := items[0]

	// Falls back to the envelope id when the finding carries none.
	if item.ID != "datadog:envelope-id" || item.Context["finding_id"] != "envelope-id" {
		t.Errorf("id = %q, context = %v", item.ID, item.Context)
	}
	if item.Severity != attention.SeverityInfo {
		t.Errorf("unknown severity = %q, want info", item.Severity)
	}
	for _, key := range []string{
		"finding_type", "image", "repo_digest", "package", "package_version",
		"fix_available", "fixed_version", "inherited_from_base_image",
		"ecosystem", "status", "first_seen_at",
	} {
		if _, ok := item.Context[key]; ok {
			t.Errorf("context[%s] present, want omitted", key)
		}
	}
	if !item.UpdatedAt.Equal(now) {
		t.Errorf("updated_at = %v, want poll time when first_seen_at is absent", item.UpdatedAt)
	}
}

func TestSeverityMapping(t *testing.T) {
	for datadog, want := range map[string]attention.Severity{
		"critical": attention.SeverityCritical,
		"high":     attention.SeverityCritical,
		"medium":   attention.SeverityWarning,
		"low":      attention.SeverityInfo,
		"info":     attention.SeverityInfo,
		"":         attention.SeverityInfo,
		"surprise": attention.SeverityInfo,
	} {
		if got := severity(datadog); got != want {
			t.Errorf("severity(%q) = %q, want %q", datadog, got, want)
		}
	}
}

func TestNormalizeSkipsUnkeyedAndRepeatedFindings(t *testing.T) {
	findings := []Finding{
		{FindingID: "a", Title: "first"},
		{FindingID: "a", Title: "again"},
		{Title: "no id"},
	}
	items := Normalize(findings, time.Now())
	if len(items) != 1 || items[0].Title != "first" {
		t.Fatalf("items = %+v", items)
	}
}

// pagedServer serves two pages: the first points at cursor "page-2", the
// second ends the search. It records each request.
func pagedServer(t *testing.T) (*httptest.Server, func() []*http.Request) {
	t.Helper()
	var mu sync.Mutex
	var requests []*http.Request

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.Clone(context.Background()))
		mu.Unlock()

		record := func(id string) string {
			return fmt.Sprintf(`{"id":%q,"type":"finding","attributes":{"attributes":{"finding_id":%q,"title":"t-%s","severity":"high"}}}`, id, id, id)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("page[cursor]") {
		case "":
			fmt.Fprintf(w, `{"data":[%s,%s],"meta":{"page":{"after":"page-2"},"status":"done"}}`, record("one"), record("two"))
		case "page-2":
			fmt.Fprintf(w, `{"data":[%s],"meta":{"page":{},"status":"done"}}`, record("three"))
		default:
			http.Error(w, "unexpected cursor", http.StatusBadRequest)
		}
	}))
	t.Cleanup(server.Close)
	return server, func() []*http.Request {
		mu.Lock()
		defer mu.Unlock()
		return append([]*http.Request(nil), requests...)
	}
}

func TestFindingsFollowsCursorsAndStops(t *testing.T) {
	server, requests := pagedServer(t)

	result, err := NewClient(server.URL, "api-secret", "app-secret", 5*time.Second).
		Findings(context.Background(), "@status:open", 2)
	if err != nil {
		t.Fatalf("Findings: %v", err)
	}
	if len(result.Findings) != 3 || result.Partial {
		t.Fatalf("result = %+v, want 3 findings, not partial", result)
	}

	got := requests()
	if len(got) != 2 {
		t.Fatalf("issued %d requests, want 2", len(got))
	}
	first, second := got[0], got[1]
	if first.URL.Path != "/api/v2/security/findings" {
		t.Errorf("path = %q", first.URL.Path)
	}
	if q := first.URL.Query(); q.Get("filter[query]") != "@status:open" ||
		q.Get("page[limit]") != "2" || q.Has("page[cursor]") {
		t.Errorf("first query = %v", q)
	}
	if q := second.URL.Query(); q.Get("page[cursor]") != "page-2" ||
		q.Get("filter[query]") != "@status:open" || q.Get("page[limit]") != "2" {
		t.Errorf("second query = %v", q)
	}
}

func TestFindingsFlagsATimeoutAsPartial(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[],"meta":{"page":{},"status":"timeout"}}`))
	}))
	defer server.Close()

	result, err := NewClient(server.URL, "k", "a", time.Second).Findings(context.Background(), "", 10)
	if err != nil || !result.Partial {
		t.Fatalf("result = %+v, err = %v, want partial", result, err)
	}
}

func TestFindingsStopsOnARepeatedCursor(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[],"meta":{"page":{"after":"same"}}}`))
	}))
	defer server.Close()

	_, err := NewClient(server.URL, "k", "a", time.Second).Findings(context.Background(), "", 10)
	if err == nil || !strings.Contains(err.Error(), "cursor repeated") {
		t.Fatalf("err = %v, want a repeated-cursor error", err)
	}
}

func TestSendsBothKeysAndNeverLeaksThem(t *testing.T) {
	const apiKey, appKey = "api-key-s3cret", "app-key-s3cret"

	var gotAPI, gotApp string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAPI = r.Header.Get("DD-API-KEY")
		gotApp = r.Header.Get("DD-APPLICATION-KEY")
		// A misbehaving proxy that echoes the credentials into its error.
		http.Error(w, "forbidden for "+gotAPI+" / "+gotApp, http.StatusForbidden)
	}))
	defer server.Close()

	_, err := NewClient(server.URL, apiKey, appKey, 5*time.Second).Findings(context.Background(), "q", 10)
	if gotAPI != apiKey || gotApp != appKey {
		t.Errorf("headers = %q / %q, want both keys sent", gotAPI, gotApp)
	}
	if err == nil {
		t.Fatal("want an error for 403")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("err = %v, want the status", err)
	}
	for _, key := range []string{apiKey, appKey} {
		if strings.Contains(err.Error(), key) {
			t.Errorf("error leaks a key: %v", err)
		}
	}

	// A transport failure carries the URL, never the headers.
	server.Close()
	_, err = NewClient(server.URL, apiKey, appKey, time.Second).Findings(context.Background(), "q", 10)
	if err == nil {
		t.Fatal("want a connection error")
	}
	for _, key := range []string{apiKey, appKey} {
		if strings.Contains(err.Error(), key) {
			t.Errorf("transport error leaks a key: %v", err)
		}
	}
}

// logBuffer collects log output so a test can look for a key in it.
type logBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func TestFailedPollKeepsLastGoodItemsAndNeverLogsKeys(t *testing.T) {
	const apiKey, appKey = "api-key-s3cret", "app-key-s3cret"

	var mu sync.Mutex
	fail := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		failing := fail
		mu.Unlock()
		if failing {
			http.Error(w, "boom "+r.Header.Get("DD-API-KEY"), http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"x","type":"finding","attributes":{"attributes":{"finding_id":"x","title":"kept","severity":"high"}}}],"meta":{"page":{}}}`))
	}))
	defer server.Close()

	logs := &logBuffer{}
	store := attention.NewStore(discardLogger(), attention.StoreConfig{})
	poller := NewPoller(NewClient(server.URL, apiKey, appKey, 5*time.Second), store,
		PollerConfig{Interval: time.Hour, Query: "q", PageLimit: 10},
		slog.New(slog.NewTextHandler(logs, nil)))

	poller.pollOnce(context.Background())
	if got := store.Items(); len(got) != 1 || got[0].Title != "kept" {
		t.Fatalf("items after success = %+v", got)
	}
	if status := poller.SourceStatus(); !status.Healthy || status.Items != 1 || status.Mode != "api" {
		t.Fatalf("status = %+v", status)
	}

	mu.Lock()
	fail = true
	mu.Unlock()
	poller.pollOnce(context.Background())

	if got := store.Items(); len(got) != 1 || got[0].Title != "kept" {
		t.Errorf("items after failure = %+v, want the last good result", got)
	}
	status := poller.SourceStatus()
	if status.Healthy || status.LastError == "" || status.Items != 1 {
		t.Errorf("status after failure = %+v", status)
	}
	for _, secret := range []string{apiKey, appKey} {
		if strings.Contains(status.LastError, secret) || strings.Contains(logs.String(), secret) {
			t.Errorf("a key reached the status or the logs: %q / %q", status.LastError, logs.String())
		}
	}
}

func TestFixtureModeNeedsNoClientAndRereadsEachPoll(t *testing.T) {
	path := filepath.Join(t.TempDir(), "findings.json")
	write := func(body string) {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"data":[{"id":"a","attributes":{"attributes":{"title":"one"}}}]}`)

	store := attention.NewStore(discardLogger(), attention.StoreConfig{})
	poller := NewPoller(nil, store, PollerConfig{Interval: time.Hour, Fixture: path}, discardLogger())

	poller.pollOnce(context.Background())
	if got := store.Items(); len(got) != 1 || got[0].Title != "one" {
		t.Fatalf("items = %+v", got)
	}
	if status := poller.SourceStatus(); status.Mode != "fixture" || !status.Healthy {
		t.Fatalf("status = %+v", status)
	}

	write(`{"data":[{"id":"b","attributes":{"attributes":{"title":"two"}}}]}`)
	poller.pollOnce(context.Background())
	got := store.Items()
	if len(got) != 1 || got[0].Title != "two" {
		t.Fatalf("items after rewrite = %+v, want the file re-read", got)
	}

	// The repository fixture loads too.
	full := NewPoller(nil, store, PollerConfig{Interval: time.Hour, Fixture: fixturePath}, discardLogger())
	full.pollOnce(context.Background())
	if n := len(store.Items()); n != 4 {
		t.Errorf("recorded fixture produced %d items, want 4", n)
	}
}

func TestResolveKeysNamesWhatIsMissingNeverTheValue(t *testing.T) {
	t.Setenv("TEST_DD_API", "")
	t.Setenv("TEST_DD_APP", "app-s3cret")

	_, _, err := ResolveKeys("TEST_DD_API", "TEST_DD_APP")
	if err == nil || !strings.Contains(err.Error(), "TEST_DD_API") {
		t.Fatalf("err = %v, want the missing variable named", err)
	}
	if strings.Contains(err.Error(), "TEST_DD_APP") || strings.Contains(err.Error(), "app-s3cret") {
		t.Errorf("err mentions the key that is set: %v", err)
	}

	t.Setenv("TEST_DD_API", " api-s3cret ")
	api, app, err := ResolveKeys("TEST_DD_API", "TEST_DD_APP")
	if err != nil || api != "api-s3cret" || app != "app-s3cret" {
		t.Fatalf("keys = %q, %q, err = %v", api, app, err)
	}
}

func TestBaseURL(t *testing.T) {
	if got := BaseURL("datadoghq.eu"); got != "https://api.datadoghq.eu" {
		t.Errorf("BaseURL = %q", got)
	}
}
