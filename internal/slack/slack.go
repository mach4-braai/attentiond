// Package slack turns the Slack messages waiting on you into attention items.
// It reads with a user token through plain net/http and never writes to Slack:
// no posting, no reactions, no marking anything read.
package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SourceName is the item source this adapter owns.
const SourceName = "slack"

// DefaultBaseURL is the Slack Web API origin.
const DefaultBaseURL = "https://slack.com/api"

// DMMethod records how direct messages are found. Slack documents no search
// modifier for "messages sent to me in a DM", only in: and from:, so the
// adapter lists the DM conversations and reads their history.
const DMMethod = "conversations.list+conversations.history"

const (
	// searchCount is the page size for both searches. Slack's maximum is 100.
	searchCount = 100
	// historyLimit bounds one conversation's history read.
	historyLimit = 200
	// listPageLimit and maxListPages bound conversations.list.
	listPageLimit = 200
	maxListPages  = 10
	maxResponse   = 16 << 20
	// defaultRetryAfter is used when Slack rate limits without saying for how
	// long.
	defaultRetryAfter = time.Minute
)

// Snapshot is everything one poll learned, before any filtering. Live polls
// and fixtures both produce one, and Normalize turns it into items.
type Snapshot struct {
	// AsOf is the poll time as Unix seconds. Only a fixture sets it, so that
	// recorded timestamps stay inside the lookback whenever it is replayed.
	AsOf int64 `json:"as_of,omitempty"`
	// DMMethod names how direct messages were found.
	DMMethod string `json:"dm_method,omitempty"`
	Self     Self   `json:"self"`
	// Mentions are search results for the query <@you>.
	Mentions []Match `json:"mentions"`
	// Mine are your own recent messages, from the search from:<@you>. A
	// message of yours after a mention answers it.
	Mine []Match `json:"mine"`
	// Conversations are your DMs and group DMs with their recent history.
	Conversations []Conversation `json:"conversations"`
	// Users maps user IDs to the name to show.
	Users map[string]string `json:"users"`
}

// Self is who the token belongs to.
type Self struct {
	ID string `json:"id"`
	// URL is the workspace URL, such as https://example.slack.com/.
	URL string `json:"url"`
}

// Match is one search.messages result.
type Match struct {
	Channel   Channel `json:"channel"`
	Permalink string  `json:"permalink"`
	Text      string  `json:"text"`
	TS        string  `json:"ts"`
	User      string  `json:"user"`
	Username  string  `json:"username"`
}

// Channel is the channel of a search result. For an IM, Name is the other
// user's ID.
type Channel struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	IsIM   bool   `json:"is_im"`
	IsMPIM bool   `json:"is_mpim"`
}

// Conversation is a DM or group DM and its messages inside the lookback.
type Conversation struct {
	ID       string    `json:"id"`
	IsMPIM   bool      `json:"is_mpim"`
	Messages []Message `json:"messages"`
}

// Message is one conversations.history entry.
type Message struct {
	User    string `json:"user"`
	Text    string `json:"text"`
	TS      string `json:"ts"`
	Subtype string `json:"subtype,omitempty"`
}

// ReadFixture decodes a recorded snapshot from disk.
func ReadFixture(path string) (Snapshot, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Snapshot{}, err
	}
	var snap Snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return Snapshot{}, fmt.Errorf("slack fixture %s: %w", path, err)
	}
	return snap, nil
}

// RateLimitError reports an HTTP 429 or a ratelimited reply. After is how
// long Slack asked us to wait.
type RateLimitError struct {
	Method string
	After  time.Duration
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("slack %s: rate limited, retry after %s", e.Method, e.After)
}

// ResolveToken reads the user token from the environment variable the config
// names. It reports a missing one by variable name, never by value.
func ResolveToken(env string) (string, error) {
	token := strings.TrimSpace(os.Getenv(env))
	if token == "" {
		return "", fmt.Errorf("%s not set", env)
	}
	return token, nil
}

// Client talks to the Slack Web API with one user token.
type Client struct {
	base  string
	token string
	http  *http.Client

	self  Self
	names map[string]string
}

// NewClient returns a client for the API origin base. The token is sent as a
// bearer header, never logged, and scrubbed from every error this package
// returns.
func NewClient(base, token string, timeout time.Duration) *Client {
	return &Client{
		base:  strings.TrimRight(base, "/"),
		token: token,
		http:  &http.Client{Timeout: timeout},
		names: map[string]string{},
	}
}

// Snapshot reads everything Normalize needs. It calls auth.test on the first
// success only.
func (c *Client) Snapshot(ctx context.Context, lookback time.Duration, maxConversations int, now time.Time) (Snapshot, error) {
	if c.self.ID == "" {
		self, err := c.authTest(ctx)
		if err != nil {
			return Snapshot{}, err
		}
		c.self = self
	}
	snap := Snapshot{DMMethod: DMMethod, Self: c.self}
	cutoff := now.Add(-lookback)

	var err error
	if snap.Mentions, err = c.search(ctx, "<@"+c.self.ID+">"); err != nil {
		return Snapshot{}, err
	}
	if snap.Mine, err = c.search(ctx, "from:<@"+c.self.ID+">"); err != nil {
		return Snapshot{}, err
	}
	if snap.Conversations, err = c.conversations(ctx, cutoff, maxConversations); err != nil {
		return Snapshot{}, err
	}

	snap.Users, err = c.resolveNames(ctx, snap, cutoff)
	if err != nil {
		return Snapshot{}, err
	}
	return snap, nil
}

type envelope struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

// call runs one GET and decodes the reply into out.
func (c *Client) call(ctx context.Context, method string, params url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.base+"/"+method+"?"+params.Encode(), nil)
	if err != nil {
		return c.scrub(err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return c.scrub(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		return &RateLimitError{Method: method, After: retryAfter(resp.Header.Get("Retry-After"))}
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return c.scrub(err)
	}
	if resp.StatusCode != http.StatusOK {
		return c.scrub(fmt.Errorf("slack %s: %s: %s", method, resp.Status, firstLine(raw)))
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return c.scrub(fmt.Errorf("slack %s: %w", method, err))
	}
	if !env.OK {
		if env.Error == "ratelimited" {
			return &RateLimitError{Method: method, After: retryAfter(resp.Header.Get("Retry-After"))}
		}
		return c.scrub(fmt.Errorf("slack %s: %s", method, env.Error))
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return c.scrub(fmt.Errorf("slack %s: %w", method, err))
	}
	return nil
}

func retryAfter(header string) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(header))
	if err != nil || seconds <= 0 {
		return defaultRetryAfter
	}
	return time.Duration(seconds) * time.Second
}

func (c *Client) authTest(ctx context.Context) (Self, error) {
	var reply struct {
		URL    string `json:"url"`
		UserID string `json:"user_id"`
	}
	if err := c.call(ctx, "auth.test", url.Values{}, &reply); err != nil {
		return Self{}, err
	}
	if reply.UserID == "" {
		return Self{}, errors.New("slack auth.test: no user_id in the reply")
	}
	return Self{ID: reply.UserID, URL: reply.URL}, nil
}

func (c *Client) search(ctx context.Context, query string) ([]Match, error) {
	params := url.Values{}
	params.Set("query", query)
	params.Set("sort", "timestamp")
	params.Set("sort_dir", "desc")
	params.Set("count", strconv.Itoa(searchCount))

	var reply struct {
		Messages struct {
			Matches []Match `json:"matches"`
		} `json:"messages"`
	}
	if err := c.call(ctx, "search.messages", params, &reply); err != nil {
		return nil, err
	}
	return reply.Messages.Matches, nil
}

func (c *Client) conversations(ctx context.Context, cutoff time.Time, max int) ([]Conversation, error) {
	type listed struct {
		ID      string `json:"id"`
		IsMPIM  bool   `json:"is_mpim"`
		Updated int64  `json:"updated"`
	}
	var all []listed
	cursor := ""
	for range maxListPages {
		params := url.Values{}
		params.Set("types", "im,mpim")
		params.Set("exclude_archived", "true")
		params.Set("limit", strconv.Itoa(listPageLimit))
		if cursor != "" {
			params.Set("cursor", cursor)
		}
		var reply struct {
			Channels         []listed `json:"channels"`
			ResponseMetadata struct {
				NextCursor string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		if err := c.call(ctx, "conversations.list", params, &reply); err != nil {
			return nil, err
		}
		all = append(all, reply.Channels...)
		cursor = reply.ResponseMetadata.NextCursor
		if cursor == "" {
			break
		}
	}

	// Most recently active first, so the cap drops the quietest.
	sort.SliceStable(all, func(i, j int) bool { return all[i].Updated > all[j].Updated })
	if max > 0 && len(all) > max {
		all = all[:max]
	}

	oldest := strconv.FormatInt(cutoff.Unix(), 10) + ".000000"
	out := make([]Conversation, 0, len(all))
	for _, conv := range all {
		params := url.Values{}
		params.Set("channel", conv.ID)
		params.Set("oldest", oldest)
		params.Set("limit", strconv.Itoa(historyLimit))
		var reply struct {
			Messages []Message `json:"messages"`
		}
		if err := c.call(ctx, "conversations.history", params, &reply); err != nil {
			return nil, err
		}
		out = append(out, Conversation{ID: conv.ID, IsMPIM: conv.IsMPIM, Messages: reply.Messages})
	}
	return out, nil
}

// resolveNames looks up the display name of everyone whose message could
// become an item. A lookup that fails other than by rate limit is not fatal:
// the item falls back to the search result's handle or the user ID.
func (c *Client) resolveNames(ctx context.Context, snap Snapshot, cutoff time.Time) (map[string]string, error) {
	wanted := map[string]bool{}
	for _, m := range snap.Mentions {
		if m.User != "" && m.User != snap.Self.ID && tsAfter(m.TS, cutoff) {
			wanted[m.User] = true
		}
	}
	for _, conv := range snap.Conversations {
		for _, m := range conv.Messages {
			if m.User != "" && m.User != snap.Self.ID {
				wanted[m.User] = true
			}
		}
	}

	users := map[string]string{}
	for id := range wanted {
		name, ok := c.names[id]
		if !ok {
			var err error
			name, err = c.userName(ctx, id)
			var limited *RateLimitError
			if errors.As(err, &limited) {
				return nil, err
			}
			if err != nil {
				continue
			}
			c.names[id] = name
		}
		users[id] = name
	}
	return users, nil
}

func (c *Client) userName(ctx context.Context, id string) (string, error) {
	var reply struct {
		User struct {
			Name     string `json:"name"`
			RealName string `json:"real_name"`
			Profile  struct {
				DisplayName string `json:"display_name"`
			} `json:"profile"`
		} `json:"user"`
	}
	if err := c.call(ctx, "users.info", url.Values{"user": {id}}, &reply); err != nil {
		return "", err
	}
	for _, name := range []string{reply.User.Profile.DisplayName, reply.User.RealName, reply.User.Name} {
		if name != "" {
			return name, nil
		}
	}
	return id, nil
}

// scrub removes the token from an error. Nothing here puts it in a message,
// but a proxy that echoes headers into a body would, and an error ends up in
// logs and in /health.
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
