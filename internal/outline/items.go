package outline

import (
	"net/url"
	"strings"
	"time"

	"github.com/devanmcgeer/attentiond/internal/attention"
)

// Label is the word every mention carries. Mourningstar reads it, so it is
// part of the contract rather than a display choice.
const Label = "mentioned you"

// Normalize turns notifications into items. Notifications that are not unread
// mentions are dropped, a repeated id is reported once, and at most max items
// are returned. base is the instance origin that document paths hang off.
func Normalize(notes []Notification, base string, max int, now time.Time) []attention.Item {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	items := make([]attention.Item, 0, len(notes))
	seen := make(map[string]bool, len(notes))
	for _, n := range notes {
		if len(items) >= max {
			break
		}
		if n.ID == "" || seen[n.ID] || !IsMention(n) {
			continue
		}
		seen[n.ID] = true
		items = append(items, newItem(n, base, now))
	}
	return items
}

func newItem(n Notification, base string, now time.Time) attention.Item {
	actor := "Someone"
	if n.Actor != nil && strings.TrimSpace(n.Actor.Name) != "" {
		actor = strings.TrimSpace(n.Actor.Name)
	}
	title := "a document"
	if n.Document != nil {
		title = "Untitled"
		if strings.TrimSpace(n.Document.Title) != "" {
			title = strings.TrimSpace(n.Document.Title)
		}
	}

	meta := map[string]string{
		"actor": actor,
		"event": n.Event,
	}
	if n.Document != nil {
		putIfSet(meta, "document", title)
		putIfSet(meta, "url", documentURL(base, n.Document.URL, n.CommentID))
	}

	updated := n.CreatedAt
	if updated.IsZero() {
		updated = now
	}

	return attention.Item{
		ID:        attention.Key(SourceName, n.ID),
		Source:    SourceName,
		Title:     actor + " mentioned you in " + title,
		State:     attention.StateNeedsAttention,
		Severity:  attention.SeverityInfo,
		Label:     Label,
		Tone:      attention.ToneAttention,
		Priority:  attention.DefaultPriority(attention.StateNeedsAttention),
		Context:   meta,
		UpdatedAt: updated,
	}
}

// documentURL makes the document path absolute and, for a comment mention,
// adds the commentId parameter Outline uses to scroll to the comment.
func documentURL(base, path, commentID string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	link := path
	if !strings.HasPrefix(path, "http://") && !strings.HasPrefix(path, "https://") {
		link = base + "/" + strings.TrimLeft(path, "/")
	}
	if commentID != "" {
		sep := "?"
		if strings.Contains(link, "?") {
			sep = "&"
		}
		link += sep + "commentId=" + url.QueryEscape(commentID)
	}
	return link
}

func putIfSet(meta map[string]string, key, value string) {
	if value != "" {
		meta[key] = value
	}
}
