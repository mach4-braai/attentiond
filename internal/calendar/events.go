package calendar

import (
	"sort"
	"strings"
	"time"

	ics "github.com/arran4/golang-ical"
	"github.com/teambition/rrule-go"

	"github.com/devanmcgeer/attentiond/internal/attention"
)

// Config tunes what counts as a meeting worth showing.
type Config struct {
	// Horizon is how far ahead to look. Anything later is not today's problem.
	Horizon time.Duration
	// Lead is how long before a meeting starts it moves into the attention
	// queue.
	Lead time.Duration
}

// Occurrence is one instance of one event, after recurrence expansion.
type Occurrence struct {
	UID      string
	Feed     string
	Summary  string
	Location string
	URL      string
	Start    time.Time
	End      time.Time
}

// Expand returns the occurrences of a parsed calendar that fall inside
// [now, now+horizon), including instances of recurring events.
//
// All-day events are skipped. They are context, not meetings: nothing starts,
// so there is nothing to be ten minutes early for, and a week of them would
// bury the calls that matter.
func Expand(calendar *ics.Calendar, feed string, now time.Time, horizon time.Duration) []Occurrence {
	until := now.Add(horizon)
	var out []Occurrence

	for _, event := range calendar.Events() {
		if text(event, ics.ComponentPropertyStatus) == "CANCELLED" {
			continue
		}
		if isAllDay(event) {
			continue
		}

		start, err := event.GetStartAt()
		if err != nil {
			continue
		}
		end, err := event.GetEndAt()
		if err != nil || !end.After(start) {
			end = start.Add(time.Hour)
		}
		duration := end.Sub(start)

		occurrence := Occurrence{
			UID:      text(event, ics.ComponentPropertyUniqueId),
			Feed:     feed,
			Summary:  strings.TrimSpace(text(event, ics.ComponentPropertySummary)),
			Location: strings.TrimSpace(text(event, ics.ComponentPropertyLocation)),
			URL:      strings.TrimSpace(text(event, ics.ComponentPropertyUrl)),
		}
		if occurrence.Summary == "" {
			occurrence.Summary = "(no title)"
		}

		for _, at := range starts(event, start, now, until) {
			if at.Add(duration).Before(now) || at.Add(duration).Equal(now) {
				continue
			}
			instance := occurrence
			instance.Start = at
			instance.End = at.Add(duration)
			out = append(out, instance)
		}
	}

	sort.Slice(out, func(a, b int) bool {
		if !out[a].Start.Equal(out[b].Start) {
			return out[a].Start.Before(out[b].Start)
		}
		return out[a].UID < out[b].UID
	})
	return out
}

// starts returns every start time of an event inside the window. A recurring
// event is expanded through its RRULE; anything else is its own single start.
func starts(event *ics.VEvent, start, from, until time.Time) []time.Time {
	rule := text(event, ics.ComponentPropertyRrule)
	if rule == "" {
		if start.Before(until) {
			return []time.Time{start}
		}
		return nil
	}

	option, err := rrule.StrToROption(rule)
	if err != nil {
		// An RRULE this library cannot read would otherwise drop a recurring
		// meeting silently. Showing the first instance is the smaller lie.
		if start.Before(until) {
			return []time.Time{start}
		}
		return nil
	}
	option.Dtstart = start

	recurrence, err := rrule.NewRRule(*option)
	if err != nil {
		if start.Before(until) {
			return []time.Time{start}
		}
		return nil
	}

	excluded := exceptions(event)
	// Look back a day so a meeting already in progress is still reported.
	times := recurrence.Between(from.Add(-24*time.Hour), until, true)
	out := make([]time.Time, 0, len(times))
	for _, at := range times {
		if _, skip := excluded[at.UTC()]; skip {
			continue
		}
		out = append(out, at)
	}
	return out
}

// exceptions reads EXDATE, which is how a single instance of a recurring
// meeting gets cancelled.
func exceptions(event *ics.VEvent) map[time.Time]struct{} {
	out := map[time.Time]struct{}{}
	for _, property := range event.Properties {
		if !strings.EqualFold(property.IANAToken, string(ics.ComponentPropertyExdate)) {
			continue
		}
		for _, value := range strings.Split(property.Value, ",") {
			for _, layout := range []string{"20060102T150405Z", "20060102T150405", "20060102"} {
				if at, err := time.Parse(layout, strings.TrimSpace(value)); err == nil {
					out[at.UTC()] = struct{}{}
					break
				}
			}
		}
	}
	return out
}

// Normalize turns occurrences into attention items.
//
// The id carries the start time first so that items sharing a severity sort
// chronologically: the store breaks ties on id, and "the next meeting" is the
// only useful order for a day.
func Normalize(occurrences []Occurrence, cfg Config, now time.Time) []attention.Item {
	items := make([]attention.Item, 0, len(occurrences))

	for _, occurrence := range occurrences {
		state, severity, phase := classify(occurrence, cfg, now)

		meta := map[string]string{
			"calendar":  occurrence.Feed,
			"starts_at": occurrence.Start.Format(time.RFC3339),
			"ends_at":   occurrence.End.Format(time.RFC3339),
			"phase":     phase,
			// The daemon runs on the machine the human is looking at, so it is
			// the only part of this stack that knows what their clock says. A
			// browser template would render the UTC hour instead.
			"starts_local": occurrence.Start.Local().Format("15:04"),
		}
		putIfSet(meta, "location", occurrence.Location)
		putIfSet(meta, "uid", occurrence.UID)

		var actions []attention.Action
		if link := joinURL(occurrence); link != "" {
			meta["url"] = link
			actions = append(actions, attention.Action{
				ID:     "join",
				Label:  "Join",
				Method: "GET",
				Href:   link,
			})
		}

		items = append(items, attention.Item{
			ID:        attention.Key(SourceName, occurrence.Start.UTC().Format(time.RFC3339)+"/"+occurrence.UID),
			Source:    SourceName,
			Title:     occurrence.Summary,
			State:     state,
			Severity:  severity,
			Context:   meta,
			UpdatedAt: now,
			Actions:   actions,
		})
	}

	return items
}

// The phases a meeting moves through, reported in context["phase"].
const (
	phaseStartingSoon = "starting soon"
	phaseInProgress   = "in progress"
	phaseLater        = "later"
)

func classify(occurrence Occurrence, cfg Config, now time.Time) (attention.State, attention.Severity, string) {
	switch {
	case !occurrence.Start.After(now):
		// Already running. You are either in it or you are not, and a daemon
		// cannot tell, so it stays out of the queue.
		return attention.StateWorking, attention.SeverityInfo, phaseInProgress
	case occurrence.Start.Sub(now) <= cfg.Lead:
		return attention.StateNeedsAttention, attention.SeverityWarning, phaseStartingSoon
	default:
		return attention.StateWaiting, attention.SeverityInfo, phaseLater
	}
}

// joinURL prefers an explicit URL, then a location that is one, which is where
// Google puts a Meet link.
func joinURL(occurrence Occurrence) string {
	if strings.HasPrefix(occurrence.URL, "http") {
		return occurrence.URL
	}
	if strings.HasPrefix(occurrence.Location, "http") {
		return strings.Fields(occurrence.Location)[0]
	}
	return ""
}

func isAllDay(event *ics.VEvent) bool {
	property := event.GetProperty(ics.ComponentPropertyDtStart)
	if property == nil {
		return false
	}
	for _, value := range property.ICalParameters["VALUE"] {
		if strings.EqualFold(value, "DATE") {
			return true
		}
	}
	return len(property.Value) == 8
}

func text(event *ics.VEvent, name ics.ComponentProperty) string {
	if property := event.GetProperty(name); property != nil {
		return property.Value
	}
	return ""
}

func putIfSet(target map[string]string, key, value string) {
	if value != "" {
		target[key] = value
	}
}
