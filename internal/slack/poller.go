package slack

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/devanmcgeer/attentiond/internal/attention"
)

// PollerConfig configures the Slack adapter.
type PollerConfig struct {
	Interval         time.Duration
	Lookback         time.Duration
	MaxConversations int
	// Fixture, when set, replaces the API with a recorded snapshot read on
	// every poll, so the source needs no token.
	Fixture string
}

// Poller keeps the Slack slice of the store in sync. Like Datadog it holds
// the last good result through a failure: Slack being unreachable or rate
// limiting us says nothing about whether you answered.
type Poller struct {
	client *Client
	store  *attention.Store
	cfg    PollerConfig
	log    *slog.Logger

	mu     sync.Mutex
	status attention.SourceStatus
}

// NewPoller returns a poller. A nil client is valid only in fixture mode.
func NewPoller(client *Client, store *attention.Store, cfg PollerConfig, log *slog.Logger) *Poller {
	mode := "api"
	if cfg.Fixture != "" {
		mode = "fixture"
	}
	return &Poller{
		client: client,
		store:  store,
		cfg:    cfg,
		log:    log,
		status: attention.SourceStatus{Mode: mode},
	}
}

// Run polls until ctx is cancelled. After a rate limit the next poll waits
// for whichever is longer, the interval or Slack's Retry-After.
func (p *Poller) Run(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			p.log.Info("slack poller stopped")
			return
		case <-timer.C:
			timer.Reset(p.pollOnce(ctx))
		}
	}
}

// pollOnce polls and returns how long to wait before the next poll.
func (p *Poller) pollOnce(ctx context.Context) time.Duration {
	var (
		snap Snapshot
		err  error
	)
	now := time.Now()
	if p.cfg.Fixture != "" {
		snap, err = ReadFixture(p.cfg.Fixture)
	} else {
		snap, err = p.client.Snapshot(ctx, p.cfg.Lookback, p.cfg.MaxConversations, now)
	}
	if err != nil {
		p.recordFailure(err)
		var limited *RateLimitError
		if errors.As(err, &limited) && limited.After > p.cfg.Interval {
			return limited.After
		}
		return p.cfg.Interval
	}
	items := Normalize(snap, now, p.cfg.Lookback)
	p.store.ReplaceSource(SourceName, items)
	p.recordSuccess(len(items), snap.DMMethod)
	return p.cfg.Interval
}

// SourceStatus reports adapter health for /health.
func (p *Poller) SourceStatus() attention.SourceStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.status
}

func (p *Poller) recordSuccess(items int, dmMethod string) {
	p.mu.Lock()
	recovered := !p.status.Healthy && p.status.LastError != ""
	first := p.status.LastSuccess == nil
	now := time.Now()
	p.status.Healthy = true
	p.status.Items = items
	p.status.LastSuccess = &now
	p.status.LastError = ""
	mode := p.status.Mode
	p.mu.Unlock()

	switch {
	case first:
		p.log.Info("slack adapter ready", "items", items, "mode", mode, "dm_method", dmMethod)
	case recovered:
		p.log.Info("slack adapter recovered", "items", items)
	}
}

func (p *Poller) recordFailure(err error) {
	p.mu.Lock()
	firstFailure := p.status.LastError != err.Error()
	p.status.Healthy = false
	p.status.LastError = err.Error()
	p.mu.Unlock()

	if firstFailure {
		p.log.Warn("slack adapter failed, keeping the last result", "error", err)
	}
}
