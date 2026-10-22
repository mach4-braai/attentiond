// Package outline turns unread Outline mentions into attention items. It
// reads the notifications of the API key's owner with plain net/http and never
// writes to Outline: a notification stays unread until you read it there.
package outline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// SourceName is the item source this adapter owns.
const SourceName = "outline"

// Notification events that mean somebody mentioned you by name. The
// group_mentioned events fire when a group you belong to is mentioned; they
// are left out because the item says "mentioned you".
const (
	EventDocumentMentioned = "documents.mentioned"
	EventCommentMentioned  = "comments.mentioned"
)

const (
	notificationsPath = "/api/notifications.list"
	documentsInfoPath = "/api/documents.info"

	// pageSize is notifications per request.
	pageSize = 100
	// maxPages bounds one poll when most notifications are not mentions.
	maxPages = 20
	// maxResponse bounds one response body.
	maxResponse = 16 << 20
)

// ErrNotSupported means the server has no notifications.list endpoint. The
// API reference does not say which Outline version added it, so the message
// does not name one.
var ErrNotSupported = errors.New("outline server answered 404 for notifications.list; " +
	"it predates the notifications API, upgrade Outline")

// Client talks to one Outline instance with one API key.
type Client struct {
	base  string
	token string
	http  *http.Client
}

// NewClient returns a client for the instance at base, such as
// https://app.getoutline.com. The key is sent as a Bearer token, never logged,
// and scrubbed from every error this package returns.
func NewClient(base, token string, timeout time.Duration) *Client {
	return &Client{
		base:  strings.TrimRight(strings.TrimSpace(base), "/"),
		token: token,
		http:  &http.Client{Timeout: timeout},
	}
}

// ResolveKey reads the API key from the environment variable the config
// names. It reports a missing one by variable name, never by value.
func ResolveKey(tokenEnv string) (string, error) {
	token := strings.TrimSpace(os.Getenv(tokenEnv))
	if token == "" {
		return "", fmt.Errorf("%s not set", tokenEnv)
	}
	return token, nil
}

// Response is one page of notifications.list.
type Response struct {
	Data       []Notification `json:"data"`
	Pagination struct {
		Limit  int `json:"limit"`
		Offset int `json:"offset"`
	} `json:"pagination"`
}

// Notification is the slice of an Outline notification this adapter reads.
// Document is not in the published schema, which carries only documentId; a
// server that embeds it saves one documents.info call per notification.
type Notification struct {
	ID         string     `json:"id"`
	Event      string     `json:"event"`
	Actor      *User      `json:"actor"`
	DocumentID string     `json:"documentId"`
	CommentID  string     `json:"commentId"`
	Document   *Document  `json:"document"`
	ViewedAt   *time.Time `json:"viewedAt"`
	ArchivedAt *time.Time `json:"archivedAt"`
	CreatedAt  time.Time  `json:"createdAt"`
}

// User is the person who mentioned you.
type User struct {
	Name string `json:"name"`
}

// Document is the document a notification points at. URL is a path such as
// /doc/handbook-AbC123, relative to the instance.
type Document struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	URL   string `json:"url"`
}

// IsMention reports whether n is an unread, unarchived mention of you.
func IsMention(n Notification) bool {
	if n.Event != EventDocumentMentioned && n.Event != EventCommentMentioned {
		return false
	}
	return n.ViewedAt == nil && n.ArchivedAt == nil
}

// Decode parses one notifications.list body, from the API or a fixture.
func Decode(raw []byte) (Response, error) {
	var resp Response
	if err := json.Unmarshal(raw, &resp); err != nil {
		return Response{}, fmt.Errorf("decode outline notifications: %w", err)
	}
	return resp, nil
}

// ReadFixture decodes a recorded notifications.list response from disk.
func ReadFixture(path string) ([]Notification, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read outline fixture: %w", err)
	}
	resp, err := Decode(raw)
	if err != nil {
		return nil, err
	}
	return resp.Data, nil
}

// Mentions pages through notifications.list, newest first, and returns up to
// max unread mentions with their documents filled in. It stops at max, at a
// short page, or after maxPages.
func (c *Client) Mentions(ctx context.Context, max int) ([]Notification, error) {
	var out []Notification
	for page := 0; page < maxPages && len(out) < max; page++ {
		resp, err := c.page(ctx, page*pageSize)
		if err != nil {
			return nil, err
		}
		for _, n := range resp.Data {
			if IsMention(n) && len(out) < max {
				out = append(out, n)
			}
		}
		if len(resp.Data) < pageSize {
			break
		}
	}
	if err := c.fillDocuments(ctx, out); err != nil {
		return nil, err
	}
	return out, nil
}

// fillDocuments looks up each document once for the notifications that did
// not arrive with one. A document you can no longer open (403 or 404) leaves
// the notification without a title or link rather than failing the poll.
func (c *Client) fillDocuments(ctx context.Context, notes []Notification) error {
	cache := map[string]*Document{}
	for i := range notes {
		n := &notes[i]
		if n.Document != nil || n.DocumentID == "" {
			continue
		}
		doc, seen := cache[n.DocumentID]
		if !seen {
			var err error
			doc, err = c.document(ctx, n.DocumentID)
			if err != nil {
				return err
			}
			cache[n.DocumentID] = doc
		}
		n.Document = doc
	}
	return nil
}

func (c *Client) page(ctx context.Context, offset int) (Response, error) {
	raw, status, err := c.post(ctx, notificationsPath, map[string]any{"limit": pageSize, "offset": offset})
	if err != nil {
		return Response{}, err
	}
	if status == http.StatusNotFound {
		return Response{}, ErrNotSupported
	}
	if status != http.StatusOK {
		return Response{}, c.scrub(fmt.Errorf("outline notifications.list: %d %s: %s",
			status, http.StatusText(status), firstLine(raw)))
	}
	resp, err := Decode(raw)
	if err != nil {
		return Response{}, c.scrub(err)
	}
	return resp, nil
}

// document returns nil, nil for a document the key cannot open.
func (c *Client) document(ctx context.Context, id string) (*Document, error) {
	raw, status, err := c.post(ctx, documentsInfoPath, map[string]any{"id": id})
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusOK:
	case http.StatusForbidden, http.StatusNotFound:
		return nil, nil
	default:
		return nil, c.scrub(fmt.Errorf("outline documents.info: %d %s: %s",
			status, http.StatusText(status), firstLine(raw)))
	}
	var body struct {
		Data Document `json:"data"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, c.scrub(fmt.Errorf("decode outline document: %w", err))
	}
	return &body.Data, nil
}

func (c *Client) post(ctx context.Context, path string, payload any) ([]byte, int, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, 0, c.scrub(err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, c.scrub(err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return nil, 0, c.scrub(err)
	}
	return raw, resp.StatusCode, nil
}

// scrub removes the key from an error. Nothing here puts it in a message, but
// a proxy that echoes headers into a body would, and an error ends up in logs
// and in /health.
func (c *Client) scrub(err error) error {
	if c.token == "" {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), c.token, "[redacted]"))
}

func firstLine(raw []byte) string {
	text := strings.TrimSpace(string(raw))
	if index := strings.IndexByte(text, '\n'); index >= 0 {
		text = text[:index]
	}
	if len(text) > 200 {
		text = text[:200] + "…"
	}
	return text
}
