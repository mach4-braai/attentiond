package github

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/devanmcgeer/attentiond/internal/attention"
)

// Role says why a pull request is in your inbox at all.
const (
	roleReviewer = "reviewer"
	roleAuthor   = "author"
	// roleReviewed is a pull request you have already reviewed. It is not the
	// same errand as roleReviewer: nobody is waiting for your opinion any
	// more, so what matters is the state of the branch. Once you approve one,
	// you are usually the person who merges it.
	roleReviewed = "reviewed"
)

// The labels this adapter reports in context["pr_state"]. They are the words a
// human uses about a pull request, kept separate from the five lifecycle states
// so the dashboard can show why something is waiting without inventing states.
const (
	labelReviewRequested  = "review requested"
	labelChangesRequested = "changes requested"
	labelChecksRunning    = "checks running"
	labelChecksFailing    = "checks failing"
	labelRebaseRequired   = "rebase required"
	labelReadyToMerge     = "ready to merge"
	labelApproved         = "approved"
	labelAwaitingReview   = "awaiting review"
	labelDraft            = "draft"
	labelStaleDraft       = "stale draft"
)

// Config tunes normalization.
type Config struct {
	// StaleDraftAfter is how long a draft may sit untouched before it is
	// called stale. Zero disables the distinction.
	StaleDraftAfter time.Duration
	// PriorityRepos are the repositories whose review requests outrank review
	// requests everywhere else. A review you owe in the repository that runs
	// the infrastructure is not the same errand as one in a side project.
	PriorityRepos map[string]bool
}

// display is the tone and rank that go with each label. Keeping it beside the
// labels is the point: the word, its colour and its place in the queue are one
// decision, and splitting them across files is how they drift apart.
//
// A review request is ranked in rank instead, because its rank depends on who
// else was asked and on which repository asked.
var display = map[string]struct {
	tone     attention.Tone
	priority int
}{
	// One click from finished, so it is the cheapest item on the board to
	// clear and it sits at the top.
	labelReadyToMerge: {attention.ToneReady, attention.PriorityOneClick},

	labelChangesRequested: {attention.ToneAttention, attention.PriorityActionable},
	labelRebaseRequired:   {attention.ToneAttention, attention.PriorityActionable},
	labelChecksFailing:    {attention.ToneFailed, attention.PriorityActionable},

	labelChecksRunning: {attention.ToneActive, attention.PriorityBackground},

	// Somebody else's turn.
	labelApproved:       {attention.ToneNeutral, attention.PriorityBackground},
	labelAwaitingReview: {attention.ToneNeutral, attention.PriorityBackground},
	labelDraft:          {attention.ToneNeutral, attention.PriorityBackground},
	labelStaleDraft:     {attention.ToneNeutral, attention.PriorityBackground},
}

// classify maps one pull request onto the core model. Order matters: the first
// condition that holds is the one reported, so the most actionable reason wins.
func classify(pr PullRequest, role string, cfg Config, now time.Time) (attention.State, attention.Severity, string) {
	// Somebody asked you. Nothing about the branch changes that. A pull
	// request you have already reviewed falls through to the branch state
	// below instead: the question is no longer what you think of it, it is
	// whether it can go in.
	if role == roleReviewer {
		return attention.StateNeedsAttention, attention.SeverityWarning, labelReviewRequested
	}

	// A draft is a statement that it is not ready, so it never enters the
	// attention queue, however red it is.
	if pr.IsDraft {
		if cfg.StaleDraftAfter > 0 && now.Sub(pr.UpdatedAt) > cfg.StaleDraftAfter {
			return attention.StateWaiting, attention.SeverityInfo, labelStaleDraft
		}
		return attention.StateWaiting, attention.SeverityInfo, labelDraft
	}

	switch {
	case pr.MergeStateStatus == "DIRTY" || pr.Mergeable == "CONFLICTING":
		return attention.StateNeedsAttention, attention.SeverityWarning, labelRebaseRequired
	case pr.Checks() == "FAILURE" || pr.Checks() == "ERROR":
		// Warning, not critical: red CI on your own pull request is routine
		// work, and critical is reserved for a producer that says so through
		// /api/events.
		return attention.StateFailed, attention.SeverityWarning, labelChecksFailing
	case pr.ReviewDecision == "CHANGES_REQUESTED":
		return attention.StateNeedsAttention, attention.SeverityWarning, labelChangesRequested
	case pr.Checks() == "PENDING" || pr.Checks() == "EXPECTED":
		return attention.StateWorking, attention.SeverityInfo, labelChecksRunning
	case pr.MergeStateStatus == "BEHIND":
		return attention.StateNeedsAttention, attention.SeverityWarning, labelRebaseRequired
	case pr.ReviewDecision == "APPROVED" && pr.MergeStateStatus == "CLEAN":
		return attention.StateNeedsAttention, attention.SeverityWarning, labelReadyToMerge
	case pr.ReviewDecision == "APPROVED":
		return attention.StateWaiting, attention.SeverityInfo, labelApproved
	default:
		return attention.StateWaiting, attention.SeverityInfo, labelAwaitingReview
	}
}

// rank returns the tone and queue position for a label. A review request is
// the one label whose rank is not a property of the label alone: it depends
// on whether anyone else can answer it, and on which repository asked.
func rank(state attention.State, label string, pr PullRequest, login string, cfg Config) (attention.Tone, int) {
	if label == labelReviewRequested {
		if soleReviewer(pr, login) {
			return attention.ToneAttention, attention.PrioritySoleReviewer
		}
		important := cfg.PriorityRepos[strings.ToLower(pr.Repository.NameWithOwner)]
		switch {
		case sharedReview(pr) && important:
			return attention.ToneAttention, attention.PrioritySharedBlockingOthers
		case sharedReview(pr):
			return attention.ToneAttention, attention.PrioritySharedAsk
		case important:
			return attention.ToneAttention, attention.PriorityBlockingOthers
		}
		return attention.ToneAttention, attention.PriorityAsked
	}
	if d, ok := display[label]; ok {
		return d.tone, d.priority
	}
	// A label nobody ranked. Fall back to what the state alone implies, which
	// is wrong in a defensible direction rather than silently last.
	_, tone := attention.DefaultDisplay(state)
	return tone, attention.DefaultPriority(state)
}

// soleReviewer reports whether a pull request that is ready for review waits
// on login and on nobody else.
func soleReviewer(pr PullRequest, login string) bool {
	return !pr.IsDraft && onlyYou(pr, login)
}

// onlyYou reports whether login, as a user rather than through a team, is the
// one pending review request.
func onlyYou(pr PullRequest, login string) bool {
	if login == "" || pr.ReviewRequests.TotalCount != 1 || len(pr.ReviewRequests.Nodes) != 1 {
		return false
	}
	reviewer := pr.ReviewRequests.Nodes[0].RequestedReviewer
	return reviewer != nil && reviewer.Typename == "User" && strings.EqualFold(reviewer.Login, login)
}

// sharedReview reports whether somebody else can answer the request: another
// reviewer, or a team, where any member can.
func sharedReview(pr PullRequest) bool {
	requests := pr.ReviewRequests
	if requests.TotalCount > 1 {
		return true
	}
	return len(requests.Nodes) == 1 && requests.Nodes[0].RequestedReviewer != nil &&
		requests.Nodes[0].RequestedReviewer.Typename == "Team"
}

// reviewScope says who else was asked, for a pull request waiting on your
// review: "sole" when it is you alone, "shared" when another reviewer or a team
// can answer it. The rank already reflects this, but a bump or a top label
// replaces the rank, and a dashboard that reads the number back loses it.
// Empty when the request list says neither, which leaves the key unset.
func reviewScope(pr PullRequest, role, login string) string {
	switch {
	case role != roleReviewer:
		return ""
	case onlyYou(pr, login):
		return "sole"
	case sharedReview(pr):
		return "shared"
	}
	return ""
}

// Normalize turns an inbox into attention items. A pull request reached by more
// than one search is reported once, under the first role that claims it:
// a pending review request is what is waiting on you, then your own authorship,
// then a review you already gave.
func Normalize(inbox Inbox, cfg Config, now time.Time) []attention.Item {
	items := make([]attention.Item, 0,
		len(inbox.ReviewRequested)+len(inbox.Authored)+len(inbox.Reviewed))
	seen := make(map[string]struct{}, cap(items))

	for _, group := range []struct {
		role  string
		pulls []PullRequest
	}{
		{roleReviewer, inbox.ReviewRequested},
		{roleAuthor, inbox.Authored},
		{roleReviewed, inbox.Reviewed},
	} {
		for _, pr := range group.pulls {
			key := pullKey(pr)
			if _, duplicate := seen[key]; duplicate {
				continue
			}
			seen[key] = struct{}{}
			items = append(items, newItem(pr, group.role, inbox.Login, cfg, now))
		}
	}

	sort.Slice(items, func(a, b int) bool { return items[a].ID < items[b].ID })
	return items
}

func newItem(pr PullRequest, role, login string, cfg Config, now time.Time) attention.Item {
	state, severity, label := classify(pr, role, cfg, now)
	tone, priority := rank(state, label, pr, login, cfg)

	meta := map[string]string{
		"repo":     pr.Repository.NameWithOwner,
		"number":   strconv.Itoa(pr.Number),
		"role":     role,
		"pr_state": label,
		"url":      pr.URL,
		// Pending review requests, counting a team as one.
		"reviewers": strconv.Itoa(pr.ReviewRequests.TotalCount),
	}
	putIfSet(meta, "review_decision", pr.ReviewDecision)
	putIfSet(meta, "review_scope", reviewScope(pr, role, login))
	putIfSet(meta, "mergeable", pr.Mergeable)
	putIfSet(meta, "merge_state", pr.MergeStateStatus)
	putIfSet(meta, "checks", pr.Checks())
	if pr.Author != nil {
		putIfSet(meta, "author", pr.Author.Login)
	}
	if pr.IsDraft {
		meta["draft"] = "true"
	}

	return attention.Item{
		ID:       attention.Key(SourceName, pullKey(pr)),
		Source:   SourceName,
		Title:    pullKey(pr) + " · " + pr.Title,
		State:    state,
		Severity: severity,
		Label:    label,
		Tone:     tone,
		Priority: priority,
		Context:  meta,
		// The pull request's own updated_at, not poll time: how long something
		// has been sitting is the useful number.
		UpdatedAt: pr.UpdatedAt,
		Actions: []attention.Action{{
			ID:     "open",
			Label:  "Open",
			Method: "GET",
			Href:   pr.URL,
		}},
	}
}

func pullKey(pr PullRequest) string {
	return pr.Repository.NameWithOwner + "#" + strconv.Itoa(pr.Number)
}

func putIfSet(target map[string]string, key, value string) {
	if value != "" {
		target[key] = value
	}
}
