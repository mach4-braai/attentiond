package notes

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/devanmcgeer/attentiond/internal/attention"
)

// PollerConfig configures the notes adapter.
type PollerConfig struct {
	Interval time.Duration
	Dirs     []Dir
}

// Poller keeps the notes slice of the store in sync with the directories. A
// directory that cannot be read has no items, which is true: the notes in it
// are not there to be seen. The poller keeps running, so the items return when
// the directory does.
type Poller struct {
	store *attention.Store
	cfg   PollerConfig
	log   *slog.Logger

	mu     sync.Mutex
	status attention.SourceStatus
	logged map[string]bool // problem keys already logged and still present
}

// NewPoller returns a poller.
func NewPoller(store *attention.Store, cfg PollerConfig, log *slog.Logger) *Poller {
	return &Poller{
		store:  store,
		cfg:    cfg,
		log:    log,
		status: attention.SourceStatus{Mode: "directory"},
		logged: map[string]bool{},
	}
}

// Run polls until ctx is cancelled.
func (p *Poller) Run(ctx context.Context) {
	ticker := time.NewTicker(p.cfg.Interval)
	defer ticker.Stop()

	p.pollOnce()
	for {
		select {
		case <-ctx.Done():
			p.log.Info("notes poller stopped")
			return
		case <-ticker.C:
			p.pollOnce()
		}
	}
}

func (p *Poller) pollOnce() {
	items, problems := Scan(p.cfg.Dirs)
	p.store.ReplaceSource(SourceName, items)

	// Log each problem once, and again if it clears and comes back.
	current := make(map[string]bool, len(problems))
	messages := make([]string, 0, len(problems))
	p.mu.Lock()
	for _, problem := range problems {
		current[problem.Key] = true
		messages = append(messages, problem.Message)
		if !p.logged[problem.Key] {
			p.log.Warn("notes", "problem", problem.Message)
		}
	}
	p.logged = current

	first := p.status.LastSuccess == nil
	now := time.Now()
	p.status.Healthy = true
	p.status.Items = len(items)
	p.status.LastSuccess = &now
	p.status.LastError = ""
	sort.Strings(messages)
	p.status.Warning = strings.Join(messages, "; ")
	p.mu.Unlock()

	if first {
		p.log.Info("notes adapter ready", "notes", len(items), "dirs", len(p.cfg.Dirs))
	}
}

// SourceStatus reports adapter health for /health.
func (p *Poller) SourceStatus() attention.SourceStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.status
}
