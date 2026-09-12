// Package github turns the pull requests that involve you into attention
// items. It reads GitHub's GraphQL API, which answers review state,
// mergeability and check status in one request; the REST equivalent needs
// three calls per pull request.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

// SourceName is the item source this adapter owns.
const SourceName = "github"

// DefaultEndpoint is the GraphQL endpoint for github.com. GitHub Enterprise
// lives at https://<host>/api/graphql.
const DefaultEndpoint = "https://api.github.com/graphql"

// Client talks to one GraphQL endpoint with one token.
type Client struct {
	endpoint string
	token    string
	http     *http.Client
}

// NewClient returns a client. The token is never logged and never leaves this
// process.
func NewClient(endpoint, token string, timeout time.Duration) *Client {
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	return &Client{
		endpoint: endpoint,
		token:    token,
		http:     &http.Client{Timeout: timeout},
	}
}

// ResolveToken finds a GitHub token without asking for one. It returns the
// token and where it came from, so startup can say which credential is in use
// without printing it.
func ResolveToken(ctx context.Context) (token string, origin string, err error) {
	for _, key := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value, key, nil
		}
	}

	// gh keeps its token in the system keychain, so shelling out to it is the
	// only way to reuse a login the human already did.
	path, err := exec.LookPath("gh")
	if err != nil {
		return "", "", fmt.Errorf("no GITHUB_TOKEN or GH_TOKEN, and gh is not on PATH")
	}
	cmd := exec.CommandContext(ctx, path, "auth", "token")
	out, err := cmd.Output()
	if err != nil {
		return "", "", fmt.Errorf("gh auth token: %w", err)
	}
	value := strings.TrimSpace(string(out))
	if value == "" {
		return "", "", fmt.Errorf("gh auth token returned nothing; run gh auth login")
	}
	return value, "gh auth token", nil
}

// PullRequest is the slice of a GraphQL PullRequest node this adapter reads.
type PullRequest struct {
	Number     int        `json:"number"`
	Title      string     `json:"title"`
	URL        string     `json:"url"`
	IsDraft    bool       `json:"isDraft"`
	UpdatedAt  time.Time  `json:"updatedAt"`
	Repository Repository `json:"repository"`
	// Author is null for pull requests whose account is gone.
	Author           *Actor  `json:"author"`
	ReviewDecision   string  `json:"reviewDecision"`
	Mergeable        string  `json:"mergeable"`
	MergeStateStatus string  `json:"mergeStateStatus"`
	Commits          Commits `json:"commits"`
}

// Repository is the owner/name pair an item is reported under.
type Repository struct {
	NameWithOwner string `json:"nameWithOwner"`
}

// Actor is a GitHub account.
type Actor struct {
	Login string `json:"login"`
}

// Commits carries the head commit, which is where check results live.
type Commits struct {
	Nodes []CommitNode `json:"nodes"`
}

// CommitNode wraps one commit in the GraphQL connection.
type CommitNode struct {
	Commit Commit `json:"commit"`
}

// Commit is the head commit of a pull request.
type Commit struct {
	// StatusCheckRollup is null when no CI ever ran.
	StatusCheckRollup *StatusCheckRollup `json:"statusCheckRollup"`
}

// StatusCheckRollup is the combined result of every check on a commit.
type StatusCheckRollup struct {
	State string `json:"state"`
}

// Checks returns the rollup state of the head commit, or an empty string when
// nothing has reported.
func (pr PullRequest) Checks() string {
	for _, node := range pr.Commits.Nodes {
		if rollup := node.Commit.StatusCheckRollup; rollup != nil {
			return rollup.State
		}
	}
	return ""
}

// Inbox is everything one poll learned.
type Inbox struct {
	Login           string
	Authored        []PullRequest
	ReviewRequested []PullRequest
}

const inboxQuery = `query($mine:String!,$review:String!,$limit:Int!){
  viewer{login}
  mine:search(query:$mine,type:ISSUE,first:$limit){nodes{...pr}}
  review:search(query:$review,type:ISSUE,first:$limit){nodes{...pr}}
}
fragment pr on PullRequest{
  number title url isDraft updatedAt
  repository{nameWithOwner}
  author{login}
  reviewDecision mergeable mergeStateStatus
  commits(last:1){nodes{commit{statusCheckRollup{state}}}}
}`

type graphQLRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

type graphQLResponse struct {
	Data struct {
		Viewer struct {
			Login string `json:"login"`
		} `json:"viewer"`
		Mine struct {
			Nodes []PullRequest `json:"nodes"`
		} `json:"mine"`
		Review struct {
			Nodes []PullRequest `json:"nodes"`
		} `json:"review"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// Inbox fetches the open pull requests you authored and the ones waiting on
// your review. Archived repositories are excluded: nothing there is actionable.
func (c *Client) Inbox(ctx context.Context, limit int) (Inbox, error) {
	body, err := json.Marshal(graphQLRequest{
		Query: inboxQuery,
		Variables: map[string]any{
			"mine":   "is:open is:pr author:@me archived:false",
			"review": "is:open is:pr review-requested:@me archived:false",
			"limit":  limit,
		},
	})
	if err != nil {
		return Inbox{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return Inbox{}, err
	}
	req.Header.Set("Authorization", "bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	// mergeStateStatus, which is the only field that separates "behind base"
	// from "conflicting", is still behind this preview.
	req.Header.Set("Accept", "application/vnd.github.merge-info-preview+json")

	resp, err := c.http.Do(req)
	if err != nil {
		return Inbox{}, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return Inbox{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return Inbox{}, fmt.Errorf("github graphql: %s: %s", resp.Status, firstLine(raw))
	}

	var decoded graphQLResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return Inbox{}, fmt.Errorf("decode github response: %w", err)
	}
	// GraphQL reports failures in a 200 body, so this is the real error path.
	if len(decoded.Errors) > 0 {
		return Inbox{}, fmt.Errorf("github graphql: %s", decoded.Errors[0].Message)
	}

	return Inbox{
		Login:           decoded.Data.Viewer.Login,
		Authored:        decoded.Data.Mine.Nodes,
		ReviewRequested: decoded.Data.Review.Nodes,
	}, nil
}

func firstLine(raw []byte) string {
	text := strings.TrimSpace(string(raw))
	if index := strings.IndexByte(text, '\n'); index >= 0 {
		text = text[:index]
	}
	if len(text) > 200 {
		text = text[:200] + "…"
	}
	return text
}
