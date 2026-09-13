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

// Scope narrows which repositories are watched. An empty scope watches every
// repository the token can see.
type Scope struct {
	// Repos are owner/name pairs, turned into repo: qualifiers.
	Repos []string
	// Orgs are account logins, turned into org: qualifiers.
	Orgs []string
}

// NewScope validates already-split lists, which is what a config file hands
// over. A typo has to fail loudly: a qualifier GitHub does not understand
// narrows the queue to nothing, and an empty attention queue looks exactly
// like having nothing to do.
func NewScope(repos, orgs []string) (Scope, error) {
	var scope Scope
	for _, repo := range repos {
		repo = strings.TrimSpace(repo)
		if repo == "" {
			continue
		}
		owner, name, ok := strings.Cut(repo, "/")
		if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
			return Scope{}, fmt.Errorf("github repo %q is not owner/name", repo)
		}
		scope.Repos = append(scope.Repos, repo)
	}
	for _, org := range orgs {
		org = strings.TrimSpace(org)
		if org == "" {
			continue
		}
		if strings.ContainsAny(org, "/ ") {
			return Scope{}, fmt.Errorf("github org %q is not an account login", org)
		}
		scope.Orgs = append(scope.Orgs, org)
	}
	return scope, nil
}

// ParseScope reads the comma or space separated form a command line uses.
func ParseScope(repos, orgs string) (Scope, error) {
	return NewScope(splitList(repos), splitList(orgs))
}

// Empty reports whether the scope watches everything.
func (s Scope) Empty() bool {
	return len(s.Repos) == 0 && len(s.Orgs) == 0
}

// String renders the scope for logs and /health.
func (s Scope) String() string {
	if s.Empty() {
		return "everything"
	}
	return strings.TrimSpace(s.qualifiers())
}

// qualifiers builds the search suffix. Repeating a qualifier is how GitHub
// search spells OR, so repos and orgs union rather than intersect.
func (s Scope) qualifiers() string {
	var b strings.Builder
	for _, repo := range s.Repos {
		b.WriteString(" repo:" + repo)
	}
	for _, org := range s.Orgs {
		b.WriteString(" org:" + org)
	}
	return b.String()
}

func splitList(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n'
	})
	for i, field := range fields {
		fields[i] = strings.TrimSpace(field)
	}
	return fields
}

// Search is one poll's worth of parameters.
type Search struct {
	Scope Scope
	// Limit caps how many pull requests each of the two searches returns.
	Limit int
}

// pageSize is GitHub's maximum for a search connection.
const pageSize = 50

// Inbox is everything one poll learned.
type Inbox struct {
	Login           string
	Authored        []PullRequest
	ReviewRequested []PullRequest
	// Truncated is set when GitHub had more results than Limit allowed. The
	// caller must surface it: a silently capped attention queue is worse than
	// no attention queue, because it looks complete.
	Truncated bool
}

const searchQuery = `query($q:String!,$first:Int!,$after:String){
  viewer{login}
  search(query:$q,type:ISSUE,first:$first,after:$after){
    pageInfo{hasNextPage endCursor}
    nodes{...pr}
  }
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
		Search struct {
			PageInfo struct {
				HasNextPage bool   `json:"hasNextPage"`
				EndCursor   string `json:"endCursor"`
			} `json:"pageInfo"`
			Nodes []PullRequest `json:"nodes"`
		} `json:"search"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// Inbox fetches the open pull requests you authored and the ones waiting on
// your review. Archived repositories are excluded: nothing there is actionable.
func (c *Client) Inbox(ctx context.Context, search Search) (Inbox, error) {
	suffix := search.Scope.qualifiers()

	authored, login, moreAuthored, err := c.searchAll(ctx,
		"is:open is:pr author:@me archived:false"+suffix, search.Limit)
	if err != nil {
		return Inbox{}, err
	}
	review, reviewLogin, moreReview, err := c.searchAll(ctx,
		"is:open is:pr review-requested:@me archived:false"+suffix, search.Limit)
	if err != nil {
		return Inbox{}, err
	}
	if login == "" {
		login = reviewLogin
	}

	return Inbox{
		Login:           login,
		Authored:        authored,
		ReviewRequested: review,
		Truncated:       moreAuthored || moreReview,
	}, nil
}

// searchAll pages until the results run out or limit is reached, and reports
// whether GitHub still had more.
func (c *Client) searchAll(ctx context.Context, query string, limit int) (pulls []PullRequest, login string, more bool, err error) {
	if limit <= 0 {
		limit = pageSize
	}
	cursor := ""
	for len(pulls) < limit {
		want := min(limit-len(pulls), pageSize)
		page, err := c.searchPage(ctx, query, want, cursor)
		if err != nil {
			return nil, "", false, err
		}
		login = page.Data.Viewer.Login
		pulls = append(pulls, page.Data.Search.Nodes...)
		if !page.Data.Search.PageInfo.HasNextPage {
			return pulls, login, false, nil
		}
		cursor = page.Data.Search.PageInfo.EndCursor
	}
	return pulls, login, true, nil
}

func (c *Client) searchPage(ctx context.Context, query string, first int, after string) (graphQLResponse, error) {
	variables := map[string]any{"q": query, "first": first}
	if after != "" {
		variables["after"] = after
	}
	body, err := json.Marshal(graphQLRequest{Query: searchQuery, Variables: variables})
	if err != nil {
		return graphQLResponse{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return graphQLResponse{}, err
	}
	req.Header.Set("Authorization", "bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	// mergeStateStatus, which is the only field that separates "behind base"
	// from "conflicting", is still behind this preview.
	req.Header.Set("Accept", "application/vnd.github.merge-info-preview+json")

	resp, err := c.http.Do(req)
	if err != nil {
		return graphQLResponse{}, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return graphQLResponse{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return graphQLResponse{}, fmt.Errorf("github graphql: %s: %s", resp.Status, firstLine(raw))
	}

	var decoded graphQLResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return graphQLResponse{}, fmt.Errorf("decode github response: %w", err)
	}
	// GraphQL reports failures in a 200 body, so this is the real error path.
	if len(decoded.Errors) > 0 {
		return graphQLResponse{}, fmt.Errorf("github graphql: %s", decoded.Errors[0].Message)
	}
	return decoded, nil
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
