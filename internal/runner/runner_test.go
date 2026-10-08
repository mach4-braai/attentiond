package runner

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/devanmcgeer/attentiond/internal/attention"
	"github.com/devanmcgeer/attentiond/internal/github"
)

const pullID = "github:didx-xyz/tofu#42"

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// fixture is a tools directory of fake scripts, a checkout, and a record file
// every script appends to.
type fixture struct {
	tools    string
	checkout string
	logs     string
	record   string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	root := t.TempDir()
	f := fixture{
		tools:    filepath.Join(root, "tools"),
		checkout: filepath.Join(root, "tofu"),
		logs:     filepath.Join(root, "runs"),
		record:   filepath.Join(root, "record"),
	}
	for _, dir := range []string{f.tools, f.checkout} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("RECORD", f.record)
	return f
}

// script writes tools/<name>.sh.
func (f fixture) script(t *testing.T, name, body string) {
	t.Helper()
	path := filepath.Join(f.tools, name+".sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func (f fixture) config() Config {
	return Config{
		Dir:          f.tools,
		LogDir:       f.logs,
		Checkouts:    map[string]string{"didx-xyz/tofu": f.checkout},
		Timeouts:     map[string]time.Duration{},
		CommentQuiet: 2 * time.Minute,
		Interval:     20 * time.Millisecond,
		CommentPoll:  10 * time.Millisecond,
		WaitDelay:    200 * time.Millisecond,
	}
}

// lines returns what the scripts recorded, in order.
func (f fixture) lines(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(f.record)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(data))
}

func pullRequest(mergeState string) attention.Item {
	return attention.Item{
		ID:       pullID,
		Source:   github.SourceName,
		Title:    "didx-xyz/tofu#42 · Move the RDS credentials",
		State:    attention.StateNeedsAttention,
		Severity: attention.SeverityWarning,
		Label:    "rebase required",
		Context: map[string]string{
			"repo": "didx-xyz/tofu", "number": "42", "role": "author",
			"merge_state": mergeState, "mergeable": "UNKNOWN",
		},
	}
}

func watchedStore(t *testing.T, path, mergeState string) *attention.Store {
	t.Helper()
	store := attention.NewStore(quiet, attention.StoreConfig{DecisionPath: path})
	store.ReplaceSource(github.SourceName, []attention.Item{pullRequest(mergeState)})
	if _, err := store.Watch(pullID); err != nil {
		t.Fatal(err)
	}
	return store
}

// start runs the runner until the test ends.
func start(t *testing.T, r *Runner) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func job(store *attention.Store) attention.Job {
	item, _ := store.Get(pullID)
	if item.Job == nil {
		return attention.Job{}
	}
	return *item.Job
}

func finished(store *attention.Store, tool string) func() bool {
	return func() bool {
		j := job(store)
		return j.Tool == tool && j.Status == attention.JobFinished
	}
}

// comments is a CommentSource that serves whatever the test last set.
type comments struct {
	mu   sync.Mutex
	list []github.Comment
}

func (c *comments) set(list ...github.Comment) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.list = list
}

func (c *comments) fetch(context.Context, string, int) ([]github.Comment, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]github.Comment(nil), c.list...), nil
}

func comment(id string, at time.Time) github.Comment {
	return github.Comment{ID: id, Author: "alice", Kind: github.CommentIssue, Body: "rename this", URL: "u", CreatedAt: at}
}

func TestOneToolPerItemAndTheRestQueueOncePerTool(t *testing.T) {
	f := newFixture(t)
	f.script(t, "rebase", `echo start-rebase >> "$RECORD"; sleep 0.4; echo end-rebase >> "$RECORD"; echo "RESULT: rebased abc123"`)
	f.script(t, "agent-comments", `echo start-comments >> "$RECORD"; cp "$ATTENTIOND_COMMENTS" "$RECORD.json"; echo end-comments >> "$RECORD"; echo "RESULT: pushed def456"`)

	store := watchedStore(t, "", "CLEAN")
	source := &comments{}
	source.set(comment("IC_1", time.Now().Add(time.Minute)))
	r := New(store, f.config(), source.fetch, quiet)

	ctx := context.Background()
	r.trigger(ctx, pullID, attention.ToolRebase)
	r.trigger(ctx, pullID, attention.ToolAgentComments)
	r.trigger(ctx, pullID, attention.ToolAgentComments)
	r.trigger(ctx, pullID, attention.ToolRebase)
	eventually(t, "the rebase to start", func() bool { return job(store).Status == attention.JobRunning })
	r.wg.Wait()

	want := []string{"start-rebase", "end-rebase", "start-comments", "end-comments", "start-rebase", "end-rebase"}
	if got := f.lines(t); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("runs = %v, want %v: overlapping, or a tool queued twice", got, want)
	}

	data, err := os.ReadFile(f.record + ".json")
	if err != nil {
		t.Fatalf("agent-comments did not get $ATTENTIOND_COMMENTS: %v", err)
	}
	var given []github.Comment
	if err := json.Unmarshal(data, &given); err != nil || len(given) != 1 || given[0].ID != "IC_1" {
		t.Fatalf("comments file = %s (%v)", data, err)
	}
	if !store.HandledComments(pullID)["IC_1"] {
		t.Error("the comment given to the tool was not marked handled")
	}
}

func TestTheLastResultLineWins(t *testing.T) {
	f := newFixture(t)
	f.script(t, "rebase", `echo "RESULT: rebased abc123"; echo "Rebasing (1/3)" >&2; echo "RESULT: up-to-date nothing to do"; printf 'trailing noise'`)
	store := watchedStore(t, "", "CLEAN")
	r := New(store, f.config(), nil, quiet)

	r.trigger(context.Background(), pullID, attention.ToolRebase)
	r.wg.Wait()

	got := job(store)
	if got.Result != "up-to-date" || got.Detail != "nothing to do" {
		t.Fatalf("job = %+v, want the last RESULT line", got)
	}
	log, err := os.ReadFile(got.Log)
	if err != nil || !strings.Contains(string(log), "Rebasing (1/3)") {
		t.Fatalf("stderr missing from the run log %q: %s (%v)", got.Log, log, err)
	}
}

func TestNoResultLineIsAFailure(t *testing.T) {
	f := newFixture(t)
	f.script(t, "rebase", `echo "RESULT: maybe?"; exit 3`)
	store := watchedStore(t, "", "CLEAN")
	r := New(store, f.config(), nil, quiet)

	r.trigger(context.Background(), pullID, attention.ToolRebase)
	r.wg.Wait()

	if got := job(store); got.Result != attention.ResultFailed || !strings.Contains(got.Detail, "no RESULT line") {
		t.Fatalf("job = %+v", got)
	}
}

func TestATimeoutKillsTheToolAndFails(t *testing.T) {
	f := newFixture(t)
	f.script(t, "rebase", `echo "RESULT: rebased early"; sleep 30; echo "RESULT: rebased late"`)
	store := watchedStore(t, "", "CLEAN")
	cfg := f.config()
	cfg.Timeouts[attention.ToolRebase] = 300 * time.Millisecond
	r := New(store, cfg, nil, quiet)

	began := time.Now()
	r.trigger(context.Background(), pullID, attention.ToolRebase)
	r.wg.Wait()

	if elapsed := time.Since(began); elapsed > 3*time.Second {
		t.Fatalf("the tool outlived its timeout by %s", elapsed)
	}
	if got := job(store); got.Result != attention.ResultFailed || !strings.Contains(got.Detail, "timed out") {
		t.Fatalf("job = %+v, want failed on timeout", got)
	}
}

func TestConflictsGoToAgentRebaseOnceAndOnlyOnce(t *testing.T) {
	f := newFixture(t)
	f.script(t, "rebase", `echo rebase >> "$RECORD"; echo "RESULT: needs-conflicts main.tf"`)
	f.script(t, "agent-rebase", `echo agent-rebase >> "$RECORD"; echo "RESULT: needs-human could not resolve main.tf"`)
	store := watchedStore(t, "", "DIRTY")
	start(t, New(store, f.config(), nil, quiet))

	eventually(t, "agent-rebase to finish", finished(store, attention.ToolAgentRebase))
	if got := job(store); got.Result != attention.ResultNeedsHuman {
		t.Fatalf("job = %+v", got)
	}

	// Still DIRTY: GitHub has not changed its answer, and running the pair
	// again on every check would loop forever.
	time.Sleep(200 * time.Millisecond)
	if got := f.lines(t); strings.Join(got, " ") != "rebase agent-rebase" {
		t.Fatalf("runs = %v, want rebase then agent-rebase, once", got)
	}
	if item, _ := store.Get(pullID); item.Label != attention.LabelNeedsHuman {
		t.Errorf("label = %q", item.Label)
	}
}

func TestARepoWithNoCheckoutFails(t *testing.T) {
	f := newFixture(t)
	f.script(t, "rebase", `echo rebase >> "$RECORD"; echo "RESULT: rebased abc"`)
	store := watchedStore(t, "", "CLEAN")
	cfg := f.config()
	cfg.Checkouts = nil
	r := New(store, cfg, nil, quiet)

	r.trigger(context.Background(), pullID, attention.ToolRebase)
	r.wg.Wait()

	got := job(store)
	if got.Status != attention.JobFinished || got.Result != attention.ResultFailed ||
		got.Detail != "no checkout configured for didx-xyz/tofu" {
		t.Fatalf("job = %+v", got)
	}
	if len(f.lines(t)) != 0 {
		t.Error("the tool ran with nowhere to work")
	}
}

// A daemon that stops mid-run leaves the job running on disk. The next start
// must run it again from scratch rather than show it running forever.
func TestARestartRunsAnInterruptedJobAgain(t *testing.T) {
	f := newFixture(t)
	f.script(t, "rebase", `echo rebase >> "$RECORD"; echo "RESULT: rebased abc123"`)
	path := filepath.Join(t.TempDir(), "decisions.json")

	before := watchedStore(t, path, "CLEAN")
	if _, err := before.QueueJob(pullID, attention.ToolRebase); err != nil {
		t.Fatal(err)
	}
	if _, err := before.StartJob(pullID, "/tmp/lost.log"); err != nil {
		t.Fatal(err)
	}

	after := attention.NewStore(quiet, attention.StoreConfig{DecisionPath: path})
	start(t, New(after, f.config(), nil, quiet))
	// The runner waits for the item to be polled again.
	time.Sleep(60 * time.Millisecond)
	after.ReplaceSource(github.SourceName, []attention.Item{pullRequest("CLEAN")})

	eventually(t, "the interrupted rebase to run again", finished(after, attention.ToolRebase))
	if got := job(after); got.Result != "rebased" || got.Log == "/tmp/lost.log" {
		t.Fatalf("job = %+v", got)
	}
	if got := f.lines(t); len(got) != 1 {
		t.Fatalf("runs = %v, want one", got)
	}
}

func TestHandledCommentsAreNotReplayedAfterARestart(t *testing.T) {
	f := newFixture(t)
	f.script(t, "agent-comments", `echo comments >> "$RECORD"; cp "$ATTENTIOND_COMMENTS" "$RECORD.json"; echo "RESULT: pushed abc"`)
	path := filepath.Join(t.TempDir(), "decisions.json")
	cfg := f.config()
	// Every comment below is older than the quiet period by this clock.
	cfg.Now = func() time.Time { return time.Now().Add(time.Hour) }

	source := &comments{}
	first := comment("IC_1", time.Now().Add(time.Minute))
	source.set(first)

	store := watchedStore(t, path, "CLEAN")
	ctx, cancel := context.WithCancel(context.Background())
	r := New(store, cfg, source.fetch, quiet)
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	eventually(t, "agent-comments to finish", finished(store, attention.ToolAgentComments))
	cancel()
	<-done

	restarted := attention.NewStore(quiet, attention.StoreConfig{DecisionPath: path})
	restarted.ReplaceSource(github.SourceName, []attention.Item{pullRequest("CLEAN")})
	start(t, New(restarted, cfg, source.fetch, quiet))
	time.Sleep(200 * time.Millisecond)
	if got := f.lines(t); len(got) != 1 {
		t.Fatalf("runs = %v: the restart replayed a handled comment", got)
	}

	source.set(first, comment("IC_2", time.Now().Add(2*time.Minute)))
	eventually(t, "the new comment to be handled", func() bool { return len(f.lines(t)) == 2 })
	eventually(t, "the second run to finish", finished(restarted, attention.ToolAgentComments))
	data, _ := os.ReadFile(f.record + ".json")
	var given []github.Comment
	if err := json.Unmarshal(data, &given); err != nil || len(given) != 1 || given[0].ID != "IC_2" {
		t.Fatalf("second run got %s, want only IC_2", data)
	}
}

func TestResultScannerReadsLinesSplitAcrossWrites(t *testing.T) {
	var s resultScanner
	for _, chunk := range []string{"noise\nRESU", "LT: rebased ab", "c123\r\n", strings.Repeat("x", maxLine*2), "\nRESULT: pushed"} {
		_, _ = s.Write([]byte(chunk))
	}
	word, detail, ok := s.result()
	if !ok || word != "pushed" || detail != "" {
		t.Fatalf("result = %q %q %v", word, detail, ok)
	}
}
