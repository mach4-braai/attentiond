// Command attentiond runs the localhost attention daemon.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/devanmcgeer/attentiond/internal/attention"
	"github.com/devanmcgeer/attentiond/internal/github"
	"github.com/devanmcgeer/attentiond/internal/herdr"
	"github.com/devanmcgeer/attentiond/internal/httpapi"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

type options struct {
	addr          string
	publicURL     string
	herdrSocket   string
	herdrFixture  string
	herdrPoll     time.Duration
	githubEnabled bool
	githubAPI     string
	githubPoll    time.Duration
	githubStale   time.Duration
	githubLimit   int
	eventTTL      time.Duration
	logLevel      string
	logFormat     string
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "attentiond:", err)
		os.Exit(1)
	}
}

func run() error {
	opts := parseFlags()

	logger, err := newLogger(opts)
	if err != nil {
		return err
	}

	if err := requireLoopback(opts.addr); err != nil {
		return err
	}
	if opts.publicURL == "" {
		opts.publicURL = "http://" + opts.addr
	}

	started := time.Now()
	store := attention.NewStore(logger, opts.eventTTL)

	client := herdr.NewClient(opts.herdrSocket, 5*time.Second)
	poller := herdr.NewPoller(client, store, herdr.PollerConfig{
		Fixture:  opts.herdrFixture,
		BaseURL:  opts.publicURL,
		Interval: opts.herdrPoll,
	}, logger)

	sources := map[string]func() attention.SourceStatus{
		herdr.SourceName: poller.SourceStatus,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	githubPoller := newGitHubPoller(ctx, opts, store, logger)
	if githubPoller != nil {
		sources[github.SourceName] = githubPoller.SourceStatus
	}

	handler := httpapi.New(httpapi.Config{
		Store:   store,
		Sources: sources,
		Actions: map[string]httpapi.Executor{
			herdr.SourceName: herdr.NewActions(client, opts.herdrFixture != ""),
		},
		Version: version,
		Started: started,
		Log:     logger,
	})

	go poller.Run(ctx)
	if githubPoller != nil {
		go githubPoller.Run(ctx)
	}

	server := &http.Server{
		Addr:              opts.addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	listener, err := net.Listen("tcp", opts.addr)
	if err != nil {
		return err
	}

	logger.Info("attentiond started",
		"version", version,
		"addr", listener.Addr().String(),
		"public_url", opts.publicURL,
		"herdr_mode", poller.SourceStatus().Mode,
		"herdr_poll", opts.herdrPoll.String(),
		"event_ttl", opts.eventTTL.String())

	errs := make(chan error, 1)
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
		}
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}

	logger.Info("attentiond shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		return err
	}
	logger.Info("attentiond stopped", "uptime", time.Since(started).Truncate(time.Second).String())
	return nil
}

// newGitHubPoller returns nil when GitHub polling is off or no credential is
// available. A missing token is not an error: the daemon is useful without it,
// and a hard failure here would make `attentiond` unstartable on a machine that
// never logged into gh.
func newGitHubPoller(ctx context.Context, opts options, store *attention.Store, log *slog.Logger) *github.Poller {
	if !opts.githubEnabled {
		return nil
	}

	lookup, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	token, origin, err := github.ResolveToken(lookup)
	if err != nil {
		log.Warn("github adapter disabled, no credential", "error", err)
		return nil
	}
	log.Info("github adapter enabled", "credential", origin, "poll", opts.githubPoll.String())

	return github.NewPoller(
		github.NewClient(opts.githubAPI, token, 15*time.Second),
		store,
		github.PollerConfig{
			Interval: opts.githubPoll,
			Limit:    opts.githubLimit,
			Normal:   github.Config{StaleDraftAfter: opts.githubStale},
		},
		log,
	)
}

func parseFlags() options {
	var opts options
	flag.StringVar(&opts.addr, "addr", envOr("ATTENTIOND_ADDR", "127.0.0.1:7717"),
		"loopback address to listen on")
	flag.StringVar(&opts.publicURL, "public-url", os.Getenv("ATTENTIOND_PUBLIC_URL"),
		"base URL consumers reach this daemon on, used to build action links (default http://<addr>)")
	flag.StringVar(&opts.herdrSocket, "herdr-socket", "",
		"path to the Herdr control socket (default: Herdr's own resolution order)")
	flag.StringVar(&opts.herdrFixture, "herdr-fixture", os.Getenv("ATTENTIOND_HERDR_FIXTURE"),
		"read a recorded `herdr api snapshot` from this file instead of a live Herdr server")
	flag.DurationVar(&opts.herdrPoll, "herdr-poll", 2*time.Second,
		"how often to poll Herdr for a session snapshot")
	flag.BoolVar(&opts.githubEnabled, "github", true,
		"poll GitHub for pull requests you authored or were asked to review")
	flag.StringVar(&opts.githubAPI, "github-api", envOr("ATTENTIOND_GITHUB_API", github.DefaultEndpoint),
		"GraphQL endpoint, for GitHub Enterprise")
	flag.DurationVar(&opts.githubPoll, "github-poll", time.Minute,
		"how often to poll GitHub")
	flag.DurationVar(&opts.githubStale, "github-stale-draft", 14*24*time.Hour,
		"how long a draft may sit untouched before it is reported as stale")
	flag.IntVar(&opts.githubLimit, "github-limit", 30,
		"maximum pull requests per search")
	flag.DurationVar(&opts.eventTTL, "event-ttl", time.Hour,
		"how long finished or failed items from /api/events stay visible")
	flag.StringVar(&opts.logLevel, "log-level", envOr("ATTENTIOND_LOG_LEVEL", "info"),
		"debug, info, warn or error")
	flag.StringVar(&opts.logFormat, "log-format", envOr("ATTENTIOND_LOG_FORMAT", "text"),
		"text or json")
	flag.Parse()
	return opts
}

func newLogger(opts options) (*slog.Logger, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(opts.logLevel)); err != nil {
		return nil, fmt.Errorf("invalid log level %q", opts.logLevel)
	}

	handlerOpts := &slog.HandlerOptions{Level: level}
	switch opts.logFormat {
	case "json":
		return slog.New(slog.NewJSONHandler(os.Stderr, handlerOpts)), nil
	case "text":
		return slog.New(slog.NewTextHandler(os.Stderr, handlerOpts)), nil
	default:
		return nil, fmt.Errorf("invalid log format %q, expected text or json", opts.logFormat)
	}
}

// requireLoopback keeps the daemon localhost-only. Remote access, and the
// authentication it would need, is out of scope.
func requireLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid address %q: %w", addr, err)
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("address %q is not loopback; attentiond is localhost-only", addr)
	}
	return nil
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
