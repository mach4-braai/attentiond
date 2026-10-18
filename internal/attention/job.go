package attention

import "time"

// JobStatus is where a tool run is in its life.
type JobStatus string

const (
	JobQueued   JobStatus = "queued"
	JobRunning  JobStatus = "running"
	JobFinished JobStatus = "finished"
)

// The tools a job can run, each a script named <tool>.sh in the tools
// directory.
const (
	ToolRebase        = "rebase"
	ToolAgentRebase   = "agent-rebase"
	ToolAgentComments = "agent-comments"
)

// Tools lists every tool, in the order they are documented.
var Tools = []string{ToolRebase, ToolAgentRebase, ToolAgentComments}

// ResultNeedsConflicts is rebase reporting conflicts it did not try to
// resolve. The runner hands those to agent-rebase.
const ResultNeedsConflicts = "needs-conflicts"

// Results that change how an item reads. Any other RESULT word leaves the
// source's label in place.
const (
	// ResultNeedsHuman is a tool saying it stopped short and somebody has to
	// look.
	ResultNeedsHuman = "needs-human"
	// ResultFailed is the runner's word for a tool that printed no RESULT
	// line, ran out of time, or could not start.
	ResultFailed = "failed"
)

// The labels a job puts on its item. Both belong in [notify] labels: they are
// the reason to run tools unattended at all.
const (
	LabelNeedsHuman  = "needs human"
	LabelAgentFailed = "agent failed"
)

// Job is the latest tool run for one item. Like a decision it lives beside
// the item rather than on it, because every poll replaces the item whole.
type Job struct {
	// Item is the item id, as built by Key.
	Item string `json:"item"`
	// Tool is the script name without .sh: rebase, agent-rebase or
	// agent-comments.
	Tool   string    `json:"tool"`
	Status JobStatus `json:"status"`
	// Result is the RESULT word the tool printed, or ResultFailed.
	Result string `json:"result,omitempty"`
	// Detail is the rest of the RESULT line, or why the run failed.
	Detail string `json:"detail,omitempty"`
	// Log is the absolute path of the run's captured output.
	Log        string     `json:"log,omitempty"`
	QueuedAt   time.Time  `json:"queued_at"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// Pending reports whether the job has yet to finish.
func (j Job) Pending() bool {
	return j.Status == JobQueued || j.Status == JobRunning
}

// latest is when anything last happened to the job.
func (j Job) latest() time.Time {
	switch {
	case j.FinishedAt != nil:
		return *j.FinishedAt
	case j.StartedAt != nil:
		return *j.StartedAt
	}
	return j.QueuedAt
}

// overlayJob writes a job's display onto a copy of its item. A running tool
// takes the item out of the queue, because nothing is waiting on you; a tool
// that gave up puts it back with a label that says so.
func overlayJob(item *Item, job Job) {
	shown := job
	item.Job = &shown
	if latest := job.latest(); latest.After(item.UpdatedAt) {
		item.UpdatedAt = latest
	}

	switch {
	case job.Pending():
		item.Label, item.Tone = "agent "+job.Tool, ToneActive
		item.State, item.Priority = StateWorking, PriorityBackground
	case job.Result == ResultNeedsHuman:
		item.Label, item.Tone = LabelNeedsHuman, ToneAttention
		item.State, item.Priority = StateNeedsAttention, PriorityActionable
	case job.Result == ResultFailed:
		item.Label, item.Tone = LabelAgentFailed, ToneFailed
		item.State, item.Priority = StateNeedsAttention, PriorityActionable
	}
}
