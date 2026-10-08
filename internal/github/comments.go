package github

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Comment kinds, as the tools contract names them.
const (
	CommentIssue  = "issue"
	CommentReview = "review"
	// CommentReviewThread is a comment on a line of the diff.
	CommentReviewThread = "review_comment"
)

// Comment is one piece of feedback on a pull request, in the shape the
// agent-comments tool reads from $ATTENTIOND_COMMENTS. Only comments that
// passed the author filter are ever built.
type Comment struct {
	ID     string `json:"id"`
	Author string `json:"author"`
	Kind   string `json:"kind"`
	Body   string `json:"body"`
	// Path and Line place a review-thread comment on the diff. Line is null
	// for other kinds and for a comment on a line that has since moved away.
	Path      string    `json:"path"`
	Line      *int      `json:"line"`
	URL       string    `json:"url"`
	CreatedAt time.Time `json:"created_at"`
}

// commentsQuery reads the newest hundred of each kind. A pull request with
// more than that is a conversation for a human.
const commentsQuery = `query($owner:String!,$name:String!,$number:Int!){
  repository(owner:$owner,name:$name){
    pullRequest(number:$number){
      comments(last:100){nodes{id body url createdAt author{__typename login}}}
      reviews(last:100){nodes{id body url createdAt author{__typename login}}}
      reviewThreads(last:100){nodes{comments(last:100){nodes{
        id body url createdAt path line author{__typename login}
      }}}}
    }
  }
}`

// rawComment is a comment as GitHub sends it, author type included. It never
// leaves this package: the filter turns it into a Comment or drops it.
type rawComment struct {
	ID        string    `json:"id"`
	Body      string    `json:"body"`
	URL       string    `json:"url"`
	CreatedAt time.Time `json:"createdAt"`
	Path      string    `json:"path"`
	Line      *int      `json:"line"`
	// Author is null for a deleted account.
	Author *struct {
		Typename string `json:"__typename"`
		Login    string `json:"login"`
	} `json:"author"`
}

type commentsResponse struct {
	Data struct {
		Repository *struct {
			PullRequest *struct {
				Comments struct {
					Nodes []rawComment `json:"nodes"`
				} `json:"comments"`
				Reviews struct {
					Nodes []rawComment `json:"nodes"`
				} `json:"reviews"`
				ReviewThreads struct {
					Nodes []struct {
						Comments struct {
							Nodes []rawComment `json:"nodes"`
						} `json:"comments"`
					} `json:"nodes"`
				} `json:"reviewThreads"`
			} `json:"pullRequest"`
		} `json:"repository"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// Comments returns the issue comments, review bodies and review-thread
// comments on one pull request that were written by a login in authors,
// oldest first.
//
// The filter runs here, before anything reaches the runner, because what
// passes it is handed to an agent that can push to the branch. Bots are
// dropped even when listed, and an empty list passes nobody.
func (c *Client) Comments(ctx context.Context, repo string, number int, authors []string) ([]Comment, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" {
		return nil, fmt.Errorf("github repo %q is not owner/name", repo)
	}
	raw, err := c.post(ctx, commentsQuery, map[string]any{"owner": owner, "name": name, "number": number})
	if err != nil {
		return nil, err
	}

	var decoded commentsResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("decode github comments: %w", err)
	}
	if len(decoded.Errors) > 0 {
		return nil, fmt.Errorf("github graphql: %s", decoded.Errors[0].Message)
	}
	if decoded.Data.Repository == nil || decoded.Data.Repository.PullRequest == nil {
		return nil, fmt.Errorf("github: %s#%d not found", repo, number)
	}

	pull := decoded.Data.Repository.PullRequest
	allow := allowlist(authors)
	var out []Comment
	out = keep(out, pull.Comments.Nodes, CommentIssue, allow)
	out = keep(out, pull.Reviews.Nodes, CommentReview, allow)
	for _, thread := range pull.ReviewThreads.Nodes {
		out = keep(out, thread.Comments.Nodes, CommentReviewThread, allow)
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].CreatedAt.Before(out[b].CreatedAt) })
	return out, nil
}

// allowlist folds logins the way GitHub compares them.
func allowlist(authors []string) map[string]bool {
	set := make(map[string]bool, len(authors))
	for _, login := range authors {
		if login = strings.TrimSpace(login); login != "" {
			set[strings.ToLower(login)] = true
		}
	}
	return set
}

// keep appends the comments that pass the author filter. A review with no
// body, an approval with nothing said, is not a comment.
func keep(out []Comment, nodes []rawComment, kind string, allow map[string]bool) []Comment {
	for _, node := range nodes {
		if node.Author == nil || node.Author.Typename == "Bot" || !allow[strings.ToLower(node.Author.Login)] {
			continue
		}
		if strings.TrimSpace(node.Body) == "" {
			continue
		}
		comment := Comment{
			ID: node.ID, Author: node.Author.Login, Kind: kind, Body: node.Body,
			URL: node.URL, CreatedAt: node.CreatedAt,
		}
		if kind == CommentReviewThread {
			comment.Path, comment.Line = node.Path, node.Line
		}
		out = append(out, comment)
	}
	return out
}
