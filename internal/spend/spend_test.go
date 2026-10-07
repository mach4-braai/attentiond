package spend

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/devanmcgeer/attentiond/internal/attention"
	"github.com/devanmcgeer/attentiond/internal/herdr"
)

// The columns Query reads, with omp's names and types.
const schema = `
CREATE TABLE messages (
	session_file TEXT NOT NULL,
	agent_type TEXT NOT NULL,
	timestamp INTEGER NOT NULL,
	cost_total REAL NOT NULL
);
CREATE TABLE tool_calls (
	session_file TEXT NOT NULL,
	agent_type TEXT NOT NULL,
	tool_name TEXT NOT NULL,
	timestamp INTEGER NOT NULL
);`

type index struct {
	t  *testing.T
	db *sql.DB
}

func newIndex(t *testing.T) index {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "stats.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	return index{t, db}
}

func (x index) message(file, agent string, at time.Time, cost float64) {
	x.t.Helper()
	if _, err := x.db.Exec(`INSERT INTO messages VALUES (?, ?, ?, ?)`, file, agent, at.UnixMilli(), cost); err != nil {
		x.t.Fatal(err)
	}
}

func (x index) tools(file, tool string, n int, at time.Time) {
	x.t.Helper()
	for range n {
		if _, err := x.db.Exec(`INSERT INTO tool_calls VALUES (?, 'advisor', ?, ?)`, file, tool, at.UnixMilli()); err != nil {
			x.t.Fatal(err)
		}
	}
}

// A real omp session name: the underscore is a LIKE wildcard.
const session = "/s/-proj/2026-09-28T07-00-00-000Z_01a0"

var (
	dayStart  = time.Date(2026, 9, 28, 0, 0, 0, 0, time.Local)
	morning   = dayStart.Add(9 * time.Hour)
	yesterday = dayStart.Add(-2 * time.Hour)
)

func TestQuerySplitsTodayByAgentAndCountsAdvisorsOverTheirLife(t *testing.T) {
	x := newIndex(t)
	advisor := session + "/__advisor.doctrine.jsonl"

	x.message(session+".jsonl", "main", morning, 1.25)
	x.message(session+".jsonl", "main", yesterday, 40)
	x.message(advisor, "advisor", morning, 2)
	x.message(advisor, "advisor", yesterday, 30)
	x.message(session+"/Research.jsonl", "subagent", morning, 0.5)
	// A subagent's own advisor is still this session's advisor.
	x.message(session+"/Research/__advisor.jsonl", "advisor", morning, 1)
	x.tools(advisor, "grep", 3, morning)
	x.tools(advisor, "read", 2, yesterday)
	x.tools(advisor, "advise", 1, morning)
	x.tools(advisor, "recall", 4, morning)

	// Neighbours that a prefix or LIKE match would pull in.
	x.message(session+"-2.jsonl", "main", morning, 100)
	x.message(session+"-2/__advisor.jsonl", "advisor", morning, 100)
	x.message(session+"X/__advisor.jsonl", "advisor", morning, 100)
	x.message("/s/-proj/2026-09-28T07-00-00-000Za01a0/__advisor.jsonl", "advisor", morning, 100)

	got, err := Query(context.Background(), x.db, session+".jsonl", dayStart)
	if err != nil {
		t.Fatal(err)
	}
	if got.Main != 1.25 || got.Advisor != 3 || got.Subagent != 0.5 {
		t.Fatalf("today main/advisor/subagent = %v/%v/%v, want 1.25/3/0.5", got.Main, got.Advisor, got.Subagent)
	}
	want := []Advisor{
		{Path: advisor, Cost: 32, Lookups: 5, Notes: 1},
		{Path: session + "/Research/__advisor.jsonl", Cost: 1},
	}
	if len(got.Advisors) != len(want) {
		t.Fatalf("advisors = %+v, want %+v", got.Advisors, want)
	}
	for i := range want {
		if got.Advisors[i] != want[i] {
			t.Errorf("advisor %d = %+v, want %+v", i, got.Advisors[i], want[i])
		}
	}
}

func TestQueryOfASessionWithNothingIndexed(t *testing.T) {
	got, err := Query(context.Background(), newIndex(t).db, session+".jsonl", dayStart)
	if err != nil {
		t.Fatal(err)
	}
	if got.Main != 0 || got.Advisor != 0 || got.Subagent != 0 || len(got.Advisors) != 0 {
		t.Fatalf("got %+v, want nothing", got)
	}
}

func TestJudge(t *testing.T) {
	limits := Config{AdvisorCost: 25, LookupsPerNote: 20, MinLookups: 100}
	cases := []struct {
		name     string
		cfg      Config
		advisors []Advisor
		label    string
		path     string
	}{
		{"no advisor", limits, nil, LabelTracking, ""},
		{"under both limits reports the costliest", limits,
			[]Advisor{{Path: "a", Cost: 24.99, Lookups: 99, Notes: 0}, {Path: "b", Cost: 1}},
			LabelTracking, "a"},
		{"cost at the limit", limits, []Advisor{{Path: "a", Cost: 25}}, LabelAdvisorCost, "a"},
		{"lookups flag a cheap advisor", limits,
			[]Advisor{{Path: "a", Cost: 3, Lookups: 400, Notes: 1}, {Path: "b", Cost: 2, Lookups: 0}},
			LabelAdvisorLookups, "a"},
		{"cost wins over lookups on another advisor", limits,
			[]Advisor{{Path: "a", Cost: 30}, {Path: "b", Cost: 3, Lookups: 400, Notes: 1}},
			LabelAdvisorCost, "a"},
		{"ratio at the limit", limits, []Advisor{{Path: "a", Lookups: 200, Notes: 10}}, LabelAdvisorLookups, "a"},
		{"ratio just under", limits, []Advisor{{Path: "a", Lookups: 199, Notes: 10}}, LabelTracking, "a"},
		{"no notes counts as one", limits, []Advisor{{Path: "a", Lookups: 100}}, LabelAdvisorLookups, "a"},
		{"too few lookups to judge", limits, []Advisor{{Path: "a", Lookups: 99}}, LabelTracking, "a"},
		{"limits of zero are off", Config{}, []Advisor{{Path: "a", Cost: 900, Lookups: 5000}}, LabelTracking, "a"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v, flagged := judge(c.advisors, c.cfg)
			if v.label != c.label || v.advisor.Path != c.path {
				t.Fatalf("label %q advisor %q, want %q %q", v.label, v.advisor.Path, c.label, c.path)
			}
			if flagged != (c.label != LabelTracking) {
				t.Fatalf("flagged = %v with label %q", flagged, v.label)
			}
		})
	}
}

// A snooze holds until the label changes, so a cost that rises every poll must
// not move the label. Nor may it, or omp's spinner replacing ">" in the
// terminal title, move the title: that resets the item's updated time.
func TestAFlaggedItemReadsTheSameAsItsCostRises(t *testing.T) {
	idle := herdr.Transcript{PaneID: "w1:p2", Workspace: "tofu", Title: "π > Plan the move", Path: session + ".jsonl"}
	working := idle
	working.Title = "π ⠦ Plan the move"
	cfg := Config{AdvisorCost: 25}
	asOf := morning

	first := Normalize(idle, Usage{Main: 1, Advisors: []Advisor{{Path: "a", Cost: 26}}}, cfg, "http://127.0.0.1:7717/", asOf)
	later := Normalize(working, Usage{Main: 9, Advisors: []Advisor{{Path: "a", Cost: 80}}}, cfg, "http://127.0.0.1:7717/", asOf)

	if first.State != attention.StateNeedsAttention || !first.State.NeedsAttention() {
		t.Fatalf("state = %q, want needs_attention", first.State)
	}
	if first.Label != later.Label || first.Title != later.Title || first.ID != later.ID {
		t.Fatalf("item moved: %q %q %q -> %q %q %q",
			first.ID, first.Title, first.Label, later.ID, later.Title, later.Label)
	}
	if later.Context["advisor_cost"] != "80.00" || later.Context["today_total"] != "9.00" {
		t.Fatalf("context = %v", later.Context)
	}
	if first.Title != "tofu · Plan the move" {
		t.Fatalf("title = %q", first.Title)
	}
	if got := first.Actions[0].Href; got != "http://127.0.0.1:7717/api/actions/herdr/pane/w1:p2/focus" {
		t.Fatalf("focus href = %q", got)
	}
}
