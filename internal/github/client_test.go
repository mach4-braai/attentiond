package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestInboxDecodesNullsGitHubActuallySends(t *testing.T) {
	// A pull request with no CI and a deleted author: both fields come back
	// null, and a naive decode panics on them.
	body := `{"data":{"viewer":{"login":"mcgeerdev"},
	  "mine":{"nodes":[{"number":7,"title":"t","url":"u","isDraft":false,
	    "updatedAt":"2026-09-12T15:05:16Z","repository":{"nameWithOwner":"o/r"},
	    "author":null,"reviewDecision":"","mergeable":"UNKNOWN","mergeStateStatus":"UNKNOWN",
	    "commits":{"nodes":[{"commit":{"statusCheckRollup":null}}]}}]},
	  "review":{"nodes":[]}}}`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "bearer t0ken" {
			t.Errorf("Authorization = %q", got)
		}
		if !strings.Contains(r.Header.Get("Accept"), "merge-info-preview") {
			t.Errorf("Accept = %q, want the preview that carries mergeStateStatus", r.Header.Get("Accept"))
		}
		var request graphQLRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("request body: %v", err)
		}
		if !strings.Contains(request.Variables["review"].(string), "review-requested:@me") {
			t.Errorf("review query = %v", request.Variables["review"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	inbox, err := NewClient(server.URL, "t0ken", 5*time.Second).Inbox(context.Background(), 10)
	if err != nil {
		t.Fatalf("Inbox: %v", err)
	}
	if inbox.Login != "mcgeerdev" || len(inbox.Authored) != 1 {
		t.Fatalf("inbox = %+v", inbox)
	}
	if got := inbox.Authored[0].Checks(); got != "" {
		t.Errorf("Checks() = %q, want empty when nothing reported", got)
	}
	if inbox.Authored[0].Author != nil {
		t.Errorf("author = %+v, want nil", inbox.Authored[0].Author)
	}
}

func TestInboxTreatsGraphQLErrorsInA200AsFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":null,"errors":[{"message":"Field 'mergeStateStatus' doesn't exist"}]}`))
	}))
	defer server.Close()

	_, err := NewClient(server.URL, "t0ken", 5*time.Second).Inbox(context.Background(), 10)
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

	_, err := NewClient(server.URL, "stale", 5*time.Second).Inbox(context.Background(), 10)
	if err == nil {
		t.Fatal("401 was accepted as success")
	}
	if !strings.Contains(err.Error(), "Bad credentials") {
		t.Errorf("error = %v, want the response body in it", err)
	}
}
