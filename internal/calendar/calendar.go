// Package calendar turns iCalendar feeds into attention items: what is on
// today, and what is about to start.
package calendar

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	ics "github.com/arran4/golang-ical"
)

// SourceName is the item source this adapter owns.
const SourceName = "calendar"

// googlePublicICS is the address Google serves for a calendar shared publicly.
// The secret address, the one for a calendar that is not public, has a
// different shape and has to be pasted in, so it arrives through URL or
// URLEnv.
const googlePublicICS = "https://calendar.google.com/calendar/ical/%s/public/basic.ics"

// FeedSpec is one calendar as the config file describes it.
type FeedSpec struct {
	Email  string
	URL    string
	URLEnv string
	Label  string
}

// Feed is a resolved calendar: a label to show and an address to fetch.
type Feed struct {
	Label string
	Email string
	URL   string
}

// ResolveFeeds validates the configured calendars and works out each address.
// getenv is injected so the secret never has to exist for a test to run.
//
// Exactly one address per feed: an explicit URL, an environment variable
// holding one, or the Google public address derived from the email. Naming two
// is a mistake in the file and fails startup.
//
// A url_env that is not exported yet is different: the file is right, the
// machine is not ready. That feed is skipped and its reason returned, so the
// daemon still serves everything else while the dashboard says a calendar is
// missing. Taking the whole daemon down over one unexported variable would
// cost you GitHub and Herdr too.
func ResolveFeeds(specs []FeedSpec, getenv func(string) string) (feeds []Feed, skipped []string, err error) {
	feeds = make([]Feed, 0, len(specs))
	for i, spec := range specs {
		where := fmt.Sprintf("calendar feed %d", i+1)
		if spec.Email != "" {
			where = "calendar " + spec.Email
		}

		if spec.URL != "" && spec.URLEnv != "" {
			return nil, nil, fmt.Errorf("%s sets both url and url_env", where)
		}

		address := spec.URL
		switch {
		case spec.URLEnv != "":
			address = strings.TrimSpace(getenv(spec.URLEnv))
			if address == "" {
				skipped = append(skipped, fmt.Sprintf("%s: $%s is not set", where, spec.URLEnv))
				continue
			}
		case address == "":
			if spec.Email == "" {
				return nil, nil, fmt.Errorf("%s needs one of email, url or url_env", where)
			}
			// Google spells the @ as %40, which PathEscape leaves alone.
			address = fmt.Sprintf(googlePublicICS,
				strings.ReplaceAll(url.PathEscape(spec.Email), "@", "%40"))
		}

		label := spec.Label
		if label == "" {
			label = spec.Email
		}
		if label == "" {
			label = "calendar"
		}

		feeds = append(feeds, Feed{Label: label, Email: spec.Email, URL: address})
	}
	return feeds, skipped, nil
}

// Client fetches and parses feeds.
type Client struct {
	http *http.Client
}

// NewClient returns a client with one timeout for every feed.
func NewClient(timeout time.Duration) *Client {
	return &Client{http: &http.Client{Timeout: timeout}}
}

// Fetch reads one feed. The URL may be a bearer credential, so it never
// appears in an error: the label does.
func (c *Client) Fetch(ctx context.Context, feed Feed) (*ics.Calendar, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, feed.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", feed.Label, err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// A transport error carries the URL, so it is replaced rather than wrapped.
		return nil, fmt.Errorf("%s: unreachable", feed.Label)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		hint := ""
		if resp.StatusCode == http.StatusNotFound && strings.Contains(feed.URL, "/public/basic.ics") {
			hint = " (the calendar is not shared publicly; use its secret address through url_env)"
		}
		return nil, fmt.Errorf("%s: %s%s", feed.Label, resp.Status, hint)
	}

	calendar, err := ics.ParseCalendar(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", feed.Label, err)
	}
	return calendar, nil
}
