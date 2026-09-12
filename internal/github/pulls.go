package github

import (
	"sort"
	"strconv"
	"time"

	"github.com/devanmcgeer/attentiond/internal/attention"
)

// Role says why a pull request is in your inbox at all.
const (
	roleReviewer = "reviewer"
	roleAuthor   = "author"
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
}

// classify maps one pull request onto the core model. Order matters: the first
// condition that holds is the one reported, so the most actionable reason wins.
func classify(pr PullRequest, role string, cfg Config, now time.Time) (attention.State, attention.Severity, string) {
	// Somebody asked you. Nothing about the branch changes that.
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

// Normalize turns an inbox into attention items. A pull request you authored
// and were also asked to review is reported once, as a review request, because
// that is the half that is waiting on you.
func Normalize(inbox Inbox, cfg Config, now time.Time) []attention.Item {
	items := make([]attention.Item, 0, len(inbox.ReviewRequested)+len(inbox.Authored))
	seen := make(map[string]struct{}, cap(items))

	for _, group := range []struct {
		role  string
		pulls []PullRequest
	}{
		{roleReviewer, inbox.ReviewRequested},
		{roleAuthor, inbox.Authored},
	} {
		for _, pr := range group.pulls {
			key := pullKey(pr)
			if _, duplicate := seen[key]; duplicate {
				continue
			}
			seen[key] = struct{}{}
			items = append(items, newItem(pr, group.role, cfg, now))
		}
	}

	sort.Slice(items, func(a, b int) bool { return items[a].ID < items[b].ID })
	return items
}

func newItem(pr PullRequest, role string, cfg Config, now time.Time) attention.Item {
	state, severity, label := classify(pr, role, cfg, now)

	meta := map[string]string{
		"repo":     pr.Repository.NameWithOwner,
		"number":   strconv.Itoa(pr.Number),
		"role":     role,
		"pr_state": label,
		"url":      pr.URL,
	}
	putIfSet(meta, "review_decision", pr.ReviewDecision)
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
