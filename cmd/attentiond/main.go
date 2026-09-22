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
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/devanmcgeer/attentiond/internal/attention"
	"github.com/devanmcgeer/attentiond/internal/calendar"
	"github.com/devanmcgeer/attentiond/internal/config"
	"github.com/devanmcgeer/attentiond/internal/github"
	"github.com/devanmcgeer/attentiond/internal/herdr"
	"github.com/devanmcgeer/attentiond/internal/httpapi"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "attentiond:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, configPath, err := resolveConfig()
	if err != nil {
		return err
	}

	logger, err := newLogger(cfg.Daemon)
	if err != nil {
		return err
	}
	if configPath != "" {
		logger.Info("configuration loaded", "path", configPath)
	} else {
		looked, _ := config.ResolvePath("")
		logger.Warn("no configuration file, running on defaults",
			"looked_at", looked, "github", "off until a config file turns it on")
	}

	if err := requireLoopback(cfg.Daemon.Addr); err != nil {
		return err
	}
	if cfg.Daemon.PublicURL == "" {
		cfg.Daemon.PublicURL = "http://" + cfg.Daemon.Addr
	}

	started := time.Now()
	store := attention.NewStore(logger, cfg.Events.TTL.Std())

	sources := map[string]func() attention.SourceStatus{}
	actions := map[string]httpapi.Executor{}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var herdrPoller *herdr.Poller
	if cfg.Herdr.Enabled {
		client := herdr.NewClient(cfg.Herdr.Socket, 5*time.Second)
		herdrPoller = herdr.NewPoller(client, store, herdr.PollerConfig{
			Fixture:  cfg.Herdr.Fixture,
			BaseURL:  cfg.Daemon.PublicURL,
			Interval: cfg.Herdr.Poll.Std(),
		}, logger)
		sources[herdr.SourceName] = herdrPoller.SourceStatus
		actions[herdr.SourceName] = herdr.NewActions(client, cfg.Herdr.Fixture != "")
	}

	githubPoller, err := newGitHubPoller(ctx, cfg.GitHub, store, logger)
	if err != nil {
		return err
	}
	if githubPoller != nil {
		sources[github.SourceName] = githubPoller.SourceStatus
	}

	calendarPoller, err := newCalendarPoller(cfg.Calendar, store, logger)
	if err != nil {
		return err
	}
	if calendarPoller != nil {
		sources[calendar.SourceName] = calendarPoller.SourceStatus
	}

	handler := httpapi.New(httpapi.Config{
		Store:   store,
		Sources: sources,
		Actions: actions,
		Version: version,
		Started: started,
		Log:     logger,
	})

	if herdrPoller != nil {
		go herdrPoller.Run(ctx)
	}
	if githubPoller != nil {
		go githubPoller.Run(ctx)
	}
	if calendarPoller != nil {
		go calendarPoller.Run(ctx)
	}

	server := &http.Server{
		Addr:              cfg.Daemon.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	listener, err := net.Listen("tcp", cfg.Daemon.Addr)
	if err != nil {
		return err
	}

	logger.Info("attentiond started",
		"version", version,
		"addr", listener.Addr().String(),
		"public_url", cfg.Daemon.PublicURL,
		"sources", strings.Join(sourceNames(sources), ","),
		"event_ttl", cfg.Events.TTL.String())

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
func newGitHubPoller(ctx context.Context, cfg config.GitHub, store *attention.Store, log *slog.Logger) (*github.Poller, error) {
	if !cfg.Enabled {
		return nil, nil
	}

	scope, err := github.NewScope(cfg.Repos, cfg.Orgs)
	if err != nil {
		return nil, err
	}

	lookup, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	token, origin, err := github.ResolveToken(lookup)
	if err != nil {
		log.Warn("github adapter disabled, no credential", "error", err)
		return nil, nil
	}
	log.Info("github adapter enabled",
		"credential", origin, "poll", cfg.Poll.String(), "scope", scope.String())

	return github.NewPoller(
		github.NewClient(cfg.API, token, 15*time.Second),
		store,
		github.PollerConfig{
			Interval: cfg.Poll.Std(),
			Search:   github.Search{Scope: scope, Limit: cfg.Limit},
			Normal:   github.Config{StaleDraftAfter: cfg.StaleDraftAfter.Std()},
		},
		log,
	), nil
}

// newCalendarPoller returns nil when the calendar source is off or has no
// feeds. A feed that cannot be resolved is fatal: a calendar silently missing
// from the dashboard is a meeting silently missing from your day.
func newCalendarPoller(cfg config.Calendar, store *attention.Store, log *slog.Logger) (*calendar.Poller, error) {
	if !cfg.Enabled || len(cfg.Feeds) == 0 {
		return nil, nil
	}

	specs := make([]calendar.FeedSpec, 0, len(cfg.Feeds))
	for _, feed := range cfg.Feeds {
		specs = append(specs, calendar.FeedSpec{
			Email:  feed.Email,
			URL:    feed.URL,
			URLEnv: feed.URLEnv,
			Label:  feed.Label,
		})
	}
	feeds, skipped, err := calendar.ResolveFeeds(specs, os.Getenv)
	if err != nil {
		return nil, err
	}
	for _, reason := range skipped {
		log.Warn("calendar feed skipped", "reason", reason)
	}

	labels := make([]string, 0, len(feeds))
	for _, feed := range feeds {
		labels = append(labels, feed.Label)
	}
	log.Info("calendar adapter enabled",
		"feeds", strings.Join(labels, ","),
		"poll", cfg.Poll.String(),
		"horizon", cfg.Horizon.String(),
		"lead", cfg.Lead.String())

	return calendar.NewPoller(
		calendar.NewClient(15*time.Second),
		store,
		calendar.PollerConfig{
			Interval: cfg.Poll.Std(),
			Feeds:    feeds,
			Skipped:  skipped,
			Normal:   calendar.Config{Horizon: cfg.Horizon.Std(), Lead: cfg.Lead.Std()},
		},
		log,
	), nil
}

// resolveConfig layers the command line over the file over the defaults. The
// file is the place settings live; the flags exist for the one-off run, so
// only the flags actually typed are applied.
func resolveConfig() (config.Config, string, error) {
	var (
		configPath = flag.String("config", "",
			"configuration file (default $ATTENTIOND_CONFIG, then ~/.attn/config.toml)")
		addr         = flag.String("addr", "", "loopback address to listen on")
		publicURL    = flag.String("public-url", "", "base URL used to build action links")
		logLevel     = flag.String("log-level", "", "debug, info, warn or error")
		logFormat    = flag.String("log-format", "", "text or json")
		eventTTL     = flag.Duration("event-ttl", 0, "how long finished event items stay visible")
		herdrSocket  = flag.String("herdr-socket", "", "path to the Herdr control socket")
		herdrFixture = flag.String("herdr-fixture", "",
			"read a recorded `herdr api snapshot` from this file instead of a live Herdr server")
		herdrPoll   = flag.Duration("herdr-poll", 0, "how often to poll Herdr")
		githubOff   = flag.Bool("no-github", false, "skip the GitHub source for this run")
		githubPoll  = flag.Duration("github-poll", 0, "how often to poll GitHub")
		githubRepos = flag.String("github-repos", "",
			"only watch these repositories, comma separated `owner/name`")
		githubOrgs = flag.String("github-orgs", "",
			"also watch every repository in these accounts, comma separated logins")
		githubLimit = flag.Int("github-limit", 0,
			"pull requests per search before the result is reported as incomplete")
	)
	flag.Parse()

	path, explicit := config.ResolvePath(*configPath)
	cfg, found, err := config.Load(path, explicit)
	if err != nil {
		return config.Config{}, "", err
	}
	if !found {
		path = ""
	}

	var flagErr error
	flag.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "addr":
			cfg.Daemon.Addr = *addr
		case "public-url":
			cfg.Daemon.PublicURL = *publicURL
		case "log-level":
			cfg.Daemon.LogLevel = *logLevel
		case "log-format":
			cfg.Daemon.LogFormat = *logFormat
		case "event-ttl":
			cfg.Events.TTL = config.Duration(*eventTTL)
		case "herdr-socket":
			cfg.Herdr.Socket = *herdrSocket
		case "herdr-fixture":
			cfg.Herdr.Fixture = *herdrFixture
		case "herdr-poll":
			cfg.Herdr.Poll = config.Duration(*herdrPoll)
		case "no-github":
			cfg.GitHub.Enabled = !*githubOff
		case "github-poll":
			cfg.GitHub.Poll = config.Duration(*githubPoll)
		case "github-limit":
			cfg.GitHub.Limit = *githubLimit
		case "github-repos", "github-orgs":
			scope, err := github.ParseScope(*githubRepos, *githubOrgs)
			if err != nil {
				flagErr = err
				return
			}
			cfg.GitHub.Repos = scope.Repos
			cfg.GitHub.Orgs = scope.Orgs
		}
	})
	if flagErr != nil {
		return config.Config{}, "", flagErr
	}

	return cfg, path, nil
}

// sourceNames lists the registered adapters for the startup line.
func sourceNames(sources map[string]func() attention.SourceStatus) []string {
	names := make([]string, 0, len(sources))
	for name := range sources {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func newLogger(cfg config.Daemon) (*slog.Logger, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		return nil, fmt.Errorf("invalid log level %q", cfg.LogLevel)
	}

	handlerOpts := &slog.HandlerOptions{Level: level}
	switch cfg.LogFormat {
	case "json":
		return slog.New(slog.NewJSONHandler(os.Stderr, handlerOpts)), nil
	case "text":
		return slog.New(slog.NewTextHandler(os.Stderr, handlerOpts)), nil
	default:
		return nil, fmt.Errorf("invalid log format %q, expected text or json", cfg.LogFormat)
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
