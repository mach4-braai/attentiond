package attention

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// Every poll replaces the item whole, so a label written onto the stored item
// would be gone a minute later. The job has to come back on every read.
func TestAJobStaysOnItsItemThroughAPoll(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	store := testStore(t, StoreConfig{}, &now)
	store.ReplaceSource("github", []Item{authored("didx-xyz/tofu#1", "rebase required")})

	if _, err := store.QueueJob("github:didx-xyz/tofu#1", "rebase"); err != nil {
		t.Fatalf("queue: %v", err)
	}
	if _, err := store.StartJob("github:didx-xyz/tofu#1", "/tmp/rebase.log"); err != nil {
		t.Fatalf("start: %v", err)
	}

	now = now.Add(time.Minute)
	store.ReplaceSource("github", []Item{authored("didx-xyz/tofu#1", "rebase required")})

	item, _ := store.Get("github:didx-xyz/tofu#1")
	if item.Label != "agent rebase" || item.Tone != ToneActive || item.State != StateWorking {
		t.Fatalf("the poll wiped the running job off the item: %+v", item)
	}
	if item.Job == nil || item.Job.Status != JobRunning || item.Job.Log != "/tmp/rebase.log" {
		t.Fatalf("job = %+v", item.Job)
	}
	if got := queued(store); len(got) != 0 {
		t.Fatalf("a pull request a tool is working on is still waiting on you: %v", got)
	}

	if _, err := store.FinishJob("github:didx-xyz/tofu#1", "needs-conflicts", "main.tf"); err != nil {
		t.Fatalf("finish: %v", err)
	}
	store.ReplaceSource("github", []Item{authored("didx-xyz/tofu#1", "rebase required")})
	item, _ = store.Get("github:didx-xyz/tofu#1")
	if item.Label != "rebase required" || item.Job == nil || item.Job.Result != "needs-conflicts" {
		t.Fatalf("a result that changes nothing should keep the source label and the job: %+v", item)
	}
}

func TestAToolThatGivesUpPutsTheItemBackInTheQueue(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	store := testStore(t, StoreConfig{}, &now)
	quiet := authored("didx-xyz/tofu#1", "awaiting review")
	quiet.State, quiet.Priority = StateWaiting, PriorityBackground
	store.ReplaceSource("github", []Item{quiet})

	for result, label := range map[string]string{ResultNeedsHuman: LabelNeedsHuman, ResultFailed: LabelAgentFailed} {
		if _, err := store.QueueJob("github:didx-xyz/tofu#1", "agent-comments"); err != nil {
			t.Fatal(err)
		}
		if _, err := store.FinishJob("github:didx-xyz/tofu#1", result, "unclear"); err != nil {
			t.Fatal(err)
		}
		queue := store.Attention()
		if len(queue) != 1 || queue[0].Label != label || queue[0].Priority != PriorityActionable {
			t.Fatalf("%s: queue = %+v", result, queue)
		}
	}
}

func TestClearDismissesAFinishedJobOnly(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	store := testStore(t, StoreConfig{}, &now)
	store.ReplaceSource("github", []Item{authored("didx-xyz/tofu#1", "rebase required")})

	if _, err := store.QueueJob("github:didx-xyz/tofu#1", "rebase"); err != nil {
		t.Fatal(err)
	}
	if item, _ := store.Clear("github:didx-xyz/tofu#1"); item.Job == nil {
		t.Fatal("clear dropped a job whose tool has yet to report")
	}

	if _, err := store.FinishJob("github:didx-xyz/tofu#1", ResultNeedsHuman, "conflict in main.tf"); err != nil {
		t.Fatal(err)
	}
	item, err := store.Clear("github:didx-xyz/tofu#1")
	if err != nil {
		t.Fatal(err)
	}
	if item.Job != nil || item.Label != "rebase required" {
		t.Fatalf("clear did not dismiss the finished job: %+v", item)
	}
	if len(store.JobItems()) != 0 {
		t.Fatal("the dismissed job is still listed")
	}
}

func TestJobsSurviveARestart(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "decisions.json")

	first := testStore(t, StoreConfig{DecisionPath: path}, &now)
	first.ReplaceSource("github", []Item{
		authored("didx-xyz/tofu#1", "rebase required"),
		authored("didx-xyz/tofu#2", "awaiting review"),
	})
	if _, err := first.QueueJob("github:didx-xyz/tofu#1", "rebase"); err != nil {
		t.Fatal(err)
	}
	if _, err := first.StartJob("github:didx-xyz/tofu#1", "/tmp/a.log"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if _, err := first.QueueJob("github:didx-xyz/tofu#2", "agent-comments"); err != nil {
		t.Fatal(err)
	}
	if _, err := first.FinishJob("github:didx-xyz/tofu#2", ResultNeedsHuman, "ambiguous"); err != nil {
		t.Fatal(err)
	}

	second := testStore(t, StoreConfig{DecisionPath: path}, &now)
	jobs := second.Jobs()
	if len(jobs) != 2 || jobs[0].Item != "github:didx-xyz/tofu#2" || jobs[1].Status != JobRunning {
		t.Fatalf("jobs after the restart = %+v", jobs)
	}

	second.ReplaceSource("github", []Item{authored("didx-xyz/tofu#2", "awaiting review")})
	if item, _ := second.Get("github:didx-xyz/tofu#2"); item.Label != LabelNeedsHuman {
		t.Fatalf("the restored result is not on the item: %+v", item)
	}
}

func TestAJobChangeNeedsAJobInTheRightState(t *testing.T) {
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	store := testStore(t, StoreConfig{}, &now)
	store.ReplaceSource("github", []Item{authored("didx-xyz/tofu#1", "rebase required")})

	if _, err := store.StartJob("github:didx-xyz/tofu#1", ""); !errors.Is(err, ErrJobMissing) {
		t.Fatalf("starting a job never queued: err = %v", err)
	}
	if _, err := store.FinishJob("github:didx-xyz/tofu#1", "rebased", ""); !errors.Is(err, ErrJobMissing) {
		t.Fatalf("finishing a job never queued: err = %v", err)
	}
	if _, err := store.QueueJob("github:didx-xyz/tofu#404", "rebase"); !errors.Is(err, ErrItemMissing) {
		t.Fatalf("queueing on an item the store does not hold: err = %v", err)
	}
}
