package calendar

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/devanmcgeer/attentiond/internal/attention"
)

// PollerConfig configures the calendar adapter.
type PollerConfig struct {
	Interval time.Duration
	Feeds    []Feed
	// Skipped holds feeds the config named but could not resolve, such as a
	// url_env that is not exported. They ride along as a standing warning so a
	// calendar cannot go missing quietly.
	Skipped []string
	Normal  Config
}

// Poller keeps the calendar slice of the store in sync. One unreachable feed
// does not blank the others: its events stay out and the failure is named in
// /health, because a calendar that half loads is still worth more than none.
type Poller struct {
	client *Client
	store  *attention.Store
	cfg    PollerConfig
	log    *slog.Logger

	mu     sync.Mutex
	status attention.SourceStatus
}

// NewPoller returns a poller.
func NewPoller(client *Client, store *attention.Store, cfg PollerConfig, log *slog.Logger) *Poller {
	return &Poller{
		client: client,
		store:  store,
		cfg:    cfg,
		log:    log,
		status: attention.SourceStatus{Mode: "ics"},
	}
}

// Run polls until ctx is cancelled.
func (p *Poller) Run(ctx context.Context) {
	ticker := time.NewTicker(p.cfg.Interval)
	defer ticker.Stop()

	p.pollOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			p.log.Info("calendar poller stopped")
			return
		case <-ticker.C:
			p.pollOnce(ctx)
		}
	}
}

func (p *Poller) pollOnce(ctx context.Context) {
	now := time.Now()
	var (
		occurrences []Occurrence
		failures    []string
	)

	for _, feed := range p.cfg.Feeds {
		calendar, err := p.client.Fetch(ctx, feed)
		if err != nil {
			failures = append(failures, err.Error())
			continue
		}
		occurrences = append(occurrences, Expand(calendar, feed.Label, now, p.cfg.Normal.Horizon)...)
	}

	// Every feed failing is a failure; some failing is a warning over real
	// data. Replacing the source with nothing in the second case would hide
	// the meetings that did load.
	if len(failures) == len(p.cfg.Feeds) && len(p.cfg.Feeds) > 0 {
		p.recordFailure(strings.Join(failures, "; "))
		return
	}

	sort.Slice(occurrences, func(a, b int) bool {
		return occurrences[a].Start.Before(occurrences[b].Start)
	})
	items := Normalize(occurrences, p.cfg.Normal, now)
	p.store.ReplaceSource(SourceName, items)
	p.recordSuccess(len(items), failures)
}

// SourceStatus reports adapter health for /health.
func (p *Poller) SourceStatus() attention.SourceStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.status
}

func (p *Poller) recordSuccess(items int, failures []string) {
	problems := append(append([]string{}, p.cfg.Skipped...), failures...)
	warning := ""
	if len(problems) > 0 {
		warning = "some calendars did not load: " + strings.Join(problems, "; ")
	}

	p.mu.Lock()
	first := p.status.LastSuccess == nil
	recovered := !p.status.Healthy && p.status.LastError != ""
	newWarning := warning != "" && p.status.Warning != warning
	now := time.Now()
	p.status.Healthy = true
	p.status.Items = items
	p.status.LastSuccess = &now
	p.status.LastError = ""
	p.status.Warning = warning
	p.mu.Unlock()

	switch {
	case first:
		p.log.Info("calendar adapter ready", "feeds", len(p.cfg.Feeds), "meetings", items)
	case recovered:
		p.log.Info("calendar adapter recovered", "meetings", items)
	}
	if newWarning {
		p.log.Warn("calendar partially loaded", "warning", warning)
	}
}

func (p *Poller) recordFailure(message string) {
	p.mu.Lock()
	firstFailure := p.status.LastError != message
	p.status.Healthy = false
	p.status.LastError = message
	p.status.Warning = ""
	p.mu.Unlock()

	p.store.ReplaceSource(SourceName, nil)
	if firstFailure {
		p.log.Warn("calendar adapter failed", "error", message)
	}
}
