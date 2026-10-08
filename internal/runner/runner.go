// Package runner acts on watched pull requests. It decides when a tool runs,
// starts it as a child process, and records what it reported. It never runs
// git or a model itself, so a hung agent cannot stall polling or the API.
package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/devanmcgeer/attentiond/internal/attention"
	"github.com/devanmcgeer/attentiond/internal/github"
)

// CommentSource fetches the comments on one pull request, already filtered
// to the allowed authors. Nil means agent-comments never runs.
type CommentSource func(ctx context.Context, repo string, number int) ([]github.Comment, error)

// Config is what the runner needs from [tools].
type Config struct {
	// Dir holds <tool>.sh for every tool.
	Dir string
	// LogDir receives one log file per run.
	LogDir string
	// Checkouts maps lower-case owner/name to the local clone of that repo.
	Checkouts map[string]string
	// Timeouts bound each tool. A tool missing here gets defaultTimeout.
	Timeouts map[string]time.Duration
	// CommentQuiet is how long the newest unhandled comment must sit before
	// agent-comments starts.
	CommentQuiet time.Duration
	// RebaseBehind also rebases a pull request that is only behind its base.
	RebaseBehind bool
	// Interval is how often watched items are checked for a trigger. The
	// check reads the store and nothing else, so it can be frequent.
	Interval time.Duration
	// CommentPoll is how often each watched pull request's comments are
	// fetched from GitHub.
	CommentPoll time.Duration
	// WaitDelay is how long a tool has between SIGTERM and SIGKILL once it
	// runs out of time.
	WaitDelay time.Duration
	// Now is the clock for the quiet period. Nil means time.Now.
	Now func() time.Time
}

// defaultTimeout bounds a tool with no configured timeout.
const defaultTimeout = 30 * time.Minute

// fetch is the last comment read for one pull request.
type fetch struct {
	at       time.Time
	comments []github.Comment
}

// Runner runs at most one tool per item at a time. Triggers that arrive while
// a tool is busy wait behind it, one entry per tool.
type Runner struct {
	store    *attention.Store
	cfg      Config
	comments CommentSource
	log      *slog.Logger
	now      func() time.Time

	mu sync.Mutex
	// busy holds the items a tool is running on.
	busy map[string]bool
	// waiting is item id to the tools queued behind the running one.
	waiting map[string][]string
	// armed is item id to whether the rebase condition held at the last
	// check. Rebase fires when it starts to hold, not while it holds:
	// GitHub keeps reporting a conflict until it recomputes after the push,
	// and a pull request a tool could not fix stays conflicting.
	armed map[string]bool
	// fetched is item id to its last comment read.
	fetched map[string]fetch
	// resume is item id to the tool a restart interrupted, rerun once the
	// item is polled again.
	resume map[string]string
	wg     sync.WaitGroup
}

// New returns a runner. It does nothing until Run is called.
func New(store *attention.Store, cfg Config, comments CommentSource, log *slog.Logger) *Runner {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 5 * time.Second
	}
	if cfg.CommentPoll <= 0 {
		cfg.CommentPoll = time.Minute
	}
	return &Runner{
		store:    store,
		cfg:      cfg,
		comments: comments,
		log:      log,
		now:      now,
		busy:     map[string]bool{},
		waiting:  map[string][]string{},
		armed:    map[string]bool{},
		fetched:  map[string]fetch{},
		resume:   map[string]string{},
	}
}

// Run checks watched items until ctx is done, then waits for running tools to
// stop. A tool interrupted by shutdown keeps its running job, so the next
// start runs it again from scratch.
func (r *Runner) Run(ctx context.Context) {
	for _, job := range r.store.Jobs() {
		if job.Pending() {
			r.resume[job.Item] = job.Tool
			r.log.Info("tool run interrupted by a restart, will run again",
				"item_id", job.Item, "tool", job.Tool)
		}
	}

	ticker := time.NewTicker(r.cfg.Interval)
	defer ticker.Stop()
	r.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			r.wg.Wait()
			r.log.Info("tool runner stopped")
			return
		case <-ticker.C:
			r.tick(ctx)
		}
	}
}

// tick looks at every item once and fires the triggers that hold.
func (r *Runner) tick(ctx context.Context) {
	watches := make(map[string]attention.Watch)
	for _, watch := range r.store.Watches() {
		watches[watch.Item] = watch
	}

	seen := make(map[string]bool, len(watches))
	for _, item := range r.store.Items() {
		if ctx.Err() != nil {
			return
		}
		if tool, ok := r.resume[item.ID]; ok {
			delete(r.resume, item.ID)
			r.trigger(ctx, item.ID, tool)
		}
		watch, watched := watches[item.ID]
		repo, number, isPull := pull(item)
		if !watched || !isPull {
			continue
		}
		seen[item.ID] = true
		if r.rebaseEdge(item) {
			r.trigger(ctx, item.ID, attention.ToolRebase)
		}
		if r.commentsReady(ctx, item.ID, repo, number, watch.MadeAt) {
			r.trigger(ctx, item.ID, attention.ToolAgentComments)
		}
	}

	r.mu.Lock()
	for id := range r.armed {
		if !seen[id] {
			delete(r.armed, id)
		}
	}
	for id := range r.fetched {
		if !seen[id] {
			delete(r.fetched, id)
		}
	}
	r.mu.Unlock()
}

// rebaseEdge reports whether the rebase condition has just started to hold.
//
// The first look at an item after a start has no previous answer. A rebase
// job already on the item while the condition holds means it was acted on
// before the restart, so that counts as already holding.
func (r *Runner) rebaseEdge(item attention.Item) bool {
	state, mergeable := item.Context["merge_state"], item.Context["mergeable"]
	holds := state == "DIRTY" || mergeable == "CONFLICTING" || (r.cfg.RebaseBehind && state == "BEHIND")

	r.mu.Lock()
	defer r.mu.Unlock()
	before, known := r.armed[item.ID]
	if !known {
		before = holds && item.Job != nil &&
			(item.Job.Tool == attention.ToolRebase || item.Job.Tool == attention.ToolAgentRebase)
	}
	r.armed[item.ID] = holds
	return holds && !before
}

// commentsReady reports whether a pull request has unhandled comments whose
// newest has been quiet for CommentQuiet, fetching them when the last read is
// older than CommentPoll.
func (r *Runner) commentsReady(ctx context.Context, id, repo string, number int, watchedAt time.Time) bool {
	if r.comments == nil {
		return false
	}
	now := r.now()
	r.mu.Lock()
	last := r.fetched[id]
	r.mu.Unlock()

	if now.Sub(last.at) >= r.cfg.CommentPoll {
		comments, err := r.comments(ctx, repo, number)
		if err != nil {
			r.log.Warn("comments not fetched", "item_id", id, "error", err)
			comments = last.comments
		}
		last = fetch{at: now, comments: comments}
		r.mu.Lock()
		r.fetched[id] = last
		r.mu.Unlock()
	}

	pending := r.unhandled(id, last.comments, watchedAt)
	if len(pending) == 0 {
		return false
	}
	newest := pending[0].CreatedAt
	for _, comment := range pending[1:] {
		if comment.CreatedAt.After(newest) {
			newest = comment.CreatedAt
		}
	}
	return now.Sub(newest) >= r.cfg.CommentQuiet
}

// unhandled drops the comments a tool was already given, and the ones written
// before the watch. Watching a pull request asks for help with what comes
// next, not for every old conversation on it to be reopened.
func (r *Runner) unhandled(id string, comments []github.Comment, watchedAt time.Time) []github.Comment {
	handled := r.store.HandledComments(id)
	out := make([]github.Comment, 0, len(comments))
	for _, comment := range comments {
		if !handled[comment.ID] && comment.CreatedAt.After(watchedAt) {
			out = append(out, comment)
		}
	}
	return out
}

// trigger runs a tool on an item, or queues it behind the tool already
// running there. A tool already waiting is not queued twice.
func (r *Runner) trigger(ctx context.Context, id, tool string) {
	r.mu.Lock()
	if r.busy[id] {
		for _, queued := range r.waiting[id] {
			if queued == tool {
				r.mu.Unlock()
				return
			}
		}
		r.waiting[id] = append(r.waiting[id], tool)
		r.mu.Unlock()
		r.log.Info("tool queued behind a running one", "item_id", id, "tool", tool)
		return
	}
	r.busy[id] = true
	r.wg.Add(1)
	r.mu.Unlock()

	go r.work(ctx, id, tool)
}

// work runs tools on one item until nothing is left for it: the triggered
// tool, then what it chains to, then whatever queued up meanwhile.
func (r *Runner) work(ctx context.Context, id, tool string) {
	defer r.wg.Done()
	for tool != "" {
		chained := r.run(ctx, id, tool)

		r.mu.Lock()
		switch {
		case ctx.Err() != nil:
			tool = ""
		case chained != "":
			// The chained tool goes first, and replaces a copy of itself
			// that was waiting.
			tool = chained
			r.waiting[id] = without(r.waiting[id], chained)
		case len(r.waiting[id]) > 0:
			tool = r.waiting[id][0]
			r.waiting[id] = r.waiting[id][1:]
		default:
			tool = ""
		}
		if tool == "" {
			delete(r.busy, id)
			delete(r.waiting, id)
		}
		r.mu.Unlock()
	}
}

// run takes one tool from queued to finished and returns the tool it chains
// to, if any.
func (r *Runner) run(ctx context.Context, id, tool string) string {
	item, ok := r.store.Get(id)
	if !ok {
		r.settle(id, attention.ResultFailed, "the pull request is no longer reported")
		return ""
	}
	repo, number, ok := pull(item)
	if !ok {
		r.settle(id, attention.ResultFailed, "not a pull request")
		return ""
	}

	var comments []github.Comment
	if tool == attention.ToolAgentComments {
		var err error
		comments, err = r.pendingComments(ctx, item, repo, number)
		if err != nil {
			r.fail(id, tool, "comments: "+err.Error())
			return ""
		}
		if len(comments) == 0 {
			// Queued behind another tool, or rerun after a restart, and
			// everything it was for has been handled since.
			r.settle(id, "up-to-date", "no unhandled comments")
			return ""
		}
	}

	if _, err := r.store.QueueJob(id, tool); err != nil {
		r.log.Warn("tool not queued", "item_id", id, "tool", tool, "error", err)
		return ""
	}
	checkout := r.cfg.Checkouts[strings.ToLower(repo)]
	if checkout == "" {
		r.finish(id, attention.ResultFailed, "no checkout configured for "+repo)
		return ""
	}
	if err := os.MkdirAll(r.cfg.LogDir, 0o755); err != nil {
		r.finish(id, attention.ResultFailed, "log dir: "+err.Error())
		return ""
	}
	logPath := filepath.Join(r.cfg.LogDir, logName(id, tool, r.now()))
	if _, err := r.store.StartJob(id, logPath); err != nil {
		r.log.Warn("tool not started", "item_id", id, "tool", tool, "error", err)
		return ""
	}

	result, detail, interrupted := r.execute(ctx, tool, repo, number, checkout, comments, logPath)
	if interrupted {
		return ""
	}
	r.finish(id, result, detail)

	if tool == attention.ToolAgentComments {
		// Handled whatever the result: a tool that failed on a review would
		// fail on it again every quiet period, and the result already put
		// the pull request back in front of a human.
		ids := make([]string, 0, len(comments))
		for _, comment := range comments {
			ids = append(ids, comment.ID)
		}
		r.store.MarkCommentsHandled(id, ids)
	}
	if tool == attention.ToolRebase && result == attention.ResultNeedsConflicts {
		return attention.ToolAgentRebase
	}
	return ""
}

// pendingComments reads the comments afresh for a run. A cached read could
// be a minute old, and the run should see the whole review.
func (r *Runner) pendingComments(ctx context.Context, item attention.Item, repo string, number int) ([]github.Comment, error) {
	if r.comments == nil {
		return nil, nil
	}
	var watchedAt time.Time
	watched := false
	for _, watch := range r.store.Watches() {
		if watch.Item == item.ID {
			watchedAt, watched = watch.MadeAt, true
		}
	}
	if !watched {
		return nil, nil
	}
	comments, err := r.comments(ctx, repo, number)
	if err != nil {
		return nil, err
	}
	return r.unhandled(item.ID, comments, watchedAt), nil
}

// execute runs one script and reads its RESULT line. interrupted means the
// daemon is stopping: the job is left running for the next start.
func (r *Runner) execute(ctx context.Context, tool, repo string, number int, checkout string, comments []github.Comment, logPath string) (result, detail string, interrupted bool) {
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return attention.ResultFailed, "log: " + err.Error(), false
	}
	defer logFile.Close()

	env := append(os.Environ(), "ATTENTIOND_CHECKOUT="+checkout)
	if tool == attention.ToolAgentComments {
		path, err := writeComments(comments)
		if err != nil {
			return attention.ResultFailed, "comments file: " + err.Error(), false
		}
		defer os.Remove(path)
		env = append(env, "ATTENTIOND_COMMENTS="+path)
	}

	timeout := r.cfg.Timeouts[tool]
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	script := filepath.Join(r.cfg.Dir, tool+".sh")
	cmd := exec.CommandContext(runCtx, script, repo, strconv.Itoa(number))
	cmd.Dir = checkout
	cmd.Env = env
	scanner := &resultScanner{}
	output := io.MultiWriter(logFile, scanner)
	cmd.Stdout, cmd.Stderr = output, output
	// Its own process group, so SIGTERM reaches the git or omp the script
	// started rather than only the shell.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = r.cfg.WaitDelay

	r.log.Info("tool started", "tool", tool, "repo", repo, "number", number, "log", logPath)
	runErr := cmd.Run()
	if runCtx.Err() != nil && cmd.Process != nil {
		// Whatever outlived SIGTERM and the wait goes now.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}

	switch {
	case ctx.Err() != nil:
		return "", "", true
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		result, detail = attention.ResultFailed, "timed out after "+timeout.String()
	default:
		var ok bool
		if result, detail, ok = scanner.result(); !ok {
			result, detail = attention.ResultFailed, "no RESULT line"
			if runErr != nil {
				detail += ": " + runErr.Error()
			}
		}
	}
	fmt.Fprintf(logFile, "attentiond: %s %s\n", result, detail)
	return result, detail, false
}

// finish records a result, logging rather than returning a failure: the run
// is over either way.
func (r *Runner) finish(id, result, detail string) {
	if _, err := r.store.FinishJob(id, result, detail); err != nil {
		r.log.Warn("tool result not recorded", "item_id", id, "result", result, "error", err)
	}
}

// fail queues and finishes a job in one go, for a run that could not start.
func (r *Runner) fail(id, tool, detail string) {
	if _, err := r.store.QueueJob(id, tool); err == nil {
		r.finish(id, attention.ResultFailed, detail)
	}
}

// settle finishes a job a restart left pending, for a run that turned out to
// have nothing to do. A run that never queued a job leaves the item alone.
func (r *Runner) settle(id, result, detail string) {
	for _, job := range r.store.Jobs() {
		if job.Item == id && job.Pending() {
			r.finish(id, result, detail)
			return
		}
	}
}

// pull reads the repository and number a GitHub item carries.
func pull(item attention.Item) (string, int, bool) {
	if item.Source != github.SourceName {
		return "", 0, false
	}
	repo := item.Context["repo"]
	number, err := strconv.Atoi(item.Context["number"])
	return repo, number, repo != "" && err == nil && number > 0
}

// writeComments puts the comments for agent-comments in a temporary file.
func writeComments(comments []github.Comment) (string, error) {
	if comments == nil {
		comments = []github.Comment{}
	}
	file, err := os.CreateTemp("", "attentiond-comments-*.json")
	if err != nil {
		return "", err
	}
	if err := json.NewEncoder(file).Encode(comments); err != nil {
		file.Close()
		os.Remove(file.Name())
		return "", err
	}
	if err := file.Close(); err != nil {
		os.Remove(file.Name())
		return "", err
	}
	return file.Name(), nil
}

// logName is a run's log file name: the item id made safe for a path, the
// tool, and the start time.
func logName(id, tool string, started time.Time) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.':
			return r
		}
		return '_'
	}, id)
	return safe + "-" + tool + "-" + started.UTC().Format("20060102T150405Z") + ".log"
}

func without(tools []string, drop string) []string {
	out := tools[:0]
	for _, tool := range tools {
		if tool != drop {
			out = append(out, tool)
		}
	}
	return out
}
