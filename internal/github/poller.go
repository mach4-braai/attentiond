package github

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/devanmcgeer/attentiond/internal/attention"
)

// PollerConfig configures the GitHub adapter.
type PollerConfig struct {
	Interval time.Duration
	// Limit caps each of the two searches.
	Limit  int
	Normal Config
}

// Poller keeps the GitHub slice of the store in sync. Unlike the Herdr adapter
// it holds the last good result through a failure: Herdr being unreachable
// means those panes are gone, but GitHub being unreachable says nothing about
// whether the pull requests still want you.
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
		status: attention.SourceStatus{Mode: "graphql"},
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
			p.log.Info("github poller stopped")
			return
		case <-ticker.C:
			p.pollOnce(ctx)
		}
	}
}

func (p *Poller) pollOnce(ctx context.Context) {
	inbox, err := p.client.Inbox(ctx, p.cfg.Limit)
	if err != nil {
		p.recordFailure(err)
		return
	}
	items := Normalize(inbox, p.cfg.Normal, time.Now())
	p.store.ReplaceSource(SourceName, items)
	p.recordSuccess(len(items), inbox.Login)
}

// SourceStatus reports adapter health for /health.
func (p *Poller) SourceStatus() attention.SourceStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.status
}

func (p *Poller) recordSuccess(items int, login string) {
	p.mu.Lock()
	recovered := !p.status.Healthy && p.status.LastError != ""
	first := p.status.LastSuccess == nil
	now := time.Now()
	p.status.Healthy = true
	p.status.Items = items
	p.status.LastSuccess = &now
	p.status.LastError = ""
	p.mu.Unlock()

	switch {
	case first:
		p.log.Info("github adapter ready", "login", login, "pull_requests", items)
	case recovered:
		p.log.Info("github adapter recovered", "pull_requests", items)
	}
}

func (p *Poller) recordFailure(err error) {
	p.mu.Lock()
	firstFailure := p.status.LastError != err.Error()
	p.status.Healthy = false
	p.status.LastError = err.Error()
	p.mu.Unlock()

	if firstFailure {
		p.log.Warn("github adapter failed, keeping the last result", "error", err)
	}
}
