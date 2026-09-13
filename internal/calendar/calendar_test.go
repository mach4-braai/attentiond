package calendar

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	ics "github.com/arran4/golang-ical"

	"github.com/devanmcgeer/attentiond/internal/attention"
)

func fixture(t *testing.T) *ics.Calendar {
	t.Helper()
	file, err := os.Open("testdata/feed.ics")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	calendar, err := ics.ParseCalendar(file)
	if err != nil {
		t.Fatal(err)
	}
	return calendar
}

// Monday 14 September 2026, 08:00 UTC: an hour before the standup.
var monday = time.Date(2026, 9, 14, 8, 0, 0, 0, time.UTC)

func TestExpandKeepsMeetingsAndDropsTheRest(t *testing.T) {
	got := Expand(fixture(t), "work", monday, 12*time.Hour)

	var summaries []string
	for _, occurrence := range got {
		summaries = append(summaries, occurrence.Summary)
	}
	want := []string{"Platform standup", "Design sync", "Architecture review"}
	if strings.Join(summaries, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v, want %v", summaries, want)
	}
	// Dropped: an all-day event (nothing to be early for), a cancelled one,
	// and one outside the horizon.
}

func TestExpandWalksARecurrenceAndHonoursItsExceptions(t *testing.T) {
	// Monday to Friday, with Wednesday carrying an EXDATE.
	got := Expand(fixture(t), "work", monday, 5*24*time.Hour)

	var standups []time.Time
	for _, occurrence := range got {
		if occurrence.UID == "standup@didx" {
			standups = append(standups, occurrence.Start.UTC())
		}
	}
	for _, at := range standups {
		if at.Weekday() == time.Wednesday {
			t.Errorf("the EXDATE instance came back: %s", at)
		}
	}
	if len(standups) != 3 {
		t.Fatalf("got %d standups, want Monday, Tuesday and the moved Thursday: %v", len(standups), standups)
	}
}

func TestExpandFollowsAnInstanceThatMoved(t *testing.T) {
	// Thursday's standup was moved from 09:00 to 11:00, which the calendar
	// writes as a second VEVENT with the same UID and a RECURRENCE-ID. Both
	// times showing would send you to an empty room.
	got := Expand(fixture(t), "work", monday, 5*24*time.Hour)

	thursday := time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC)
	var hours []int
	for _, occurrence := range got {
		at := occurrence.Start.UTC()
		if occurrence.UID == "standup@didx" && at.YearDay() == thursday.YearDay() {
			hours = append(hours, at.Hour())
		}
	}
	if len(hours) != 1 || hours[0] != 11 {
		t.Fatalf("Thursday hours = %v, want only the moved 11:00", hours)
	}
}

func TestExpandDropsAnInstanceThatWasCancelled(t *testing.T) {
	// Friday's standup is cancelled by an override. The rule still generates
	// it, so the override has to suppress it.
	got := Expand(fixture(t), "work", monday, 5*24*time.Hour)

	for _, occurrence := range got {
		at := occurrence.Start.UTC()
		if occurrence.UID == "standup@didx" && at.Weekday() == time.Friday {
			t.Fatalf("a cancelled instance is still on the calendar: %s", at)
		}
	}
}

func TestExpandReadsAnExclusionInItsOwnTimezone(t *testing.T) {
	// EXDATE;TZID=Africa/Johannesburg:20260915T120000 is 10:00 UTC. Reading it
	// as UTC would exclude nothing and leave Tuesday's sync on the board.
	got := Expand(fixture(t), "work", monday, 5*24*time.Hour)

	var days []int
	for _, occurrence := range got {
		if occurrence.UID == "design@didx" {
			days = append(days, occurrence.Start.UTC().Day())
			if hour := occurrence.Start.UTC().Hour(); hour != 10 {
				t.Errorf("instance at %02d:00 UTC, want 10:00 for a 12:00 SAST meeting", hour)
			}
		}
	}
	for _, day := range days {
		if day == 15 {
			t.Error("the zoned EXDATE did not exclude Tuesday")
		}
	}
	if len(days) != 4 {
		t.Fatalf("got %d design syncs, want 5 daily minus the excluded one: %v", len(days), days)
	}
}

func TestExpandKeepsAMeetingThatHasAlreadyStarted(t *testing.T) {
	// Ten minutes into the standup. A meeting in progress is the one you are
	// most likely to be looking for.
	during := time.Date(2026, 9, 14, 9, 10, 0, 0, time.UTC)
	got := Expand(fixture(t), "work", during, 2*time.Hour)
	if len(got) == 0 || got[0].Summary != "Platform standup" {
		t.Fatalf("got %+v, want the standup still listed", got)
	}
}

func TestNormalizePhasesAMeetingOnTheWayIn(t *testing.T) {
	cfg := Config{Horizon: 12 * time.Hour, Lead: 10 * time.Minute}

	cases := []struct {
		name     string
		now      time.Time
		state    attention.State
		severity attention.Severity
		phase    string
	}{
		{"an hour out is just the day ahead", monday,
			attention.StateWaiting, attention.SeverityInfo, phaseLater},
		{"inside the lead it wants you", time.Date(2026, 9, 14, 8, 55, 0, 0, time.UTC),
			attention.StateNeedsAttention, attention.SeverityWarning, phaseStartingSoon},
		{"once it starts it is no longer a summons", time.Date(2026, 9, 14, 9, 5, 0, 0, time.UTC),
			attention.StateWorking, attention.SeverityInfo, phaseInProgress},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			items := Normalize(Expand(fixture(t), "work", tc.now, cfg.Horizon), cfg, tc.now)
			if len(items) == 0 {
				t.Fatal("no items")
			}
			item := items[0]
			if item.Title != "Platform standup" {
				t.Fatalf("first item = %q", item.Title)
			}
			if item.State != tc.state || item.Severity != tc.severity {
				t.Errorf("state = %s/%s, want %s/%s", item.State, item.Severity, tc.state, tc.severity)
			}
			if item.Context["phase"] != tc.phase {
				t.Errorf("phase = %q, want %q", item.Context["phase"], tc.phase)
			}
		})
	}
}

func TestNormalizeCarriesTheJoinLinkAndSortsByStart(t *testing.T) {
	cfg := Config{Horizon: 12 * time.Hour, Lead: 10 * time.Minute}
	items := Normalize(Expand(fixture(t), "work", monday, cfg.Horizon), cfg, monday)

	if len(items) != 3 {
		t.Fatalf("got %d items", len(items))
	}
	// Ids lead with the start time, and the store breaks severity ties on id,
	// so the next meeting is the one at the top.
	for i := 1; i < len(items); i++ {
		if items[i-1].ID >= items[i].ID {
			t.Errorf("ids do not sort chronologically: %q then %q", items[i-1].ID, items[i].ID)
		}
	}

	standup := items[0]
	if standup.Context["calendar"] != "work" {
		t.Errorf("calendar = %q", standup.Context["calendar"])
	}
	if standup.Context["starts_at"] != "2026-09-14T09:00:00Z" {
		t.Errorf("starts_at = %q", standup.Context["starts_at"])
	}
	if len(standup.Actions) != 1 || standup.Actions[0].Href != "https://meet.google.com/abc-defg-hij" {
		t.Errorf("actions = %+v, want the Meet link from LOCATION", standup.Actions)
	}

	review := items[2]
	if review.Title != "Architecture review" {
		t.Fatalf("last item = %q", review.Title)
	}
	if len(review.Actions) != 1 || review.Actions[0].Href != "https://meet.google.com/zzz-yyyy-xxx" {
		t.Errorf("actions = %+v, want the link from URL", review.Actions)
	}
}

func TestResolveFeedsWorksOutEachAddress(t *testing.T) {
	env := map[string]string{"CAL_SECRET": "https://example.com/private-abc/basic.ics"}
	getenv := func(key string) string { return env[key] }

	feeds, skipped, err := ResolveFeeds([]FeedSpec{
		{Email: "devan.mcgeer@didx.co.za"},
		{Email: "me@gmail.com", URLEnv: "CAL_SECRET"},
		{Email: "other@example.com", URL: "https://example.com/plain.ics", Label: "shared"},
	}, getenv)
	if err != nil || len(skipped) != 0 {
		t.Fatalf("err=%v skipped=%v", err, skipped)
	}

	if feeds[0].URL != "https://calendar.google.com/calendar/ical/devan.mcgeer%40didx.co.za/public/basic.ics" {
		t.Errorf("derived url = %q", feeds[0].URL)
	}
	if feeds[1].URL != env["CAL_SECRET"] {
		t.Errorf("url_env not used: %q", feeds[1].URL)
	}
	if feeds[2].Label != "shared" || feeds[2].URL != "https://example.com/plain.ics" {
		t.Errorf("explicit feed = %+v", feeds[2])
	}
	if feeds[0].Label != "devan.mcgeer@didx.co.za" {
		t.Errorf("label should fall back to the email, got %q", feeds[0].Label)
	}
}

func TestFetchKeepsAMalformedAddressOutOfErrorsToo(t *testing.T) {
	// A secret address with a stray control character never reaches the
	// transport: NewRequest rejects it, and url.Error quotes what it was
	// given. That is the second way a bearer URL can reach a log line.
	secret := "https://calendar.example.com/private-SECRETTOKEN/basic.ics\n"
	_, err := NewClient(time.Second).Fetch(context.Background(), Feed{Label: "work", URL: secret})
	if err == nil {
		t.Fatal("a malformed address was accepted")
	}
	if strings.Contains(err.Error(), "SECRETTOKEN") {
		t.Errorf("the secret address leaked into an error: %v", err)
	}
	if !strings.Contains(err.Error(), "work") {
		t.Errorf("error = %v, want the feed label so it can be found", err)
	}
}

func TestResolveFeedsSkipsAnUnexportedSecretWithoutTakingTheRest(t *testing.T) {
	// The file is right, the machine is not ready. Killing startup here would
	// cost GitHub and Herdr as well, for one unexported variable.
	feeds, skipped, err := ResolveFeeds([]FeedSpec{
		{Email: "work@didx.co.za", URLEnv: "NOT_SET"},
		{Email: "other@example.com", URL: "https://example.com/plain.ics"},
	}, func(string) string { return "" })
	if err != nil {
		t.Fatalf("err = %v, want the other feed to survive", err)
	}
	if len(feeds) != 1 || feeds[0].Email != "other@example.com" {
		t.Fatalf("feeds = %+v", feeds)
	}
	if len(skipped) != 1 || !strings.Contains(skipped[0], "NOT_SET") {
		t.Fatalf("skipped = %v, want the variable named so the dashboard can say so", skipped)
	}
}

func TestResolveFeedsRefusesToGuess(t *testing.T) {
	getenv := func(string) string { return "" }

	for _, tc := range []struct {
		name string
		spec FeedSpec
	}{
		{"nothing to go on", FeedSpec{}},
		{"two addresses", FeedSpec{Email: "a@b.c", URL: "https://x", URLEnv: "Y"}},
	} {
		if _, _, err := ResolveFeeds([]FeedSpec{tc.spec}, getenv); err == nil {
			t.Errorf("%s was accepted", tc.name)
		}
	}
}

func TestFetchExplainsA404OnAPublicAddress(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer server.Close()

	_, err := NewClient(5*time.Second).Fetch(context.Background(), Feed{
		Label: "work",
		URL:   server.URL + "/calendar/ical/x/public/basic.ics",
	})
	if err == nil {
		t.Fatal("a 404 was accepted")
	}
	if !strings.Contains(err.Error(), "not shared publicly") {
		t.Errorf("error = %v, want the reason a public address 404s", err)
	}
}

func TestFetchKeepsTheAddressOutOfErrors(t *testing.T) {
	// The URL can be a bearer credential. It must not end up in a log line.
	secret := "https://calendar.example.com/private-SECRETTOKEN/basic.ics"
	_, err := NewClient(100*time.Millisecond).Fetch(context.Background(), Feed{Label: "work", URL: secret})
	if err == nil {
		t.Fatal("expected a transport failure")
	}
	if strings.Contains(err.Error(), "SECRETTOKEN") {
		t.Errorf("the secret address leaked into an error: %v", err)
	}
}
