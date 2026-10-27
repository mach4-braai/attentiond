package outline

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/devanmcgeer/attentiond/internal/attention"
)

// PollerConfig configures the Outline adapter.
type PollerConfig struct {
	Interval time.Duration
	BaseURL  string
	MaxItems int
	// Fixture, when set, replaces the API with a recorded response read on
	// every poll, so the source needs no key.
	Fixture string
}

// Poller keeps the Outline slice of the store in sync. Like Datadog it holds
// the last good result through a failure: Outline being unreachable says
// nothing about whether you read the mention.
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

// Run polls until ctx is cancelled.
func (p *Poller) Run(ctx context.Context) {
	ticker := time.NewTicker(p.cfg.Interval)
	defer ticker.Stop()

	p.pollOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			p.log.Info("outline poller stopped")
			return
		case <-ticker.C:
			p.pollOnce(ctx)
		}
	}
}

func (p *Poller) pollOnce(ctx context.Context) {
	var (
		notes []Notification
		err   error
	)
	if p.cfg.Fixture != "" {
		notes, err = ReadFixture(p.cfg.Fixture)
	} else {
		notes, err = p.client.Mentions(ctx, p.cfg.MaxItems)
	}
	if err != nil {
		p.recordFailure(err)
		return
	}
	items := Normalize(notes, p.cfg.BaseURL, p.cfg.MaxItems, time.Now())
	p.store.ReplaceSource(SourceName, items)
	p.recordSuccess(len(items))
}

// SourceStatus reports adapter health for /health.
func (p *Poller) SourceStatus() attention.SourceStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.status
}

func (p *Poller) recordSuccess(items int) {
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
		p.log.Info("outline adapter ready", "mentions", items, "mode", mode)
	case recovered:
		p.log.Info("outline adapter recovered", "mentions", items)
	}
}

func (p *Poller) recordFailure(err error) {
	p.mu.Lock()
	firstFailure := p.status.LastError != err.Error()
	p.status.Healthy = false
	p.status.LastError = err.Error()
	p.mu.Unlock()

	if !firstFailure {
		return
	}
	if errors.Is(err, ErrNotSupported) {
		p.log.Warn("outline adapter failed", "error", err)
		return
	}
	p.log.Warn("outline adapter failed, keeping the last result", "error", err)
}
