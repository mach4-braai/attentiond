package herdr

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/devanmcgeer/attentiond/internal/attention"
)

// PollerConfig configures the Herdr adapter.
type PollerConfig struct {
	// Fixture, when set, replaces the socket with a recorded session snapshot
	// so the daemon is usable without a running Herdr.
	Fixture string
	// BaseURL prefixes the action URLs handed to consumers.
	BaseURL  string
	Interval time.Duration
}

// Poller keeps the Herdr slice of the store in sync with a running Herdr
// server. It replaces the whole source on every tick, so panes that disappear
// stop being reported without any separate bookkeeping.
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
	mode := "socket"
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
			p.log.Info("herdr poller stopped")
			return
		case <-ticker.C:
			p.pollOnce(ctx)
		}
	}
}

func (p *Poller) pollOnce(ctx context.Context) {
	snapshot, err := p.fetch(ctx)
	if err != nil {
		p.recordFailure(err)
		return
	}
	items := Normalize(snapshot, p.cfg.BaseURL, time.Now())
	p.store.ReplaceSource(SourceName, items)
	p.recordSuccess(len(items), snapshot.Version)
}

func (p *Poller) fetch(ctx context.Context) (SessionSnapshot, error) {
	if p.cfg.Fixture != "" {
		raw, err := os.ReadFile(p.cfg.Fixture)
		if err != nil {
			return SessionSnapshot{}, err
		}
		return DecodeFixture(raw)
	}
	return p.client.Snapshot(ctx)
}

// SourceStatus reports adapter health for /health.
func (p *Poller) SourceStatus() attention.SourceStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.status
}

func (p *Poller) recordSuccess(items int, version string) {
	p.mu.Lock()
	recovered := !p.status.Healthy && p.status.LastError != ""
	now := time.Now()
	p.status.Healthy = true
	p.status.Items = items
	p.status.LastSuccess = &now
	p.status.LastError = ""
	p.mu.Unlock()

	if recovered {
		p.log.Info("herdr adapter recovered", "items", items, "herdr_version", version)
	}
}

func (p *Poller) recordFailure(err error) {
	p.mu.Lock()
	// Herdr not running is the normal state between sessions, so only the
	// transition into failure is logged. Repeating it every tick would bury
	// everything else.
	firstFailure := p.status.LastError != err.Error()
	p.status.Healthy = false
	p.status.Items = 0
	p.status.LastError = err.Error()
	p.mu.Unlock()

	p.store.ReplaceSource(SourceName, nil)
	if firstFailure {
		p.log.Warn("herdr adapter failed", "error", err)
	}
}
