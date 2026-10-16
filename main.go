// Command ghome serves a navigation page whose content lives in git.
//
// A push updates the page; nothing needs rebuilding or redeploying. The git
// access token stays on the server, so the repository can stay private.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/vrocwang/ghome/internal/icons"
	"github.com/vrocwang/ghome/internal/nav"
	"github.com/vrocwang/ghome/internal/server"
	"github.com/vrocwang/ghome/internal/source"
	"github.com/vrocwang/ghome/internal/status"
	"github.com/vrocwang/ghome/internal/store"
	"github.com/vrocwang/ghome/internal/watcher"
)

// version is stamped at build time:
//
//	go build -ldflags "-X main.version=$(git describe --tags)"
var version = "dev"

type options struct {
	listen        string
	sourceKind    string
	file          string
	forgeURL      string
	forgeOwner    string
	forgeRepo     string
	forgeRef      string
	forgePath     string
	forgeToken    string
	poll          time.Duration
	webhookSecret string
	adminToken    string
	basePath      string
	cacheFile     string
	webDir        string
	iconDir       string
	iconFetch     string
	logLevel      string
	dumpWeb       string
	healthcheck   bool
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

	if o.healthcheck {
		return runHealthcheck(o)
	}

	// Extracting the UI needs no config, no git host and no listener.
	if o.dumpWeb != "" {
		if err := server.DumpWeb(o.dumpWeb); err != nil {
			return err
		}
		fmt.Printf("embedded UI written to %s\n", o.dumpWeb)
		return nil
	}

	log, err := newLogger(o.logLevel)
	if err != nil {
		return err
	}

	src, err := buildSource(o)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st := store.New()
	loadCache(o.cacheFile, st, log)

	// Parsed before anything is wired up, so a typo fails at startup with the
	// accepted values spelled out instead of being read as "on" forever.
	fetchIcons, err := parseIconFetch(o.iconFetch)
	if err != nil {
		return err
	}

	reportIconDir(o.iconDir, fetchIcons, log)
	iconCache := icons.New(icons.Options{Dir: o.iconDir, NoFetch: !fetchIcons, Logger: log})
	probes := newProber(ctx, st, log)

	// Every piece of derived state is kept in step from this one function, so
	// there is no way for the icon allow-list or the probe targets to drift away
	// from the config that is actually being served.
	// Unlocking a private entry means proving you can read the config
	// repository, so there is no separate allow list: the permission model is
	// the one already maintained in the forge.
	//
	// File mode has no forge to ask. That leaves the tier locked for everyone
	// rather than open to everyone, which is the safe direction, but it is
	// worth saying out loud because the symptom is silence.
	var verifyPrivate func(context.Context, string) source.Access
	if g, ok := src.(*source.Gitea); ok {
		verifyPrivate = g.CheckRead
	}

	var warnedPrivate atomic.Bool

	applyConfig := func(cfg *nav.Config) {
		if verifyPrivate == nil && cfg.PrivateEntries() > 0 && warnedPrivate.CompareAndSwap(false, true) {
			log.Warn("nav.yaml marks entries private but this source cannot verify a viewer token, " +
				"so they will never be served; private entries need --source=gitea or forgejo")
		}
		// Icons are warmed and allowed from the public view only. A host that
		// only a private entry uses is never served to anyone -- the browser
		// gets a letter avatar because an <img> cannot carry the viewer token --
		// so fetching its icon would be a request with no possible reader.
		//
		// IconURLs, not PageURLs: one URL per host is what the icon endpoint can
		// ever serve, so warming more than that fetches icons nobody can reach.
		iconURLs := cfg.Public().IconURLs()
		iconCache.Allow(iconURLs)
		iconCache.Warm(ctx, iconURLs)
		// Probing is different: a private link is still a service worth
		// watching for the person who can see it.
		probes.apply(cfg)
	}

	watch := watcher.New(src, st, o.poll, log)
	watch.OnUpdate = func(cfg *nav.Config) {
		// Derived state first. The watcher publishes before it calls this hook,
		// so a subscriber that reacts immediately can already be asking for the
		// icon of a host only the new config mentions -- and until the icon
		// allow-list is replaced that answer is a 404, which the browser latches
		// as a letter avatar for the life of the revision. The cache write is
		// disk I/O, so it goes last; it reads the payload that was just
		// published rather than the outgoing one.
		applyConfig(cfg)
		css, _, _ := st.CSS()
		saveCache(o.cacheFile, st.Raw(), css, log)
	}

	// A config restored from the cache never passed through OnUpdate, so its
	// derived state has to be built here as well. Without this a restart while
	// the forge is unreachable serves the cached page with every icon degraded
	// to a letter avatar and every status light stuck on "checking" -- in exactly
	// the situation the cache exists for.
	if payload, ok := st.Get(); ok {
		applyConfig(&payload.Config)
	}

	srv, err := server.New(server.Options{
		Store:         st,
		Trigger:       watch.Kick,
		Source:        src.Describe(),
		Icons:         iconAdapter{iconCache},
		Status:        probes,
		Verify:        verifyPrivate,
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
	//
	// ReadTimeout covers the whole request, body included, which
	// ReadHeaderTimeout does not and IdleTimeout only does between requests.
	// Without it an unauthenticated caller can send a complete header block with
	// a large Content-Length, then one byte, and the webhook handler sits in the
	// body read forever with the signature check never reached. It does not
	// affect /api/events: that request has no body, so the read finishes at once
	// and only the (unbounded) write side stays open.
	httpSrv := &http.Server{
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- httpSrv.Serve(listener) }()

	log.Info("ghome is up",
		"url", displayURL(listener.Addr().String(), o.basePath),
		"version", version,
		"source", src.Describe(),
		"poll", pollLabel(o.poll),
		"webhook", enabledLabel(o.webhookSecret != ""),
		"manual_refresh", enabledLabel(o.adminToken != ""),
		"web_dir", orDefault(o.webDir, "embedded"),
		"icon_dir", orDefault(o.iconDir, "none"),
		"icon_fetch", fetchLabel(fetchIcons),
	)
	switch {
	case o.poll <= 0 && o.webhookSecret == "" && o.adminToken == "":
		// Every way of asking for a refresh is switched off, so the page is a
		// snapshot: it loads once and never changes again. That is nearly always
		// a mistake rather than an intent, so say it plainly.
		log.Warn("polling is off and neither --webhook-secret nor --admin-token is set: " +
			"the config loads once and can never update")
	case o.poll <= 0:
		log.Info("polling is off: updates come from the webhook and manual refresh only")
	case o.webhookSecret == "" && isForge(o.sourceKind):
		log.Warn("no --webhook-secret set: updates rely on polling alone. " +
			"See the README to wire up a webhook for instant updates.")
	}
	if isForge(o.sourceKind) && o.forgeToken == "" {
		log.Warn("no --forge-token set: this only works for a public repository")
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
	// Event streams never end by themselves and Shutdown does not cancel
	// in-flight request contexts, so they have to be asked to finish or every
	// shutdown waits out the whole grace period.
	srv.CloseEventStreams()
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

func (a iconAdapter) Stats() int { return a.r.Stats() }

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
		p.current.Close()
	}

	ctx, cancel := context.WithCancel(p.parent)
	probe := status.New(opts)
	p.current, p.cancel, p.applied, p.started = probe, cancel, opts, true

	// Targets first, then start. Run begins with an immediate round, so
	// starting it first would let that round probe nothing at all -- and an
	// empty result set is not a change, so nothing would be broadcast and the
	// status lights would stay unknown until the next interval.
	probe.SetTargets(targets)
	go probe.Run(ctx, func() { p.store.Notify() })
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
func (p *prober) Snapshot(allow map[string]struct{}) any {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.current == nil {
		return map[string]status.Result{}
	}
	return p.current.Snapshot(allow)
}

// StatsFor implements server.StatusSource. /healthz is unauthenticated, so the
// counts have to honour the same visibility rule as /api/status.
func (p *prober) StatsFor(allow map[string]struct{}) (up, down int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.current == nil {
		return 0, 0
	}
	return p.current.StatsFor(allow)
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
		"where nav.yaml lives: file, gitea or forgejo")
	fs.StringVar(&o.file, "file", env("GHOME_FILE", "nav.yaml"),
		"path to nav.yaml when --source=file")
	fs.StringVar(&o.forgeURL, "forge-url", env("GHOME_FORGE_URL", ""),
		"forge base URL, for example https://git.example.com (sub-paths are fine)")
	fs.StringVar(&o.forgeOwner, "forge-owner", env("GHOME_FORGE_OWNER", ""),
		"repository owner or organisation")
	fs.StringVar(&o.forgeRepo, "forge-repo", env("GHOME_FORGE_REPO", ""),
		"repository name")
	fs.StringVar(&o.forgeRef, "forge-ref", env("GHOME_FORGE_REF", "main"),
		"branch, tag or commit to read")
	fs.StringVar(&o.forgePath, "forge-path", env("GHOME_FORGE_PATH", "nav.yaml"),
		"file path inside the repository")
	fs.StringVar(&o.forgeToken, "forge-token", env("GHOME_FORGE_TOKEN", ""),
		"access token with read scope, needed for a private repository")
	fs.DurationVar(&o.poll, "poll", envDuration("GHOME_POLL", 60*time.Second),
		"how often to re-check the config (the safety net behind webhooks); "+
			"0 disables polling, leaving webhook and manual refresh")
	fs.StringVar(&o.webhookSecret, "webhook-secret", env("GHOME_WEBHOOK_SECRET", ""),
		"HMAC secret for POST /api/webhook; empty disables the endpoint")
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
	fs.StringVar(&o.iconFetch, "icon-fetch", env("GHOME_ICON_FETCH", "auto"),
		"auto fetches <host>/favicon.ico when a host has no local file; off (or none) "+
			"never fetches, so every link without one falls back to a generated letter avatar")
	fs.StringVar(&o.logLevel, "log-level", env("GHOME_LOG_LEVEL", "info"),
		"log level: debug, info, warn or error")
	fs.BoolVar(&o.healthcheck, "healthcheck", false,
		"check a running instance's /healthz and exit; for container health checks")
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

  # Private Gitea or Forgejo repository, instant updates via webhook,
  # polling as a net
  ghome --source=forgejo \
        --forge-url=https://git.example.com \
        --forge-owner=your-name --forge-repo=homepage --forge-ref=main \
        --forge-token=$GHOME_FORGE_TOKEN \
        --webhook-secret=$GHOME_WEBHOOK_SECRET

  # Restyle from git, no rebuild: edit the style: block in nav.yaml, or point
  # style.customCss at a stylesheet in the same repository.
  ghome --source=forgejo ... --web-dir=/srv/ghome/web

  # To change the markup or the base stylesheet instead, start from a copy:
  ghome --dump-web=/srv/ghome/web   # then edit and pass --web-dir

Every setting can also be given through the matching GHOME_* environment
variable, which keeps tokens out of the process list. The three actions above
(--dump-web, --healthcheck, --version) have no variable: they run and exit
rather than configure a running server.
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
//
// The warning names the actual consequence, which depends on fetch: with
// fetching off an unusable directory means no icons at all, rather than a
// fallback to the network.
func reportIconDir(dir string, fetch bool, log *slog.Logger) {
	if dir == "" {
		return
	}
	fallback := "every icon will be generated from the link name"
	if fetch {
		fallback = "every icon will be fetched over the network"
	}
	count, err := icons.LocalIcons(dir)
	switch {
	case err != nil:
		log.Warn("--icon-dir is unusable, so local icons are disabled and "+fallback,
			"dir", dir, "error", err)
	case count == 0:
		log.Warn("--icon-dir contains no usable icon files, so it has no effect; expected names like <host>.svg or <host>.png",
			"dir", dir)
	default:
		log.Info("local icon directory ready", "dir", dir, "icons", count)
	}
}

// parseIconFetch reads --icon-fetch. Only the two documented values are
// accepted: anything else is far more likely to be a typo than an intent, and
// silently treating it as "auto" is how a box ends up making the outbound
// requests it was configured not to make.
func parseIconFetch(v string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "auto", "":
		return true, nil
	case "off", "none":
		return false, nil
	default:
		return false, fmt.Errorf("unknown --icon-fetch %q: expected auto or off", v)
	}
}

func buildSource(o options) (source.Source, error) {
	switch strings.ToLower(strings.TrimSpace(o.sourceKind)) {
	case "file":
		return source.NewFile(o.file)
	case "gitea", "forgejo":
		// Both speak the same API; the kind only changes how the source names
		// itself in logs and /healthz.
		kind := "gitea"
		if strings.EqualFold(strings.TrimSpace(o.sourceKind), "forgejo") {
			kind = "forgejo"
		}
		return source.NewGitea(source.GiteaOptions{
			BaseURL: o.forgeURL,
			Owner:   o.forgeOwner,
			Repo:    o.forgeRepo,
			Ref:     o.forgeRef,
			Path:    o.forgePath,
			Token:   o.forgeToken,
			Kind:    kind,
		})
	default:
		return nil, fmt.Errorf("unknown --source %q: expected file, gitea or forgejo", o.sourceKind)
	}
}

// newLogger rejects an unknown level instead of quietly logging at info: every
// other enum-valued flag (--source, --icon-fetch) fails at startup for the same
// reason, and a typo that silently changes how much you can see is the kind of
// thing you only notice while debugging something else.
func newLogger(level string) (*slog.Logger, error) {
	var lvl slog.Level
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		lvl = slog.LevelDebug
	case "info":
		lvl = slog.LevelInfo
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		return nil, fmt.Errorf("unknown --log-level %q: expected debug, info, warn or error", level)
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})), nil
}

// loadCache seeds the store from the last good config so a restart while git is
// unreachable still serves a usable page.
//
// The repository stylesheet is cached next to the config and restored with it.
// Caching only nav.yaml meant that exactly the outage the cache exists for also
// dropped the styling: the page came back unstyled, and the theme revision gave
// the browser no reason to fetch anything.
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
	css, cssRev := loadCachedCSS(path)
	var loadErr error
	if css == nil {
		// No stylesheet was cached. Passing nothing rather than an empty string
		// is what keeps "this config has no customCss" distinct from "the file
		// is empty" in the store.
		loadErr = st.Load(data, "cache:"+filepath.Base(path), "cache")
	} else {
		loadErr = st.Load(data, "cache:"+filepath.Base(path), "cache", string(css), cssRev)
	}
	if loadErr != nil {
		log.Warn("ignoring an unusable cache file", "path", path, "error", loadErr)
		return
	}
	log.Info("serving the cached config until the first fetch succeeds",
		"path", path, "cached_css", len(css) > 0)
}

// loadCachedCSS reads the stylesheet cached alongside the config. A missing file
// is normal: most configs have no customCss at all.
func loadCachedCSS(cachePath string) ([]byte, string) {
	cssPath := cachePath + ".css"
	data, err := os.ReadFile(cssPath)
	if err != nil || len(data) == 0 {
		return nil, ""
	}
	return data, "cache:" + filepath.Base(cssPath)
}

// saveCache writes atomically so a crash mid-write cannot leave a truncated
// file behind. The config and the stylesheet are written in the same step
// because they are served as a pair: a cache holding one without the other is
// what produced an unstyled page after a restart.
func saveCache(path string, data, css []byte, log *slog.Logger) {
	if path == "" || len(data) == 0 {
		return
	}
	writeAtomic(path, data, log)
	if len(css) > 0 {
		writeAtomic(path+".css", css, log)
		return
	}
	// The stylesheet is gone from the repository. Removing the cached copy is
	// what keeps a restart from resurrecting it.
	_ = os.Remove(path + ".css")
}

func writeAtomic(path string, data []byte, log *slog.Logger) {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		log.Warn("could not write the cache file", "path", path, "error", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		log.Warn("could not replace the cache file", "path", path, "error", err)
	}
}

// runHealthcheck is what a container health check calls.
//
// A scratch image has no shell and no curl, so the binary has to be able to
// check itself. It exits 0 only on HTTP 200: a 503 means the process is up but
// still has no usable config, which is exactly the state a restart should fix
// and which a naive "is the port open" probe would call healthy.
func runHealthcheck(o options) error {
	// displayURL already maps a wildcard listen address onto loopback, which is
	// where the check has to go from inside the container.
	target := displayURL(o.listen, o.basePath) + "healthz"

	client := &http.Client{Timeout: 5 * time.Second}
	res, err := client.Get(target)
	if err != nil {
		return fmt.Errorf("health check %s: %w", target, err)
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 256))
		return fmt.Errorf("health check %s: HTTP %d: %s",
			target, res.StatusCode, bytes.TrimSpace(body))
	}
	return nil
}

// pollLabel describes the re-check cadence, including the fact that it can be
// switched off. A bare "0s" in the startup line reads like a misconfiguration.
func pollLabel(d time.Duration) string {
	if d = watcher.EffectiveInterval(d); d <= 0 {
		return "off"
	}
	return d.Round(time.Second).String()
}

// isForge reports whether the source kind reads through the Gitea/Forgejo API.
func isForge(kind string) bool {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "gitea", "forgejo":
		return true
	}
	return false
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

// fetchLabel is the icon equivalent of enabledLabel: "auto" is the default and
// means outbound fetches are allowed, so a startup line reading "off" is the
// one worth noticing.
func fetchLabel(on bool) string {
	if on {
		return "auto"
	}
	return "off"
}

func orDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
