package httpapi

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/devanmcgeer/attentiond/internal/attention"
)

func TestJobsListsOnlyItemsWithAJobNewestFirst(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	handler, store := newQueueServer(t, attention.StoreConfig{StaleAfter: 30 * 24 * time.Hour}, &now)

	old := authoredPull("didx-xyz/tofu#1")
	old.UpdatedAt = now.Add(-60 * 24 * time.Hour)
	store.ReplaceSource("github", []attention.Item{
		old,
		authoredPull("didx-xyz/tofu#2"),
		authoredPull("didx-xyz/tofu#3"),
	})

	// #1 finished two months ago and has gone stale since; #2 is running now.
	clock := now
	now = now.Add(-60 * 24 * time.Hour)
	if _, err := store.QueueJob("github:didx-xyz/tofu#1", "agent-comments"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FinishJob("github:didx-xyz/tofu#1", "pushed", "abc123"); err != nil {
		t.Fatal(err)
	}
	now = clock
	if _, err := store.QueueJob("github:didx-xyz/tofu#2", "rebase"); err != nil {
		t.Fatal(err)
	}

	list := decodeList(t, do(t, handler, http.MethodGet, "/api/jobs", ""))
	if list.Count != 2 || len(list.Items) != 2 {
		t.Fatalf("jobs = %+v", list.Items)
	}
	if list.Items[0].ID != "github:didx-xyz/tofu#2" || list.Items[1].ID != "github:didx-xyz/tofu#1" {
		t.Fatalf("not newest job first: %s, %s", list.Items[0].ID, list.Items[1].ID)
	}
	if !list.Items[1].Stale || list.StaleCount != 1 {
		t.Errorf("the stale item's finished job was hidden or not counted: %+v", list)
	}
	if list.Items[0].Job.Status != attention.JobQueued || list.Items[0].Label != "agent rebase" {
		t.Errorf("queued job reads %+v", list.Items[0])
	}

	dismiss := action(t, list.Items[1], "clear")
	if dismiss.Label != "Dismiss" {
		t.Errorf("finished job action = %+v", dismiss)
	}
	for _, candidate := range list.Items[0].Actions {
		if candidate.Label == "Dismiss" {
			t.Error("a pending job offers Dismiss")
		}
	}

	if recorder := do(t, handler, http.MethodPost, strings.TrimPrefix(dismiss.Href, "http://127.0.0.1:7717"), ""); recorder.Code != http.StatusOK {
		t.Fatalf("dismiss: %d %s", recorder.Code, recorder.Body)
	}
	if list := decodeList(t, do(t, handler, http.MethodGet, "/api/jobs", "")); list.Count != 1 {
		t.Fatalf("the dismissed job is still listed: %+v", list.Items)
	}
}
