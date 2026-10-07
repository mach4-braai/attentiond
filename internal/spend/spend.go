// Package spend turns omp's usage index into attention items: what each omp
// session open in Herdr has cost today, and whether its advisor is running
// away.
//
// omp keeps one transcript per session and, in a directory named after it, one
// transcript per advisor and subagent. ~/.omp/stats.db indexes every assistant
// message and tool call by transcript, with agent_type saying which of the
// three wrote it. That is all this package reads.
package spend

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/devanmcgeer/attentiond/internal/attention"
	"github.com/devanmcgeer/attentiond/internal/herdr"
)

// SourceName is the item source this adapter owns.
const SourceName = "spend"

// Labels. Each names a reason, not an amount, because the label is what a
// snooze is held against: "$31.20" would move every poll and wake it.
const (
	LabelTracking       = "tracking"
	LabelAdvisorCost    = "advisor cost"
	LabelAdvisorLookups = "advisor lookups"
)

// Config holds the two advisor limits. Zero turns a limit off.
type Config struct {
	// AdvisorCost is the cost in dollars at which one advisor transcript
	// wants a human, counted over the transcript's whole life. A day
	// boundary is not a reason to forget that an advisor has already cost
	// $600.
	AdvisorCost float64
	// LookupsPerNote is the ratio of read, grep and glob calls to advise
	// calls at which an advisor is searching rather than advising.
	LookupsPerNote float64
	// MinLookups keeps the ratio quiet until there is enough of it to mean
	// something: 12 lookups and no note yet is an advisor getting started.
	MinLookups int
}

// Usage is what one session has spent.
type Usage struct {
	// Today is split by the agent that spent it, from local midnight.
	Main, Advisor, Subagent float64
	// Advisors is every advisor transcript under the session, over its
	// whole life.
	Advisors []Advisor
}

// Advisor is one advisor transcript.
type Advisor struct {
	Path    string
	Cost    float64
	Lookups int
	Notes   int
}

// lookupTools are the calls an advisor makes to look something up rather than
// to say something. advise is the only call that says something.
const lookupTools = "'read', 'grep', 'glob'"

// Query reads one session's usage. transcript is the main session file; its
// advisors and subagents are the files under the directory of the same name.
//
// The children are matched with a range rather than LIKE: session paths are
// full of underscores, which LIKE reads as a wildcard, and a range on
// session_file is what the index can answer. Every path under "dir/" sorts
// after "dir/" and before "dir0", because '0' is the byte after '/'.
func Query(ctx context.Context, db *sql.DB, transcript string, dayStart time.Time) (Usage, error) {
	dir := strings.TrimSuffix(transcript, ".jsonl")
	lo, hi := dir+"/", dir+"0"
	var usage Usage

	rows, err := db.QueryContext(ctx, `
		SELECT agent_type, SUM(cost_total)
		FROM messages
		WHERE (session_file = ? OR (session_file > ? AND session_file < ?))
		  AND timestamp >= ?
		GROUP BY agent_type`,
		transcript, lo, hi, dayStart.UnixMilli())
	if err != nil {
		return Usage{}, fmt.Errorf("today's cost: %w", err)
	}
	for rows.Next() {
		var agent string
		var cost float64
		if err := rows.Scan(&agent, &cost); err != nil {
			rows.Close()
			return Usage{}, fmt.Errorf("today's cost: %w", err)
		}
		switch agent {
		case "main":
			usage.Main = cost
		case "advisor":
			usage.Advisor = cost
		case "subagent":
			usage.Subagent = cost
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Usage{}, fmt.Errorf("today's cost: %w", err)
	}

	rows, err = db.QueryContext(ctx, `
		SELECT m.session_file, m.cost, COALESCE(t.lookups, 0), COALESCE(t.notes, 0)
		FROM (
			SELECT session_file, SUM(cost_total) AS cost
			FROM messages
			WHERE session_file > ? AND session_file < ? AND agent_type = 'advisor'
			GROUP BY session_file
		) m
		LEFT JOIN (
			SELECT session_file,
			       SUM(tool_name IN (`+lookupTools+`)) AS lookups,
			       SUM(tool_name = 'advise') AS notes
			FROM tool_calls
			WHERE session_file > ? AND session_file < ? AND agent_type = 'advisor'
			GROUP BY session_file
		) t USING (session_file)
		ORDER BY m.cost DESC, m.session_file`,
		lo, hi, lo, hi)
	if err != nil {
		return Usage{}, fmt.Errorf("advisor transcripts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var a Advisor
		if err := rows.Scan(&a.Path, &a.Cost, &a.Lookups, &a.Notes); err != nil {
			return Usage{}, fmt.Errorf("advisor transcripts: %w", err)
		}
		usage.Advisors = append(usage.Advisors, a)
	}
	if err := rows.Err(); err != nil {
		return Usage{}, fmt.Errorf("advisor transcripts: %w", err)
	}
	return usage, nil
}

// Newest is the timestamp of the latest message omp has indexed, which is how
// current every number from this database is. Zero for an empty index.
func Newest(ctx context.Context, db *sql.DB) (time.Time, error) {
	var ms sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT MAX(timestamp) FROM messages`).Scan(&ms); err != nil {
		return time.Time{}, fmt.Errorf("newest message: %w", err)
	}
	if !ms.Valid {
		return time.Time{}, nil
	}
	return time.UnixMilli(ms.Int64), nil
}

// verdict is why an advisor wants a human, if it does.
type verdict struct {
	label   string
	reason  string
	advisor Advisor
}

// judge picks the advisor to report and says whether it has passed a limit.
// Cost wins over lookups when both have: an expensive advisor is the problem
// whatever it is doing, and a cheap one making too many lookups is only a
// habit. With nothing over a limit it reports the costliest advisor, so the
// numbers beside a quiet row are still the ones that would trip first.
func judge(advisors []Advisor, cfg Config) (verdict, bool) {
	for _, a := range advisors {
		if cfg.AdvisorCost > 0 && a.Cost >= cfg.AdvisorCost {
			return verdict{
				label:   LabelAdvisorCost,
				reason:  fmt.Sprintf("advisor transcript at $%.2f, limit $%.2f", a.Cost, cfg.AdvisorCost),
				advisor: a,
			}, true
		}
	}
	for _, a := range advisors {
		if cfg.LookupsPerNote > 0 && a.Lookups >= cfg.MinLookups &&
			float64(a.Lookups) >= cfg.LookupsPerNote*float64(max(a.Notes, 1)) {
			return verdict{
				label: LabelAdvisorLookups,
				reason: fmt.Sprintf("advisor made %d lookups for %d %s, limit %s per note",
					a.Lookups, a.Notes, plural("note", a.Notes), strconv.FormatFloat(cfg.LookupsPerNote, 'f', -1, 64)),
				advisor: a,
			}, true
		}
	}
	if len(advisors) == 0 {
		return verdict{label: LabelTracking}, false
	}
	return verdict{label: LabelTracking, advisor: advisors[0]}, false
}

// Normalize turns one session's usage into its item. asOf is how current the
// index was when it was read.
func Normalize(t herdr.Transcript, usage Usage, cfg Config, baseURL string, asOf time.Time) attention.Item {
	title := sessionTitle(t)
	v, flagged := judge(usage.Advisors, cfg)

	state, severity, tone, priority := attention.StateWaiting, attention.SeverityInfo, attention.ToneNeutral, attention.PriorityBackground
	if flagged {
		state, severity, tone, priority = attention.StateNeedsAttention, attention.SeverityWarning, attention.ToneAttention, attention.PriorityActionable
	}

	meta := map[string]string{
		"pane_id":         t.PaneID,
		"workspace_label": t.Workspace,
		"session_title":   title,
		"transcript":      t.Path,
		"today_main":      dollars(usage.Main),
		"today_advisor":   dollars(usage.Advisor),
		"today_subagent":  dollars(usage.Subagent),
		"today_total":     dollars(usage.Main + usage.Advisor + usage.Subagent),
	}
	if t.CWD != "" {
		meta["cwd"] = t.CWD
	}
	if !asOf.IsZero() {
		meta["as_of"] = asOf.UTC().Format(time.RFC3339)
	}
	if v.advisor.Path != "" {
		meta["advisor_transcript"] = v.advisor.Path
		meta["advisor_cost"] = dollars(v.advisor.Cost)
		meta["advisor_lookups"] = strconv.Itoa(v.advisor.Lookups)
		meta["advisor_notes"] = strconv.Itoa(v.advisor.Notes)
	}
	if v.reason != "" {
		meta["reason"] = v.reason
	}

	return attention.Item{
		ID:       attention.Key(SourceName, t.PaneID),
		Source:   SourceName,
		Title:    t.Workspace + " · " + title,
		State:    state,
		Severity: severity,
		Label:    v.label,
		Tone:     tone,
		Priority: priority,
		Context:  meta,
		Actions: []attention.Action{{
			ID:     "focus",
			Label:  "Open",
			Method: "POST",
			Href:   strings.TrimSuffix(baseURL, "/") + "/api/actions/herdr/pane/" + t.PaneID + "/focus",
		}},
	}
}

// sessionTitle drops what omp puts in front of its session title in the
// terminal title: "π", then ">" when idle or a spinner frame while working,
// "π ⠦ Debug the provider". Left in, the spinner would change the title on
// every poll.
func sessionTitle(t herdr.Transcript) string {
	title := t.Title
	if rest, ok := strings.CutPrefix(title, "π"); ok {
		title = strings.TrimLeftFunc(rest, func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsDigit(r)
		})
	}
	if title = strings.TrimSpace(title); title == "" {
		return "omp"
	}
	return title
}

func dollars(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) }

func plural(word string, n int) string {
	if n == 1 {
		return word
	}
	return word + "s"
}
