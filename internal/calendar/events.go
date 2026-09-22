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

	masters, overrides := split(calendar.Events())

	for _, event := range masters {
		base, ok := describe(event, feed)
		if !ok {
			continue
		}
		replaced := overrides[base.UID]

		for _, at := range starts(event, base.Start, now, until) {
			// An instance the calendar moved or cancelled is carried by its
			// own VEVENT, further down. Emitting the rule's version too would
			// show a meeting at a time nobody is holding.
			if _, overridden := replaced[at.UTC()]; overridden {
				continue
			}
			out = append(out, base.at(at, now, until)...)
		}
	}

	for _, instances := range overrides {
		for _, event := range instances {
			if event == nil {
				// A cancelled override: it only exists to remove the rule's
				// instance, which the loop above already skipped.
				continue
			}
			base, ok := describe(event, feed)
			if !ok {
				continue
			}
			if !base.Start.Before(until) {
				continue
			}
			out = append(out, base.at(base.Start, now, until)...)
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

// split separates the recurring masters from the per-instance overrides a
// calendar writes when one occurrence moves or is cancelled. An override is a
// VEVENT sharing the master's UID and carrying RECURRENCE-ID, the original
// start of the instance it replaces. A cancelled one is recorded as nil: it
// still has to suppress the rule's instance.
func split(events []*ics.VEvent) (masters []*ics.VEvent, overrides map[string]map[time.Time]*ics.VEvent) {
	overrides = map[string]map[time.Time]*ics.VEvent{}

	for _, event := range events {
		cancelled := strings.EqualFold(text(event, ics.ComponentPropertyStatus), "CANCELLED")
		recurrenceID := event.GetProperty(ics.ComponentPropertyRecurrenceId)

		if recurrenceID == nil {
			if !cancelled {
				masters = append(masters, event)
			}
			continue
		}

		at, ok := icsTime(recurrenceID)
		if !ok {
			continue
		}
		uid := text(event, ics.ComponentPropertyUniqueId)
		if overrides[uid] == nil {
			overrides[uid] = map[time.Time]*ics.VEvent{}
		}
		if cancelled {
			overrides[uid][at.UTC()] = nil
			continue
		}
		overrides[uid][at.UTC()] = event
	}
	return masters, overrides
}

// describe reads the parts of an event that do not depend on which instance
// this is. It reports false for events with no usable start, and for all-day
// events, which are context rather than meetings: nothing starts, so there is
// nothing to be ten minutes early for.
func describe(event *ics.VEvent, feed string) (template, bool) {
	if isAllDay(event) {
		return template{}, false
	}
	start, err := event.GetStartAt()
	if err != nil {
		return template{}, false
	}
	end, err := event.GetEndAt()
	if err != nil || !end.After(start) {
		end = start.Add(time.Hour)
	}

	summary := strings.TrimSpace(text(event, ics.ComponentPropertySummary))
	if summary == "" {
		summary = "(no title)"
	}

	return template{
		Occurrence: Occurrence{
			UID:      text(event, ics.ComponentPropertyUniqueId),
			Feed:     feed,
			Summary:  summary,
			Location: strings.TrimSpace(text(event, ics.ComponentPropertyLocation)),
			URL:      strings.TrimSpace(text(event, ics.ComponentPropertyUrl)),
			Start:    start,
			End:      end,
		},
		duration: end.Sub(start),
	}, true
}

// template is one event's fixed parts plus how long it runs.
type template struct {
	Occurrence
	duration time.Duration
}

// at places the template at one start time, dropping it if it has already
// finished or has not started by the end of the window.
func (t template) at(start, now, until time.Time) []Occurrence {
	end := start.Add(t.duration)
	if !end.After(now) || !start.Before(until) {
		return nil
	}
	instance := t.Occurrence
	instance.Start = start
	instance.End = end
	return []Occurrence{instance}
}

// starts returns every start time of an event inside the window. A recurring
// event is expanded through its RRULE; anything else is its own single start.
func starts(event *ics.VEvent, start, from, until time.Time) []time.Time {
	rule := text(event, ics.ComponentPropertyRrule)
	if rule == "" {
		return []time.Time{start}
	}

	option, err := rrule.StrToROption(rule)
	if err != nil {
		// An RRULE this library cannot read would otherwise drop a recurring
		// meeting silently. Showing the first instance is the smaller lie.
		return []time.Time{start}
	}
	option.Dtstart = start

	recurrence, err := rrule.NewRRule(*option)
	if err != nil {
		return []time.Time{start}
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
// meeting is removed without an override.
func exceptions(event *ics.VEvent) map[time.Time]struct{} {
	out := map[time.Time]struct{}{}
	for i := range event.Properties {
		property := &event.Properties[i]
		if !strings.EqualFold(property.IANAToken, string(ics.ComponentPropertyExdate)) {
			continue
		}
		for _, at := range icsTimes(property) {
			out[at.UTC()] = struct{}{}
		}
	}
	return out
}

// icsTime parses a single date-time property value. A value with no Z is local
// to the property's TZID, not to UTC: reading "20260916T090000" as UTC would
// silently miss the instance it was meant to exclude by however many hours the
// calendar's offset happens to be.
func icsTime(property *ics.IANAProperty) (time.Time, bool) {
	times := icsTimes(property)
	if len(times) == 0 {
		return time.Time{}, false
	}
	return times[0], true
}

// icsTimes parses the comma separated list an EXDATE may carry.
func icsTimes(property *ics.IANAProperty) []time.Time {
	location := time.UTC
	for _, tzid := range property.ICalParameters["TZID"] {
		if loaded, err := time.LoadLocation(tzid); err == nil {
			location = loaded
			break
		}
	}

	var out []time.Time
	for _, value := range strings.Split(property.Value, ",") {
		value = strings.TrimSpace(value)
		if at, err := time.Parse("20060102T150405Z", value); err == nil {
			out = append(out, at)
			continue
		}
		if at, err := time.ParseInLocation("20060102T150405", value, location); err == nil {
			out = append(out, at)
			continue
		}
		if at, err := time.ParseInLocation("20060102", value, location); err == nil {
			out = append(out, at)
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
		state, severity, phase, tone, priority := classify(occurrence, cfg, now)

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
			Label:     phase,
			Tone:      tone,
			Priority:  priority,
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

// classify places a meeting in its phase, and ranks it. A meeting starting
// soon outranks everything else on the queue because it is the only item in
// this stack with a deadline: a pull request is still mergeable in an hour,
// and a call is not still joinable once it has finished.
func classify(occurrence Occurrence, cfg Config, now time.Time) (attention.State, attention.Severity, string, attention.Tone, int) {
	switch {
	case !occurrence.Start.After(now):
		// Already running. You are either in it or you are not, and a daemon
		// cannot tell, so it stays out of the queue.
		return attention.StateWorking, attention.SeverityInfo, phaseInProgress,
			attention.ToneActive, attention.PriorityBackground
	case occurrence.Start.Sub(now) <= cfg.Lead:
		return attention.StateNeedsAttention, attention.SeverityWarning, phaseStartingSoon,
			attention.ToneAttention, attention.PriorityDeadline
	default:
		return attention.StateWaiting, attention.SeverityInfo, phaseLater,
			attention.ToneNeutral, attention.PriorityBackground
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
