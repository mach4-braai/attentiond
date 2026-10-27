package datadog

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/devanmcgeer/attentiond/internal/attention"
)

// PollerConfig configures the Datadog adapter.
type PollerConfig struct {
	Interval  time.Duration
	Query     string
	PageLimit int
	// Fixture, when set, replaces the API with a recorded response read on
	// every poll, so the source needs no keys.
	Fixture string
}

// Poller keeps the Datadog slice of the store in sync. Like GitHub it holds
// the last good result through a failure: Datadog being unreachable says
// nothing about whether the vulnerabilities were fixed.
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
			p.log.Info("datadog poller stopped")
			return
		case <-ticker.C:
			p.pollOnce(ctx)
		}
	}
}

func (p *Poller) pollOnce(ctx context.Context) {
	var (
		result Result
		err    error
	)
	if p.cfg.Fixture != "" {
		result, err = ReadFixture(p.cfg.Fixture)
	} else {
		result, err = p.client.Findings(ctx, p.cfg.Query, p.cfg.PageLimit)
	}
	if err != nil {
		p.recordFailure(err)
		return
	}
	items := Normalize(result.Findings, time.Now())
	p.store.ReplaceSource(SourceName, items)
	p.recordSuccess(len(items), result.Partial)
}

// SourceStatus reports adapter health for /health.
func (p *Poller) SourceStatus() attention.SourceStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.status
}

func (p *Poller) recordSuccess(items int, partial bool) {
	warning := ""
	if partial {
		// The queue is now a prefix and nothing on screen would say so.
		warning = "datadog timed out searching findings; the result is incomplete"
	}

	p.mu.Lock()
	recovered := !p.status.Healthy && p.status.LastError != ""
	first := p.status.LastSuccess == nil
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
		p.log.Info("datadog adapter ready", "findings", items, "mode", p.status.Mode)
	case recovered:
		p.log.Info("datadog adapter recovered", "findings", items)
	}
	if newWarning {
		p.log.Warn("datadog result is incomplete", "warning", warning)
	}
}

func (p *Poller) recordFailure(err error) {
	p.mu.Lock()
	firstFailure := p.status.LastError != err.Error()
	p.status.Healthy = false
	p.status.LastError = err.Error()
	p.mu.Unlock()

	if firstFailure {
		p.log.Warn("datadog adapter failed, keeping the last result", "error", err)
	}
}
