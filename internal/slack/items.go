package slack

import (
	"sort"
	"strings"
	"time"

	"github.com/devanmcgeer/attentiond/internal/attention"
)

// Labels are part of the contract with mourningstar, not a display choice.
const (
	LabelMention = "mentioned you"
	LabelDM      = "messaged you"
)

// Kinds are the values of the `kind` context key.
const (
	KindMention = "mention"
	KindDM      = "dm"
)

const titleChars = 100

// contentSubtypes are the message subtypes that carry something a person
// wrote. Joins, topic changes and the like are not waiting on you.
var contentSubtypes = map[string]bool{
	"":                 true,
	"thread_broadcast": true,
	"file_share":       true,
	"me_message":       true,
}

// Normalize turns a snapshot into items. Direct messages come first so a
// message found both ways is reported as a DM. A message is dropped when it is
// from you, older than the lookback, or followed by a message of yours in the
// same conversation.
func Normalize(snap Snapshot, now time.Time, lookback time.Duration) []attention.Item {
	if snap.AsOf > 0 {
		now = time.Unix(snap.AsOf, 0)
	}
	cutoff := now.Add(-lookback)
	n := namer{users: snap.Users}

	var items []attention.Item
	seen := map[string]bool{}
	scanned := map[string]bool{}
	for _, conv := range snap.Conversations {
		scanned[conv.ID] = true
	}
	add := func(item attention.Item) {
		if !seen[item.ID] {
			seen[item.ID] = true
			items = append(items, item)
		}
	}

	for _, conv := range snap.Conversations {
		last := lastOwn(conv.Messages, snap.Self.ID)
		for _, msg := range conv.Messages {
			if msg.User == "" || msg.User == snap.Self.ID || !contentSubtypes[msg.Subtype] {
				continue
			}
			if !tsAfter(msg.TS, cutoff) || (last != "" && tsCompare(msg.TS, last) <= 0) {
				continue
			}
			add(newItem(snap, n, KindDM, conv.ID, "", msg.TS, msg.User, "", msg.Text, ""))
		}
	}

	mine := map[string]string{}
	for _, m := range snap.Mine {
		if m.User != "" && m.User != snap.Self.ID {
			continue
		}
		if tsCompare(m.TS, mine[m.Channel.ID]) > 0 {
			mine[m.Channel.ID] = m.TS
		}
	}
	for _, m := range snap.Mentions {
		if m.User == snap.Self.ID || !tsAfter(m.TS, cutoff) {
			continue
		}
		if reply, ok := mine[m.Channel.ID]; ok && tsCompare(reply, m.TS) > 0 {
			continue
		}
		kind := KindMention
		if m.Channel.IsIM || m.Channel.IsMPIM {
			kind = KindDM
		}
		if kind == KindDM && scanned[m.Channel.ID] {
			// The history read already decided this conversation.
			continue
		}
		add(newItem(snap, n, kind, m.Channel.ID, m.Channel.Name, m.TS, m.User, m.Username, m.Text, m.Permalink))
	}

	sort.SliceStable(items, func(i, j int) bool { return items[i].UpdatedAt.After(items[j].UpdatedAt) })
	return items
}

func newItem(snap Snapshot, n namer, kind, channel, channelName, ts, user, handle, text, permalink string) attention.Item {
	sender := n.name(user, handle)
	label := LabelMention
	title := sender + " in #" + firstNonEmpty(channelName, channel) + ": " + preview(text)
	if kind == KindDM {
		label = LabelDM
		title = sender + " (DM): " + preview(text)
	}
	if permalink == "" {
		permalink = buildPermalink(snap.Self.URL, channel, ts)
	}

	return attention.Item{
		ID:       attention.Key(SourceName, channel+":"+ts),
		Source:   SourceName,
		Title:    title,
		State:    attention.StateNeedsAttention,
		Severity: attention.SeverityInfo,
		Label:    label,
		Tone:     attention.ToneAttention,
		Priority: attention.DefaultPriority(attention.StateNeedsAttention),
		Context: map[string]string{
			"channel":   channel,
			"ts":        ts,
			"permalink": permalink,
			"sender":    sender,
			"kind":      kind,
		},
		UpdatedAt: tsTime(ts),
	}
}

type namer struct{ users map[string]string }

func (n namer) name(id, handle string) string {
	return firstNonEmpty(n.users[id], handle, id, "someone")
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// preview is the first titleChars characters of a message on one line.
func preview(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	runes := []rune(text)
	if len(runes) <= titleChars {
		return text
	}
	return string(runes[:titleChars]) + "…"
}

// buildPermalink makes the link Slack's own clients make: the message
// timestamp without its dot, prefixed with p.
func buildPermalink(workspaceURL, channel, ts string) string {
	if workspaceURL == "" {
		return ""
	}
	return strings.TrimRight(workspaceURL, "/") + "/archives/" + channel + "/p" + strings.Replace(ts, ".", "", 1)
}

func lastOwn(messages []Message, self string) string {
	last := ""
	for _, m := range messages {
		if m.User == self && tsCompare(m.TS, last) > 0 {
			last = m.TS
		}
	}
	return last
}

// tsMicros parses a Slack timestamp such as 1508284197.000015 into
// microseconds. Floats would lose the last digits.
func tsMicros(ts string) int64 {
	whole, frac, _ := strings.Cut(ts, ".")
	var sec, micro int64
	for _, c := range whole {
		if c < '0' || c > '9' {
			return 0
		}
		sec = sec*10 + int64(c-'0')
	}
	frac = (frac + "000000")[:6]
	for _, c := range frac {
		if c < '0' || c > '9' {
			return sec * 1_000_000
		}
		micro = micro*10 + int64(c-'0')
	}
	return sec*1_000_000 + micro
}

func tsCompare(a, b string) int {
	x, y := tsMicros(a), tsMicros(b)
	switch {
	case x < y:
		return -1
	case x > y:
		return 1
	}
	return 0
}

func tsTime(ts string) time.Time {
	return time.UnixMicro(tsMicros(ts)).UTC()
}

func tsAfter(ts string, cutoff time.Time) bool {
	micros := tsMicros(ts)
	return micros > 0 && micros >= cutoff.UnixMicro()
}
