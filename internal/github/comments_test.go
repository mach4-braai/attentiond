package github

import (
	"context"
	"testing"
	"time"
)

const commentsBody = `{"data":{"repository":{"pullRequest":{
  "comments":{"nodes":[
    {"id":"IC_1","body":"Can you rename this?","url":"u1","createdAt":"2026-09-12T10:00:00Z","author":{"__typename":"User","login":"Alice"}},
    {"id":"IC_2","body":"Deploy preview ready","url":"u2","createdAt":"2026-09-12T10:01:00Z","author":{"__typename":"Bot","login":"alice"}},
    {"id":"IC_3","body":"Drive-by opinion","url":"u3","createdAt":"2026-09-12T10:02:00Z","author":{"__typename":"User","login":"mallory"}},
    {"id":"IC_4","body":"ghost","url":"u4","createdAt":"2026-09-12T10:03:00Z","author":null}
  ]},
  "reviews":{"nodes":[
    {"id":"PRR_1","body":"","url":"u5","createdAt":"2026-09-12T09:00:00Z","author":{"__typename":"User","login":"alice"}},
    {"id":"PRR_2","body":"Two nits below.","url":"u6","createdAt":"2026-09-12T09:30:00Z","author":{"__typename":"User","login":"bob"}}
  ]},
  "reviewThreads":{"nodes":[
    {"comments":{"nodes":[
      {"id":"PRRC_1","body":"Off by one","url":"u7","createdAt":"2026-09-12T09:31:00Z","path":"main.go","line":42,"author":{"__typename":"User","login":"bob"}},
      {"id":"PRRC_2","body":"lgtm","url":"u8","createdAt":"2026-09-12T09:32:00Z","path":"main.go","line":null,"author":{"__typename":"Bot","login":"copilot"}}
    ]}}
  ]}
}}}}`

// What passes this filter is handed to an agent that can push to the branch.
// A bot passes nothing, even under an allowlisted login, and neither does
// anybody not on the list.
func TestCommentsKeepOnlyAllowlistedHumans(t *testing.T) {
	server, requests := recordingServer(t, commentsBody)

	comments, err := NewClient(server.URL, "t0ken", 5*time.Second).
		Comments(context.Background(), "didx-xyz/tofu", 42, []string{"alice", "bob", "copilot"})
	if err != nil {
		t.Fatalf("Comments: %v", err)
	}

	var ids []string
	for _, comment := range comments {
		ids = append(ids, comment.ID)
	}
	want := []string{"PRR_2", "PRRC_1", "IC_1"}
	if len(ids) != len(want) {
		t.Fatalf("kept %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("kept %v, want %v in that order", ids, want)
		}
	}

	thread := comments[1]
	if thread.Kind != CommentReviewThread || thread.Path != "main.go" || thread.Line == nil || *thread.Line != 42 {
		t.Errorf("review-thread comment lost its place on the diff: %+v", thread)
	}
	if comments[2].Kind != CommentIssue || comments[2].Author != "Alice" || comments[2].Line != nil {
		t.Errorf("issue comment = %+v", comments[2])
	}

	vars := (*requests)[0].Variables
	if vars["owner"] != "didx-xyz" || vars["name"] != "tofu" || vars["number"] != float64(42) {
		t.Errorf("variables = %v", vars)
	}
}

func TestCommentsWithNoAllowlistPassNobody(t *testing.T) {
	server, _ := recordingServer(t, commentsBody)
	comments, err := NewClient(server.URL, "t0ken", 5*time.Second).
		Comments(context.Background(), "didx-xyz/tofu", 42, nil)
	if err != nil {
		t.Fatalf("Comments: %v", err)
	}
	if len(comments) != 0 {
		t.Fatalf("an empty allowlist let through %+v", comments)
	}
}
