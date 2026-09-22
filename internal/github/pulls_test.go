package github

import (
	"strconv"
	"testing"
	"time"

	"github.com/devanmcgeer/attentiond/internal/attention"
)

func pull(number int, mutate func(*PullRequest)) PullRequest {
	pr := PullRequest{
		Number:           number,
		Title:            "Move the RDS credentials into Aurora",
		URL:              "https://github.com/didx-xyz/tofu/pull/" + strconv.Itoa(number),
		UpdatedAt:        time.Date(2026, 9, 12, 15, 0, 0, 0, time.UTC),
		Repository:       Repository{NameWithOwner: "didx-xyz/tofu"},
		ReviewDecision:   "REVIEW_REQUIRED",
		Mergeable:        "MERGEABLE",
		MergeStateStatus: "BLOCKED",
	}
	if mutate != nil {
		mutate(&pr)
	}
	return pr
}

func withChecks(state string) func(*PullRequest) {
	return func(pr *PullRequest) {
		pr.Commits = Commits{Nodes: []CommitNode{{
			Commit: Commit{StatusCheckRollup: &StatusCheckRollup{State: state}},
		}}}
	}
}

func TestClassifyReportsTheMostActionableReason(t *testing.T) {
	now := time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC)
	cfg := Config{StaleDraftAfter: 14 * 24 * time.Hour}

	cases := []struct {
		name     string
		role     string
		pr       PullRequest
		state    attention.State
		severity attention.Severity
		label    string
	}{
		{
			name: "a review request is on you whatever the branch looks like",
			role: roleReviewer,
			pr: pull(126, func(pr *PullRequest) {
				pr.MergeStateStatus = "DIRTY"
				withChecks("FAILURE")(pr)
			}),
			state: attention.StateNeedsAttention, severity: attention.SeverityWarning,
			label: labelReviewRequested,
		},
		{
			name: "conflicts on your own pull request need a rebase",
			role: roleAuthor,
			pr: pull(963, func(pr *PullRequest) {
				pr.MergeStateStatus = "DIRTY"
				pr.Mergeable = "CONFLICTING"
			}),
			state: attention.StateNeedsAttention, severity: attention.SeverityWarning,
			label: labelRebaseRequired,
		},
		{
			name:  "red checks are a failure, not a warning to squint at",
			role:  roleAuthor,
			pr:    pull(880, withChecks("FAILURE")),
			state: attention.StateFailed, severity: attention.SeverityWarning,
			label: labelChecksFailing,
		},
		{
			name: "requested changes come back to you",
			role: roleAuthor,
			pr: pull(881, func(pr *PullRequest) {
				pr.ReviewDecision = "CHANGES_REQUESTED"
				withChecks("SUCCESS")(pr)
			}),
			state: attention.StateNeedsAttention, severity: attention.SeverityWarning,
			label: labelChangesRequested,
		},
		{
			name:  "running checks are work in flight, not a summons",
			role:  roleAuthor,
			pr:    pull(882, withChecks("PENDING")),
			state: attention.StateWorking, severity: attention.SeverityInfo,
			label: labelChecksRunning,
		},
		{
			name: "a branch behind its base needs the same rebase",
			role: roleAuthor,
			pr: pull(862, func(pr *PullRequest) {
				pr.MergeStateStatus = "BEHIND"
				withChecks("SUCCESS")(pr)
			}),
			state: attention.StateNeedsAttention, severity: attention.SeverityWarning,
			label: labelRebaseRequired,
		},
		{
			name: "approved and clean is the one that wants a click",
			role: roleAuthor,
			pr: pull(1001, func(pr *PullRequest) {
				pr.ReviewDecision = "APPROVED"
				pr.MergeStateStatus = "CLEAN"
				withChecks("SUCCESS")(pr)
			}),
			state: attention.StateNeedsAttention, severity: attention.SeverityWarning,
			label: labelReadyToMerge,
		},
		{
			name: "approved but not mergeable is somebody else's turn",
			role: roleAuthor,
			pr: pull(1002, func(pr *PullRequest) {
				pr.ReviewDecision = "APPROVED"
				pr.MergeStateStatus = "BLOCKED"
				withChecks("SUCCESS")(pr)
			}),
			state: attention.StateWaiting, severity: attention.SeverityInfo,
			label: labelApproved,
		},
		{
			name:  "waiting for a reviewer is waiting",
			role:  roleAuthor,
			pr:    pull(1003, withChecks("SUCCESS")),
			state: attention.StateWaiting, severity: attention.SeverityInfo,
			label: labelAwaitingReview,
		},
		{
			name: "a draft stays out of the queue however red it is",
			role: roleAuthor,
			pr: pull(1471, func(pr *PullRequest) {
				pr.IsDraft = true
				pr.MergeStateStatus = "DIRTY"
				pr.Mergeable = "CONFLICTING"
				withChecks("FAILURE")(pr)
			}),
			state: attention.StateWaiting, severity: attention.SeverityInfo,
			label: labelDraft,
		},
		{
			name: "a draft nobody has touched is called stale",
			role: roleAuthor,
			pr: pull(1472, func(pr *PullRequest) {
				pr.IsDraft = true
				pr.UpdatedAt = now.Add(-15 * 24 * time.Hour)
			}),
			state: attention.StateWaiting, severity: attention.SeverityInfo,
			label: labelStaleDraft,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state, severity, label := classify(tc.pr, tc.role, cfg, now)
			if state != tc.state || severity != tc.severity || label != tc.label {
				t.Errorf("classify = %s/%s/%q, want %s/%s/%q",
					state, severity, label, tc.state, tc.severity, tc.label)
			}
			if state.NeedsAttention() != (tc.state != attention.StateWorking && tc.state != attention.StateWaiting) {
				t.Errorf("%s ended up in the attention queue: %v", label, state.NeedsAttention())
			}
		})
	}
}

func TestClassifyLeavesDraftsPlainWhenStalenessIsDisabled(t *testing.T) {
	pr := pull(1, func(pr *PullRequest) {
		pr.IsDraft = true
		pr.UpdatedAt = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	})
	_, _, label := classify(pr, roleAuthor, Config{}, time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC))
	if label != labelDraft {
		t.Errorf("label = %q, want %q", label, labelDraft)
	}
}

func TestQueueOrderPutsTheCheapestWorkFirst(t *testing.T) {
	now := time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC)
	cfg := Config{PriorityRepos: map[string]bool{"didx-xyz/tofu": true}}

	mergeable := pull(1, func(pr *PullRequest) {
		pr.ReviewDecision = "APPROVED"
		pr.MergeStateStatus = "CLEAN"
	})
	inTofu := pull(2, nil)
	elsewhere := pull(3, func(pr *PullRequest) {
		pr.Repository = Repository{NameWithOwner: "mcgeerdev/portfolio"}
	})
	broken := pull(4, withChecks("FAILURE"))

	items := Normalize(Inbox{
		ReviewRequested: []PullRequest{inTofu, elsewhere},
		Authored:        []PullRequest{mergeable, broken},
	}, cfg, now)

	priorities := map[string]int{}
	tones := map[string]attention.Tone{}
	for _, item := range items {
		priorities[item.Label] = item.Priority
		tones[item.Label] = item.Tone
	}

	// One click from finished beats a review somebody is waiting on, which
	// beats the same review in a repository nobody named, which beats work
	// that is merely broken.
	if !(priorities[labelReadyToMerge] > priorities[labelReviewRequested]) {
		t.Errorf("ready to merge (%d) did not outrank a review request (%d)",
			priorities[labelReadyToMerge], priorities[labelReviewRequested])
	}
	if !(priorities[labelReviewRequested] > priorities[labelChecksFailing]) {
		t.Errorf("a review request (%d) did not outrank failing checks (%d)",
			priorities[labelReviewRequested], priorities[labelChecksFailing])
	}
	if tones[labelReadyToMerge] != attention.ToneReady {
		t.Errorf("ready to merge has tone %q, want the one tone nothing else uses",
			tones[labelReadyToMerge])
	}

	// Both review requests carry the same label, so compare the items.
	var tofuRank, elsewhereRank int
	for _, item := range items {
		switch item.Context["repo"] {
		case "didx-xyz/tofu":
			if item.Label == labelReviewRequested {
				tofuRank = item.Priority
			}
		case "mcgeerdev/portfolio":
			elsewhereRank = item.Priority
		}
	}
	if !(tofuRank > elsewhereRank) {
		t.Errorf("a review in a priority repo (%d) did not outrank one elsewhere (%d)",
			tofuRank, elsewhereRank)
	}
}

func TestPriorityRepoMatchingIgnoresCase(t *testing.T) {
	// GitHub answers with whatever case the repository was created in, so a
	// config file is allowed to disagree about it.
	set, err := NewPriorityRepos([]string{"DIDx-XYZ/Tofu"})
	if err != nil {
		t.Fatalf("NewPriorityRepos: %v", err)
	}

	items := Normalize(
		Inbox{ReviewRequested: []PullRequest{pull(1, nil)}},
		Config{PriorityRepos: set},
		time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC),
	)
	if items[0].Priority != attention.PriorityBlockingOthers {
		t.Errorf("priority = %d, want %d: didx-xyz/tofu did not match DIDx-XYZ/Tofu",
			items[0].Priority, attention.PriorityBlockingOthers)
	}

	if _, err := NewPriorityRepos([]string{"tofu"}); err == nil {
		t.Error("a bare repository name was accepted; a typo has to fail startup, not quietly rank nothing")
	}
}

func TestNormalizeCarriesIdentityAndAnOpenLink(t *testing.T) {
	now := time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC)
	updated := time.Date(2026, 9, 12, 15, 5, 16, 0, time.UTC)

	inbox := Inbox{
		Login: "mcgeerdev",
		Authored: []PullRequest{pull(1001, func(pr *PullRequest) {
			pr.UpdatedAt = updated
			pr.ReviewDecision = "APPROVED"
			pr.MergeStateStatus = "CLEAN"
			pr.Author = &Actor{Login: "mcgeerdev"}
			withChecks("SUCCESS")(pr)
		})},
	}

	items := Normalize(inbox, Config{}, now)
	if len(items) != 1 {
		t.Fatalf("got %d items", len(items))
	}
	item := items[0]

	if item.ID != "github:didx-xyz/tofu#1001" {
		t.Errorf("id = %q", item.ID)
	}
	if item.Source != SourceName {
		t.Errorf("source = %q", item.Source)
	}
	if item.Title != "didx-xyz/tofu#1001 · Move the RDS credentials into Aurora" {
		t.Errorf("title = %q", item.Title)
	}
	// Poll time would make every pull request look like it changed a second
	// ago, which is the opposite of what the dashboard needs.
	if !item.UpdatedAt.Equal(updated) {
		t.Errorf("updated_at = %s, want the pull request's own %s", item.UpdatedAt, updated)
	}

	for key, want := range map[string]string{
		"repo":            "didx-xyz/tofu",
		"number":          "1001",
		"role":            roleAuthor,
		"pr_state":        labelReadyToMerge,
		"review_decision": "APPROVED",
		"merge_state":     "CLEAN",
		"checks":          "SUCCESS",
		"author":          "mcgeerdev",
	} {
		if got := item.Context[key]; got != want {
			t.Errorf("context[%s] = %q, want %q", key, got, want)
		}
	}
	if _, ok := item.Context["draft"]; ok {
		t.Error("a non-draft reported draft context")
	}

	if len(item.Actions) != 1 {
		t.Fatalf("actions = %+v", item.Actions)
	}
	action := item.Actions[0]
	if action.Method != "GET" || action.Href != "https://github.com/didx-xyz/tofu/pull/1001" {
		t.Errorf("action = %+v, want a GET to the pull request", action)
	}
}

func TestNormalizeReportsAPullRequestOnceAsTheHalfThatWantsYou(t *testing.T) {
	pr := pull(770, nil)
	inbox := Inbox{
		Authored:        []PullRequest{pr},
		ReviewRequested: []PullRequest{pr},
	}

	items := Normalize(inbox, Config{}, time.Now())
	if len(items) != 1 {
		t.Fatalf("got %d items, want the duplicate collapsed", len(items))
	}
	if items[0].Context["role"] != roleReviewer {
		t.Errorf("role = %q, want %q", items[0].Context["role"], roleReviewer)
	}
	if items[0].Context["pr_state"] != labelReviewRequested {
		t.Errorf("pr_state = %q", items[0].Context["pr_state"])
	}
}
