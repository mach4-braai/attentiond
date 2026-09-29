package spend

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/devanmcgeer/attentiond/internal/attention"
	"github.com/devanmcgeer/attentiond/internal/herdr"

	// Registers the "sqlite" driver. Pure Go, so the daemon still builds
	// with CGO_ENABLED=0.
	_ "modernc.org/sqlite"
)

// syncTimeout bounds the index command. A catch-up after days without one
// takes tens of seconds; an incremental one takes well under a second.
const syncTimeout = 2 * time.Minute

// PollerConfig configures the spend adapter.
type PollerConfig struct {
	Interval time.Duration
	// DB is omp's stats.db.
	DB string
	// Sync is run before every read. omp only indexes transcripts into
	// stats.db when something asks it to, so without this the numbers are
	// as old as the last `omp stats`. Empty skips it.
	Sync []string
	// Sessions lists the omp sessions open right now.
	Sessions func(context.Context) ([]herdr.Transcript, error)
	BaseURL  string
	Normal   Config
}

// Poller keeps the spend slice of the store in sync with the omp sessions
// Herdr has open. A session that closes drops off on the next poll.
type Poller struct {
	store *attention.Store
	cfg   PollerConfig
	log   *slog.Logger

	mu     sync.Mutex
	status attention.SourceStatus
}

// NewPoller returns a poller.
func NewPoller(store *attention.Store, cfg PollerConfig, log *slog.Logger) *Poller {
	return &Poller{
		store:  store,
		cfg:    cfg,
		log:    log,
		status: attention.SourceStatus{Mode: "stats.db"},
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
			p.log.Info("spend poller stopped")
			return
		case <-ticker.C:
			p.pollOnce(ctx)
		}
	}
}

func (p *Poller) pollOnce(ctx context.Context) {
	sessions, err := p.cfg.Sessions(ctx)
	if err != nil {
		p.recordFailure("list omp sessions: " + err.Error())
		return
	}
	// No omp pane open means nothing to show, and no reason to run an
	// indexer or open a 100 MB database to show it.
	if len(sessions) == 0 {
		p.store.ReplaceSource(SourceName, nil)
		p.recordSuccess(0, "")
		return
	}

	// A failed index run leaves the database as it was, which is still an
	// answer: the numbers are older, and as_of on every item says how old.
	warning := p.sync(ctx)

	items, err := p.read(ctx, sessions, time.Now())
	if err != nil {
		p.recordFailure(err.Error())
		return
	}
	p.store.ReplaceSource(SourceName, items)
	p.recordSuccess(len(items), warning)
}

// sync runs the index command and returns a warning when it fails.
func (p *Poller) sync(ctx context.Context) string {
	if len(p.cfg.Sync) == 0 {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, syncTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, p.cfg.Sync[0], p.cfg.Sync[1:]...).CombinedOutput()
	if err == nil {
		return ""
	}
	warning := fmt.Sprintf("%s failed, numbers are as of the last index: %v", strings.Join(p.cfg.Sync, " "), err)
	if last := lastLine(out); last != "" {
		warning += ": " + last
	}
	return warning
}

// read opens the database for this poll only. omp can rebuild stats.db, and a
// handle held across that would keep reading the file it replaced.
func (p *Poller) read(ctx context.Context, sessions []herdr.Transcript, now time.Time) ([]attention.Item, error) {
	dsn := (&url.URL{Scheme: "file", Path: p.cfg.DB, RawQuery: "mode=ro&_pragma=busy_timeout(5000)"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", p.cfg.DB, err)
	}
	defer db.Close()

	asOf, err := Newest(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", p.cfg.DB, err)
	}
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())

	items := make([]attention.Item, 0, len(sessions))
	for _, session := range sessions {
		usage, err := Query(ctx, db, session.Path, dayStart)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p.cfg.DB, err)
		}
		items = append(items, Normalize(session, usage, p.cfg.Normal, p.cfg.BaseURL, asOf))
	}
	return items, nil
}

// SourceStatus reports adapter health for /health.
func (p *Poller) SourceStatus() attention.SourceStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.status
}

func (p *Poller) recordSuccess(items int, warning string) {
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
		p.log.Info("spend adapter ready", "sessions", items)
	case recovered:
		p.log.Info("spend adapter recovered", "sessions", items)
	}
	if newWarning {
		p.log.Warn("spend index not refreshed", "warning", warning)
	}
}

func (p *Poller) recordFailure(message string) {
	p.mu.Lock()
	firstFailure := p.status.LastError != message
	p.status.Healthy = false
	p.status.Items = 0
	p.status.LastError = message
	p.status.Warning = ""
	p.mu.Unlock()

	p.store.ReplaceSource(SourceName, nil)
	if firstFailure {
		p.log.Warn("spend adapter failed", "error", message)
	}
}

func lastLine(out []byte) string {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
