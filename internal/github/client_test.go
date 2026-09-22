package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// node renders one pull request, enough to decode.
func node(number int) string {
	return fmt.Sprintf(`{"number":%d,"title":"t","url":"u","isDraft":false,
	  "updatedAt":"2026-09-12T15:05:16Z","repository":{"nameWithOwner":"o/r"},
	  "author":null,"reviewDecision":"","mergeable":"UNKNOWN","mergeStateStatus":"UNKNOWN",
	  "commits":{"nodes":[{"commit":{"statusCheckRollup":null}}]}}`, number)
}

func page(nodes string, hasNext bool, cursor string) string {
	return fmt.Sprintf(`{"data":{"viewer":{"login":"mcgeerdev"},
	  "search":{"pageInfo":{"hasNextPage":%t,"endCursor":%q},"nodes":[%s]}}}`,
		hasNext, cursor, nodes)
}

// recordingServer answers every search with the supplied bodies in order and
// keeps the decoded requests.
func recordingServer(t *testing.T, bodies ...string) (*httptest.Server, *[]graphQLRequest) {
	t.Helper()
	var mu sync.Mutex
	var requests []graphQLRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request graphQLRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("request body: %v", err)
		}
		mu.Lock()
		index := len(requests)
		requests = append(requests, request)
		mu.Unlock()

		body := bodies[len(bodies)-1]
		if index < len(bodies) {
			body = bodies[index]
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server, &requests
}

func TestInboxDecodesNullsGitHubActuallySends(t *testing.T) {
	// A pull request with no CI and a deleted author: both fields come back
	// null, and a naive decode panics on them.
	server, requests := recordingServer(t, page(node(7), false, ""), page("", false, ""))

	inbox, err := NewClient(server.URL, "t0ken", 5*time.Second).
		Inbox(context.Background(), Search{Limit: 10})
	if err != nil {
		t.Fatalf("Inbox: %v", err)
	}
	if inbox.Login != "mcgeerdev" || len(inbox.Authored) != 1 ||
		len(inbox.ReviewRequested) != 0 || len(inbox.Reviewed) != 0 {
		t.Fatalf("inbox = %+v", inbox)
	}
	if got := inbox.Authored[0].Checks(); got != "" {
		t.Errorf("Checks() = %q, want empty when nothing reported", got)
	}
	if inbox.Authored[0].Author != nil {
		t.Errorf("author = %+v, want nil", inbox.Authored[0].Author)
	}

	if len(*requests) != 3 {
		t.Fatalf("issued %d searches, want one per bucket", len(*requests))
	}
	for i, want := range []string{"author:@me", "review-requested:@me", "reviewed-by:@me"} {
		if got := (*requests)[i].Variables["q"].(string); !strings.Contains(got, want) {
			t.Errorf("query %d = %q, want it to contain %q", i, got, want)
		}
	}
}

func TestInboxSendsTheHeadersGitHubNeeds(t *testing.T) {
	var headers http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(page("", false, "")))
	}))
	defer server.Close()

	if _, err := NewClient(server.URL, "t0ken", 5*time.Second).
		Inbox(context.Background(), Search{Limit: 10}); err != nil {
		t.Fatal(err)
	}
	if got := headers.Get("Authorization"); got != "bearer t0ken" {
		t.Errorf("Authorization = %q", got)
	}
	if !strings.Contains(headers.Get("Accept"), "merge-info-preview") {
		t.Errorf("Accept = %q, want the preview that carries mergeStateStatus", headers.Get("Accept"))
	}
}

func TestInboxFollowsCursorsUntilTheResultsRunOut(t *testing.T) {
	server, requests := recordingServer(t,
		page(node(1)+","+node(2), true, "CURSOR1"), // authored, page 1
		page(node(3), false, ""),                   // authored, page 2
		page("", false, ""),                        // review requested
	)

	inbox, err := NewClient(server.URL, "t0ken", 5*time.Second).
		Inbox(context.Background(), Search{Limit: 200})
	if err != nil {
		t.Fatalf("Inbox: %v", err)
	}
	if len(inbox.Authored) != 3 {
		t.Fatalf("collected %d pull requests, want every page", len(inbox.Authored))
	}
	if inbox.Truncated {
		t.Error("a fully drained search reported itself truncated")
	}
	if got := (*requests)[1].Variables["after"]; got != "CURSOR1" {
		t.Errorf("second page after = %v, want the first page's cursor", got)
	}
	if _, sent := (*requests)[0].Variables["after"]; sent {
		t.Error("the first page sent a cursor")
	}
}

func TestInboxReportsTruncationInsteadOfPretendingToBeComplete(t *testing.T) {
	// Every page claims there is another one, so the limit is what stops it.
	server, _ := recordingServer(t, page(node(1)+","+node(2), true, "CURSOR"))

	inbox, err := NewClient(server.URL, "t0ken", 5*time.Second).
		Inbox(context.Background(), Search{Limit: 2})
	if err != nil {
		t.Fatalf("Inbox: %v", err)
	}
	if len(inbox.Authored) != 2 {
		t.Errorf("collected %d, want the limit", len(inbox.Authored))
	}
	if !inbox.Truncated {
		t.Error("a capped search reported itself complete; the queue would silently drop pull requests")
	}
}

func TestInboxAppliesTheScopeToBothSearches(t *testing.T) {
	scope, err := ParseScope("didx-xyz/tofu, mach4-braai/hum", "mach4-braai")
	if err != nil {
		t.Fatal(err)
	}
	server, requests := recordingServer(t, page("", false, ""))

	if _, err := NewClient(server.URL, "t0ken", 5*time.Second).
		Inbox(context.Background(), Search{Limit: 10, Scope: scope}); err != nil {
		t.Fatal(err)
	}

	for _, request := range *requests {
		query := request.Variables["q"].(string)
		for _, want := range []string{"repo:didx-xyz/tofu", "repo:mach4-braai/hum", "org:mach4-braai"} {
			if !strings.Contains(query, want) {
				t.Errorf("query %q is missing %q", query, want)
			}
		}
	}
}

func TestParseScopeRejectsWhatGitHubWouldSilentlyIgnore(t *testing.T) {
	for _, bad := range []struct {
		repos, orgs string
	}{
		{repos: "tofu"},
		{repos: "didx-xyz/"},
		{repos: "/tofu"},
		{repos: "didx-xyz/tofu/extra"},
		{orgs: "mach4-braai/hum"},
	} {
		if _, err := ParseScope(bad.repos, bad.orgs); err == nil {
			t.Errorf("ParseScope(%q, %q) was accepted", bad.repos, bad.orgs)
		}
	}

	scope, err := ParseScope("", "")
	if err != nil || !scope.Empty() || scope.String() != "everything" {
		t.Errorf("empty scope = %+v, %v", scope, err)
	}
}

func TestInboxTreatsGraphQLErrorsInA200AsFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":null,"errors":[{"message":"Field 'mergeStateStatus' doesn't exist"}]}`))
	}))
	defer server.Close()

	_, err := NewClient(server.URL, "t0ken", 5*time.Second).
		Inbox(context.Background(), Search{Limit: 10})
	if err == nil {
		t.Fatal("a 200 carrying GraphQL errors was accepted as success")
	}
	if !strings.Contains(err.Error(), "mergeStateStatus") {
		t.Errorf("error = %v, want the GraphQL message", err)
	}
}

func TestInboxReportsHTTPFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
	}))
	defer server.Close()

	_, err := NewClient(server.URL, "stale", 5*time.Second).
		Inbox(context.Background(), Search{Limit: 10})
	if err == nil {
		t.Fatal("401 was accepted as success")
	}
	if !strings.Contains(err.Error(), "Bad credentials") {
		t.Errorf("error = %v, want the response body in it", err)
	}
}
