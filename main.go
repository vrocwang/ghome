// Command ghome serves a navigation page whose content lives in git.
//
// A push updates the page; nothing needs rebuilding or redeploying. The git
// access token stays on the server, so the repository can stay private.
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
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"git.xhupo.com/simon/ghome/internal/icons"
	"git.xhupo.com/simon/ghome/internal/nav"
	"git.xhupo.com/simon/ghome/internal/server"
	"git.xhupo.com/simon/ghome/internal/source"
	"git.xhupo.com/simon/ghome/internal/status"
	"git.xhupo.com/simon/ghome/internal/store"
	"git.xhupo.com/simon/ghome/internal/watcher"
)

// version is stamped at build time:
//
//	go build -ldflags "-X main.version=$(git describe --tags)"
var version = "dev"

type options struct {
	listen        string
	sourceKind    string
	file          string
	giteaURL      string
	giteaOwner    string
	giteaRepo     string
	giteaRef      string
	giteaPath     string
	giteaToken    string
	poll          time.Duration
	webhookSecret string
	adminToken    string
	branch        string
	basePath      string
	cacheFile     string
	webDir        string
	iconDir       string
	logLevel      string
	dumpWeb       string
	showVersion   bool
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "ghome: "+err.Error())
		os.Exit(1)
	}
}

func run() error {
	o := parseFlags()
	if o.showVersion {
		fmt.Println("ghome " + version)
		return nil
	}

	// Extracting the UI needs no config, no git host and no listener.
	if o.dumpWeb != "" {
		if err := server.DumpWeb(o.dumpWeb); err != nil {
			return err
		}
		fmt.Printf("embedded UI written to %s\n", o.dumpWeb)
		return nil
	}

	log := newLogger(o.logLevel)

	src, err := buildSource(o)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st := store.New()
	loadCache(o.cacheFile, st, log)

	reportIconDir(o.iconDir, log)
	iconCache := icons.New(icons.Options{Dir: o.iconDir, Logger: log})
	probes := newProber(ctx, st, log)

	watch := watcher.New(src, st, o.poll, log)
	watch.OnUpdate = func(cfg *nav.Config) {
		saveCache(o.cacheFile, st.Raw(), log)
		// Every piece of derived state is kept in step from this one hook, so
		// there is no way for the icon allow-list or the probe targets to drift
		// away from the config that is actually being served.
		iconCache.Allow(cfg.PageURLs())
		iconCache.Warm(ctx, cfg.PageURLs())
		probes.apply(cfg)
	}

	branch := o.branch
	if branch == "" {
		// Only filter webhook refs when we are actually reading a branch or
		// tag. Pinned to a commit SHA, a push to any branch could change what
		// we read, so filtering would silently drop every notification.
		if g, ok := src.(*source.Gitea); ok && !looksLikeCommit(g.Ref()) {
			branch = g.Ref()
		}
	}

	srv, err := server.New(server.Options{
		Store:         st,
		Trigger:       watch.Kick,
		Icons:         iconAdapter{iconCache},
		Status:        probes,
		Branch:        branch,
		WebhookSecret: o.webhookSecret,
		AdminToken:    o.adminToken,
		BasePath:      o.basePath,
		WebDir:        o.webDir,
		Version:       version,
		Started:       time.Now(),
		Logger:        log,
	})
	if err != nil {
		return err
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		watch.Run(ctx)
	}()

	listener, err := net.Listen("tcp", o.listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", o.listen, err)
	}

	// WriteTimeout stays unset on purpose: /api/events is a long lived stream
	// and a write deadline would kill it mid-flight.
	httpSrv := &http.Server{
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- httpSrv.Serve(listener) }()

	log.Info("ghome is up",
		"url", displayURL(listener.Addr().String(), o.basePath),
		"version", version,
		"source", src.Describe(),
		"poll", o.poll.Round(time.Second).String(),
		"webhook", enabledLabel(o.webhookSecret != ""),
		"manual_refresh", enabledLabel(o.adminToken != ""),
		"web_dir", orDefault(o.webDir, "embedded"),
		"icon_dir", orDefault(o.iconDir, "none"),
	)
	if o.webhookSecret == "" && o.sourceKind == "gitea" {
		log.Warn("no --webhook-secret set: updates rely on polling alone. " +
			"See the README to wire up a Gitea webhook for instant updates.")
	}
	if o.sourceKind == "gitea" && o.giteaToken == "" {
		log.Warn("no --gitea-token set: this only works for a public repository")
	}

	var result error
	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			result = err
		}
	case <-ctx.Done():
		log.Info("shutdown signal received, draining connections")
	}

	stop()
	probes.stop()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Warn("graceful shutdown timed out", "error", err)
	}
	wg.Wait()
	return result
}

// iconAdapter bridges the icon resolver's result type to the narrower shape the
// HTTP layer asks for, which keeps the server package free of the dependency.
type iconAdapter struct{ r *icons.Resolver }

func (a iconAdapter) Lookup(ctx context.Context, pageURL string) ([]byte, string, bool) {
	icon, ok := a.r.Lookup(ctx, pageURL)
	return icon.Data, icon.ContentType, ok
}

func (a iconAdapter) Stats() (int, uint64, uint64) { return a.r.Stats() }

// prober supervises the service health prober across config reloads.
//
// The probe cadence belongs in nav.yaml, so a config change can require a whole
// new prober. Rebuilding on every reload would throw away perfectly good
// results, so it happens only when the cadence settings actually differ. The
// supervisor itself is what the HTTP layer holds, which keeps that interface
// stable while the thing behind it is swapped.
type prober struct {
	parent context.Context
	store  *store.Store
	log    *slog.Logger

	mu      sync.Mutex
	current *status.Prober
	cancel  context.CancelFunc
	applied status.Options
	started bool
}

func newProber(parent context.Context, st *store.Store, log *slog.Logger) *prober {
	return &prober{parent: parent, store: st, log: log}
}

func (p *prober) apply(cfg *nav.Config) {
	enabled := cfg.Status.Enabled != nil && *cfg.Status.Enabled

	var targets []string
	if enabled {
		targets = cfg.ProbeTargets()
	}

	opts := status.Options{
		Interval:    cfg.Status.Interval.Std(),
		Timeout:     cfg.Status.Timeout.Std(),
		Concurrency: cfg.Status.Concurrency,
		Logger:      p.log,
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// Disabled probing keeps the existing prober but with nothing to probe, so
	// the feature costs no traffic at all until it is switched on.
	if p.started && sameProbeOptions(p.applied, opts) {
		p.current.SetTargets(targets)
		return
	}
	if p.cancel != nil {
		p.cancel()
	}

	ctx, cancel := context.WithCancel(p.parent)
	probe := status.New(opts)
	p.current, p.cancel, p.applied, p.started = probe, cancel, opts, true

	go probe.Run(ctx, func() { p.store.Notify() })
	probe.SetTargets(targets)
}

func (p *prober) stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancel != nil {
		p.cancel()
		p.cancel = nil
	}
}

// Snapshot implements server.StatusSource.
func (p *prober) Snapshot() any {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.current == nil {
		return map[string]status.Result{}
	}
	return p.current.Snapshot()
}

// Stats implements server.StatusSource.
func (p *prober) Stats() (targets, up, down int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.current == nil {
		return 0, 0, 0
	}
	return p.current.Stats()
}

func sameProbeOptions(a, b status.Options) bool {
	return a.Interval == b.Interval &&
		a.Timeout == b.Timeout &&
		a.Concurrency == b.Concurrency
}

func parseFlags() options {
	var o options

	fs := flag.NewFlagSet("ghome", flag.ExitOnError)
	fs.StringVar(&o.listen, "listen", env("GHOME_LISTEN", ":8080"),
		"address to serve on")
	fs.StringVar(&o.sourceKind, "source", env("GHOME_SOURCE", "file"),
		"where nav.yaml lives: file or gitea")
	fs.StringVar(&o.file, "file", env("GHOME_FILE", "nav.yaml"),
		"path to nav.yaml when --source=file")
	fs.StringVar(&o.giteaURL, "gitea-url", env("GHOME_GITEA_URL", ""),
		"Gitea base URL, for example https://git.example.com (sub-paths are fine)")
	fs.StringVar(&o.giteaOwner, "gitea-owner", env("GHOME_GITEA_OWNER", ""),
		"repository owner or organisation")
	fs.StringVar(&o.giteaRepo, "gitea-repo", env("GHOME_GITEA_REPO", ""),
		"repository name")
	fs.StringVar(&o.giteaRef, "gitea-ref", env("GHOME_GITEA_REF", "main"),
		"branch, tag or commit to read")
	fs.StringVar(&o.giteaPath, "gitea-path", env("GHOME_GITEA_PATH", "nav.yaml"),
		"file path inside the repository")
	fs.StringVar(&o.giteaToken, "gitea-token", env("GHOME_GITEA_TOKEN", ""),
		"access token with read scope, needed for a private repository")
	fs.DurationVar(&o.poll, "poll", envDuration("GHOME_POLL", 60*time.Second),
		"how often to re-check the config (the safety net behind webhooks)")
	fs.StringVar(&o.webhookSecret, "webhook-secret", env("GHOME_WEBHOOK_SECRET", ""),
		"HMAC secret for POST /api/webhook; empty disables the endpoint")
	fs.StringVar(&o.branch, "branch", env("GHOME_BRANCH", ""),
		"branch whose pushes trigger a reload; defaults to --gitea-ref")
	fs.StringVar(&o.adminToken, "admin-token", env("GHOME_ADMIN_TOKEN", ""),
		"bearer token for POST /api/refresh; empty disables the endpoint")
	fs.StringVar(&o.basePath, "base-path", env("GHOME_BASE_PATH", ""),
		"serve under a path prefix, for example /nav")
	fs.StringVar(&o.cacheFile, "cache-file", env("GHOME_CACHE_FILE", ""),
		"persist the last good config here so restarts survive a git outage")
	fs.StringVar(&o.webDir, "web-dir", env("GHOME_WEB_DIR", ""),
		"serve the UI from this directory instead of the embedded copy; missing files fall back to embedded")
	fs.StringVar(&o.iconDir, "icon-dir", env("GHOME_ICON_DIR", ""),
		"directory of local icon files named <host>.png/.svg/.ico, used before fetching a site")
	fs.StringVar(&o.logLevel, "log-level", env("GHOME_LOG_LEVEL", "info"),
		"log level: debug, info, warn or error")
	fs.StringVar(&o.dumpWeb, "dump-web", "",
		"write the embedded UI to this directory and exit (a starting point for --web-dir)")
	fs.BoolVar(&o.showVersion, "version", false, "print the version and exit")

	fs.Usage = func() {
		out := fs.Output()
		fmt.Fprintf(out, "ghome %s — a git driven navigation page.\n\n", version)
		fmt.Fprintf(out, "Usage:\n  ghome [flags]\n\nFlags:\n")
		fs.PrintDefaults()
		fmt.Fprintf(out, `
Examples:
  # Local file, ideal for trying it out
  ghome --source=file --file=./nav.yaml

  # Private Gitea repository, instant updates via webhook, polling as a net
  ghome --source=gitea \
        --gitea-url=https://git.example.com \
        --gitea-owner=simon --gitea-repo=nav --gitea-ref=main \
        --gitea-token=$GHOME_GITEA_TOKEN \
        --webhook-secret=$GHOME_WEBHOOK_SECRET

  # Restyle from git, no rebuild: edit the style: block in nav.yaml, or point
  # style.customCss at a stylesheet in the same repository.
  ghome --source=gitea ... --web-dir=/srv/ghome/web

  # To change the markup or the base stylesheet instead, start from a copy:
  ghome --dump-web=/srv/ghome/web   # then edit and pass --web-dir

Every flag can also be set through the matching GHOME_* environment variable,
which keeps tokens out of the process list.
`)
	}
	// ExitOnError already handles the error; parsing cannot fail here.
	_ = fs.Parse(os.Args[1:])
	return o
}

// reportIconDir says out loud what --icon-dir is actually going to do.
//
// A bad value is a warning rather than a startup failure, unlike --web-dir:
// icons are decoration, and taking the navigation page down over a missing
// icon directory would be the wrong trade. But staying silent is worse still,
// because a typo here fails in a way nobody notices.
func reportIconDir(dir string, log *slog.Logger) {
	if dir == "" {
		return
	}
	count, err := icons.LocalIcons(dir)
	switch {
	case err != nil:
		log.Warn("--icon-dir is unusable, so local icons are disabled and every icon will be fetched over the network",
			"dir", dir, "error", err)
	case count == 0:
		log.Warn("--icon-dir contains no usable icon files, so it has no effect; expected names like <host>.svg or <host>.png",
			"dir", dir)
	default:
		log.Info("local icon directory ready", "dir", dir, "icons", count)
	}
}

func buildSource(o options) (source.Source, error) {
	switch strings.ToLower(strings.TrimSpace(o.sourceKind)) {
	case "file":
		return source.NewFile(o.file)
	case "gitea", "forgejo":
		return source.NewGitea(source.GiteaOptions{
			BaseURL: o.giteaURL,
			Owner:   o.giteaOwner,
			Repo:    o.giteaRepo,
			Ref:     o.giteaRef,
			Path:    o.giteaPath,
			Token:   o.giteaToken,
		})
	default:
		return nil, fmt.Errorf("unknown --source %q: expected file or gitea", o.sourceKind)
	}
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}

// loadCache seeds the store from the last good config so a restart while git is
// unreachable still serves a usable page.
func loadCache(path string, st *store.Store, log *slog.Logger) {
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Warn("could not read the cache file", "path", path, "error", err)
		}
		return
	}
	if err := st.Load(data, "cache:"+filepath.Base(path), "cache"); err != nil {
		log.Warn("ignoring an unusable cache file", "path", path, "error", err)
		return
	}
	log.Info("serving the cached config until the first fetch succeeds", "path", path)
}

// saveCache writes atomically so a crash mid-write cannot leave a truncated
// file behind.
func saveCache(path string, data []byte, log *slog.Logger) {
	if path == "" || len(data) == 0 {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		log.Warn("could not write the cache file", "path", path, "error", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		log.Warn("could not replace the cache file", "path", path, "error", err)
	}
}

// looksLikeCommit reports whether ref is a full git object name rather than a
// branch or a tag.
func looksLikeCommit(ref string) bool {
	if len(ref) != 40 && len(ref) != 64 {
		return false
	}
	for _, r := range strings.ToLower(ref) {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return true
}

func env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ghome: ignoring invalid %s=%q: %v\n", key, v, err)
		return fallback
	}
	return d
}

// displayURL turns a listen address into something worth clicking.
func displayURL(addr, basePath string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://" + addr
	}
	switch host {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}
	path := "/"
	if basePath != "" && basePath != "/" {
		path = "/" + strings.Trim(basePath, "/") + "/"
	}
	return "http://" + net.JoinHostPort(host, port) + path
}

func enabledLabel(on bool) string {
	if on {
		return "enabled"
	}
	return "disabled"
}

func orDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
