// Package server exposes the navigation document over HTTP and serves the
// single page UI.
package server

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vrocwang/ghome/internal/nav"
	"github.com/vrocwang/ghome/internal/store"
)

//go:embed web
var webRoot embed.FS

// IconSource is the icon cache as the HTTP layer needs it. Declaring it here
// rather than importing the concrete resolver keeps the handler testable
// without a live cache.
type IconSource interface {
	Lookup(ctx context.Context, pageURL string) (data []byte, contentType string, ok bool)
	Stats() (entries int, hits, misses uint64)
}

// StatusSource is the health prober as the HTTP layer needs it. Snapshot
// returns a value that is marshalled straight to JSON.
type StatusSource interface {
	Snapshot() any
	Stats() (targets, up, down int)
}

// Options configures the HTTP layer.
type Options struct {
	Store *store.Store
	// Trigger asks the watcher for an immediate reload.
	Trigger func()
	// Icons resolves site icons server side. Nil disables /api/icon.
	Icons IconSource
	// Status reports probe results. Nil disables /api/status.
	Status StatusSource
	// Source describes where the config comes from, for /healthz. It is
	// reported even when nothing has loaded yet, which is precisely when an
	// operator needs to see what the process is pointed at.
	Source string
	// Branch is the ref a webhook must match, e.g. "main". Empty accepts any.
	Branch string
	// WebhookSecret enables POST /api/webhook. Empty leaves it disabled.
	WebhookSecret string
	// AdminToken enables POST /api/refresh. Empty leaves it disabled.
	AdminToken string
	// BasePath mounts everything under a prefix such as "/nav".
	BasePath string
	// WebDir serves the UI from disk instead of the embedded copy, so the
	// look can be changed without rebuilding. Missing files fall back to the
	// embedded version, so replacing one file is enough.
	WebDir  string
	Version string
	Started time.Time
	Logger  *slog.Logger
}

// Server is the ghome HTTP handler.
type Server struct {
	store  *store.Store
	opts   Options
	log    *slog.Logger
	root   http.Handler
	assets fs.FS

	// idx caches host -> page URL for /api/icon, rebuilt when the config
	// revision changes.
	idxMu  sync.Mutex
	idx    map[string]string
	idxRev string
}

// New builds the handler tree.
func New(o Options) (*Server, error) {
	if o.Store == nil {
		return nil, errors.New("server: Store is required")
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Started.IsZero() {
		o.Started = time.Now()
	}
	if o.WebDir != "" {
		info, err := os.Stat(o.WebDir)
		if err != nil {
			return nil, fmt.Errorf("server: --web-dir %s: %w", o.WebDir, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("server: --web-dir %s is not a directory", o.WebDir)
		}
		o.WebDir = filepath.Clean(o.WebDir)
	}

	s := &Server{store: o.Store, opts: o, log: o.Logger}

	embedded, err := fs.Sub(webRoot, "web")
	if err != nil {
		return nil, fmt.Errorf("server: embedded assets: %w", err)
	}
	s.assets = overlayFS{dir: o.WebDir, base: embedded}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.Handle("GET /assets/", s.cacheControl(http.FileServerFS(s.assets), "no-cache"))
	mux.HandleFunc("GET /theme.css", s.handleThemeCSS)
	mux.HandleFunc("GET /favicon.ico", s.handleFavicon)
	mux.HandleFunc("GET /api/nav", s.handleNav)
	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("GET /api/icon", s.handleIcon)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	mux.HandleFunc("POST /api/webhook", s.handleWebhook)
	mux.HandleFunc("POST /api/refresh", s.handleRefresh)
	mux.HandleFunc("GET /healthz", s.handleHealth)

	var h http.Handler = mux
	if base := normaliseBasePath(o.BasePath); base != "" {
		stripped := http.StripPrefix(base, mux)
		h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.URL.Path == base:
				// Relative asset URLs only resolve correctly with the slash.
				http.Redirect(w, r, base+"/", http.StatusMovedPermanently)
			case strings.HasPrefix(r.URL.Path, base+"/"):
				stripped.ServeHTTP(w, r)
			default:
				http.NotFound(w, r)
			}
		})
	}

	s.root = s.withLogging(s.withSecurityHeaders(h))
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.root.ServeHTTP(w, r) }

func normaliseBasePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" || p == "/" {
		return ""
	}
	return "/" + strings.Trim(p, "/")
}

// overlayFS serves from an on-disk directory when the file is there, and falls
// back to the embedded copy otherwise. It is what lets --web-dir replace a
// single file without having to mirror the whole UI.
type overlayFS struct {
	dir  string
	base fs.FS
}

// resolve confines a request path to the override directory. A traversal
// attempt is refused outright rather than being clamped onto some other file
// inside the directory: silently serving a different file than the one asked
// for is the kind of surprise that turns into a bug report.
func (o overlayFS) resolve(name string) (string, bool) {
	if o.dir == "" {
		return "", false
	}
	slashed := strings.ReplaceAll(name, "\\", "/")
	for _, part := range strings.Split(slashed, "/") {
		if part == ".." {
			return "", false
		}
	}
	full := filepath.Join(o.dir, filepath.FromSlash(path.Clean("/"+slashed)))

	// Symlinks are resolved on both sides before the comparison. Comparing the
	// unresolved path would let a link inside the override directory serve a
	// file from outside it, and would equally reject a directory that is itself
	// reached through a link.
	realDir, err := filepath.EvalSymlinks(o.dir)
	if err != nil {
		return "", false
	}
	real, err := filepath.EvalSymlinks(full)
	if err != nil {
		// Missing files land here. Returning false falls back to the embedded
		// copy, which is what an absent override should do anyway.
		return "", false
	}
	rel, err := filepath.Rel(realDir, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return real, true
}

func (o overlayFS) Open(name string) (fs.File, error) {
	if full, ok := o.resolve(name); ok {
		if f, err := os.Open(full); err == nil {
			return f, nil
		}
	}
	return o.base.Open(name)
}

func (o overlayFS) Stat(name string) (fs.FileInfo, error) {
	if full, ok := o.resolve(name); ok {
		if info, err := os.Stat(full); err == nil {
			return info, nil
		}
	}
	return fs.Stat(o.base, name)
}

func (o overlayFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if full, ok := o.resolve(name); ok {
		if entries, err := os.ReadDir(full); err == nil {
			return entries, nil
		}
	}
	return fs.ReadDir(o.base, name)
}

// readWebFile reads one UI file through the same override-then-embedded rule.
func (s *Server) readWebFile(name string) ([]byte, error) {
	if f, err := s.assets.Open(name); err == nil {
		defer func() { _ = f.Close() }()
		return io.ReadAll(f)
	}
	return nil, fmt.Errorf("asset %s is missing", name)
}

func (s *Server) withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		// Nav links send people to third party sites; do not leak our URL.
		h.Set("Referrer-Policy", "no-referrer")
		// Icons are proxied through this origin, so nothing needs to be
		// fetched from a third party by the browser any more.
		h.Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data:; "+
				"style-src 'self'; script-src 'self'; connect-src 'self'; "+
				"base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) cacheControl(next http.Handler, value string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The UI ships inside the binary, so a stale cache would survive an
		// upgrade. Everything is a few kilobytes: revalidate instead.
		w.Header().Set("Cache-Control", value)
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	data, err := s.readWebFile("index.html")
	if err != nil {
		s.log.Error("index.html is unavailable", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
}

func (s *Server) handleFavicon(w http.ResponseWriter, r *http.Request) {
	data, err := s.readWebFile("assets/favicon.svg")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
}

// navError is the shape of a failed API response.
type navError struct {
	Error  string `json:"error"`
	Detail string `json:"detail,omitempty"`
}

func (s *Server) handleNav(w http.ResponseWriter, r *http.Request) {
	payload, ok := s.store.Get()
	if !ok {
		detail := ""
		if err := s.store.LastError(); err != nil {
			detail = err.Error()
		}
		s.writeJSON(w, http.StatusServiceUnavailable, navError{
			Error:  "navigation config is not available yet",
			Detail: detail,
		})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusOK, payload)
}

// handleStatus serves the optional health probe results. When probing is
// disabled it answers 200 with enabled=false so the UI can render nothing
// without treating it as an error.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")

	payload, loaded := s.store.Get()
	enabled := loaded && payload.Status.Enabled != nil && *payload.Status.Enabled

	if !enabled || s.opts.Status == nil {
		s.writeJSON(w, http.StatusOK, map[string]any{"enabled": false, "results": map[string]any{}})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"enabled": true,
		"results": s.opts.Status.Snapshot(),
	})
}

// handleIcon proxies a site's icon through this origin.
//
// Two things make this safe: the resolver only serves hosts that appear in the
// current config (so this is not an open proxy), and the response carries a
// locked-down CSP of its own. That second part matters: an SVG fetched from a
// third party and served from our origin would otherwise be able to run script
// if someone opened the icon URL directly.
func (s *Server) handleIcon(w http.ResponseWriter, r *http.Request) {
	if s.opts.Icons == nil {
		http.NotFound(w, r)
		return
	}
	host := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("h")))
	if host == "" {
		http.Error(w, "missing h parameter", http.StatusBadRequest)
		return
	}

	pageURL, ok := s.hostIndex()[host]
	if !ok {
		http.NotFound(w, r)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	data, contentType, ok := s.opts.Icons.Lookup(ctx, pageURL)
	if !ok || len(data) == 0 {
		http.NotFound(w, r)
		return
	}

	h := w.Header()
	h.Set("Content-Type", contentType)
	// The browser passes a revision in ?v=, so a long cache is safe and saves
	// re-fetching on every page load.
	h.Set("Cache-Control", "public, max-age=604800")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	h.Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(data)
}

// hostIndex maps host -> a configured page URL on that host. It is memoised by
// config revision because /api/icon is hit once per link on every page load.
func (s *Server) hostIndex() map[string]string {
	payload, ok := s.store.Get()
	if !ok {
		return nil
	}

	s.idxMu.Lock()
	defer s.idxMu.Unlock()
	if s.idx != nil && s.idxRev == payload.Meta.Revision {
		return s.idx
	}

	idx := make(map[string]string, 16)
	for _, raw := range payload.PageURLs() {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			continue
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			continue
		}
		host := strings.ToLower(u.Hostname())
		if _, seen := idx[host]; !seen {
			idx[host] = raw
		}
	}
	s.idx, s.idxRev = idx, payload.Meta.Revision
	return idx
}

// handleThemeCSS emits the design tokens from the config as CSS custom
// properties, followed by the repository's custom stylesheet if one is
// configured.
//
// This is the piece that makes restyling a data change rather than a code
// change: a colour or a radius lives in nav.yaml, arrives through the same
// push-to-update path as everything else, and never needs a rebuild.
func (s *Server) handleThemeCSS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")

	var b strings.Builder
	if payload, ok := s.store.Get(); ok {
		writeThemeTokens(&b, payload.Style)
	}
	if css, rev, ok := s.store.CSS(); ok && len(css) > 0 {
		fmt.Fprintf(&b, "\n/* %s */\n", "custom.css @ "+rev)
		b.Write(css)
		if !strings.HasSuffix(strings.TrimSpace(string(css)), "}") {
			b.WriteString("\n")
		}
	}
	_, _ = io.WriteString(w, b.String())
}

// writeThemeTokens renders the non-empty style tokens.
//
// The selector list repeats the dark theme on purpose: the built-in stylesheet
// declares its dark values under :root[data-theme="dark"], which has the same
// specificity as ours, so ours only wins by coming later. Matching both
// selectors keeps a user-set accent from being ignored in dark mode.
func writeThemeTokens(b *strings.Builder, style nav.Style) {
	type token struct {
		name  string
		value string
	}
	var tokens []token
	add := func(name, value string) {
		if value != "" {
			tokens = append(tokens, token{name, value})
		}
	}

	add("--accent", style.Accent)
	add("--bg", style.Background)
	add("--surface", style.Surface)
	add("--text", style.Text)
	add("--muted", style.Muted)
	add("--border", style.Border)
	add("--font-family", style.Font)

	if style.Radius > 0 {
		add("--radius", strconv.Itoa(style.Radius)+"px")
		add("--radius-sm", strconv.Itoa(max(style.Radius-4, 2))+"px")
	}
	if style.FontSize > 0 {
		add("--font-size", strconv.Itoa(style.FontSize)+"px")
	}
	if style.Width > 0 {
		add("--max-width", strconv.Itoa(style.Width)+"px")
	}
	if style.IconSize > 0 {
		add("--icon-size", strconv.Itoa(style.IconSize)+"px")
	}
	switch style.Density {
	case "compact":
		add("--gap", "10px")
		add("--card-padding", "10px")
		add("--link-padding", "5px 8px")
	case "comfortable":
		add("--gap", "16px")
		add("--card-padding", "16px")
		add("--link-padding", "9px 10px")
	}
	switch style.CardStyle {
	case "flat":
		add("--card-shadow", "none")
		add("--card-shadow-hover", "none")
	case "soft":
		add("--card-shadow", "0 1px 2px rgba(16,24,40,.05)")
		add("--card-shadow-hover", "0 2px 6px rgba(16,24,40,.06), 0 10px 24px rgba(16,24,40,.08)")
	case "strong":
		add("--card-shadow", "0 6px 24px rgba(16,24,40,.12)")
		add("--card-shadow-hover", "0 10px 34px rgba(16,24,40,.18)")
	}

	if len(tokens) == 0 {
		return
	}
	sort.Slice(tokens, func(i, j int) bool { return tokens[i].name < tokens[j].name })

	b.WriteString("/* generated from nav.yaml style: */\n:root, :root[data-theme=\"dark\"] {\n")
	for _, t := range tokens {
		fmt.Fprintf(b, "  %s: %s;\n", t.name, t.value)
	}
	b.WriteString("}\n")
}

// handleEvents streams changes to the browser over SSE. The browser therefore
// learns about a push immediately instead of waiting for a poll, at the cost of
// one idle connection.
//
// A single event type means "something changed, re-read": the browser then
// refetches and diffs. Using one type rather than one per source removes any
// chance of a dropped notification leaving the page stale.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	// Tell nginx and friends not to buffer, or events arrive in clumps.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// Hand the client the revision it should already have, so a page that
	// loaded during a refresh can catch up without waiting for the next push.
	if payload, ok := s.store.Get(); ok {
		writeSSE(w, "hello", payload.Meta.Revision, fmt.Sprintf(`{"revision":%q}`, payload.Meta.Revision))
	} else {
		writeSSE(w, "hello", "", `{"revision":""}`)
	}
	flusher.Flush()

	updates, cancel := s.store.Subscribe()
	defer cancel()

	// Comment frames keep proxies and NATs from dropping an idle connection.
	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case _, open := <-updates:
			if !open {
				return
			}
			rev := ""
			if payload, ok := s.store.Get(); ok {
				rev = payload.Meta.Revision
			}
			writeSSE(w, "changed", rev, "{}")
			flusher.Flush()
		case <-heartbeat.C:
			_, _ = fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

// writeSSE emits one event, stripping newlines that would break the framing.
func writeSSE(w http.ResponseWriter, event, id, data string) {
	if event != "" {
		_, _ = fmt.Fprintf(w, "event: %s\n", sanitiseSSEField(event))
	}
	if id != "" {
		_, _ = fmt.Fprintf(w, "id: %s\n", sanitiseSSEField(id))
	}
	_, _ = fmt.Fprintf(w, "data: %s\n\n", strings.ReplaceAll(data, "\n", " "))
}

func sanitiseSSEField(v string) string {
	return strings.NewReplacer("\n", "", "\r", "").Replace(v)
}

// handleRefresh is the manual trigger, for a deploy script or a `curl` in a
// terminal. It is disabled unless an admin token is configured.
func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	if s.opts.AdminToken == "" {
		s.writeJSON(w, http.StatusForbidden, navError{
			Error: "manual refresh is disabled; start ghome with --admin-token to enable it",
		})
		return
	}
	token := bearerToken(r)
	if token == "" {
		token = r.Header.Get("X-Ghome-Token")
	}
	if !constantTimeEqual(token, s.opts.AdminToken) {
		s.writeJSON(w, http.StatusUnauthorized, navError{Error: "invalid admin token"})
		return
	}

	if s.opts.Trigger != nil {
		s.opts.Trigger()
	}
	s.log.Info("manual refresh requested", "remote", clientIP(r))
	s.writeJSON(w, http.StatusAccepted, map[string]string{"status": "refresh requested"})
}

// healthState is the /healthz payload. It reports the last error but never any
// credential.
type healthState struct {
	Status        string    `json:"status"`
	Version       string    `json:"version"`
	Source        string    `json:"source"`
	Revision      string    `json:"revision"`
	ThemeRevision string    `json:"themeRevision,omitempty"`
	UpdatedAt     time.Time `json:"updatedAt"`
	Groups        int       `json:"groups"`
	Links         int       `json:"links"`
	Subscribers   int       `json:"subscribers"`
	Uptime        string    `json:"uptime"`

	// Runtime figures, so a small box can be watched without extra tooling.
	Goroutines int     `json:"goroutines"`
	HeapMB     float64 `json:"heapMB"`
	SysMB      float64 `json:"sysMB"`

	ProbesEnabled bool `json:"probesEnabled"`
	ProbesUp      int  `json:"probesUp,omitempty"`
	ProbesDown    int  `json:"probesDown,omitempty"`

	IconsCached int `json:"iconsCached,omitempty"`

	Warnings  []string `json:"warnings,omitempty"`
	LastError string   `json:"lastError,omitempty"`
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	payload, ok := s.store.Get()
	state := healthState{
		Status:      "ok",
		Version:     s.opts.Version,
		Subscribers: s.store.SubscriberCount(),
		Uptime:      time.Since(s.opts.Started).Round(time.Second).String(),
		Goroutines:  runtime.NumGoroutine(),
	}
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	state.HeapMB = round1(float64(mem.HeapAlloc) / (1 << 20))
	state.SysMB = round1(float64(mem.Sys) / (1 << 20))

	state.Source = s.opts.Source
	if ok {
		state.Source = payload.Meta.Source
		state.Revision = payload.Meta.Revision
		state.ThemeRevision = payload.Meta.ThemeRevision
		state.UpdatedAt = payload.Meta.UpdatedAt
		state.Groups = payload.Meta.GroupCount
		state.Links = payload.Meta.LinkCount
		state.ProbesEnabled = payload.Status.Enabled != nil && *payload.Status.Enabled
	} else {
		state.Status = "starting"
	}

	if state.ProbesEnabled && s.opts.Status != nil {
		_, up, down := s.opts.Status.Stats()
		state.ProbesUp, state.ProbesDown = up, down
	}
	if s.opts.Icons != nil {
		state.IconsCached, _, _ = s.opts.Icons.Stats()
	}

	// Warnings come from the most recent attempt, so a typo is visible even
	// while a previously good config is still being served.
	state.Warnings = s.store.Warnings()

	if err := s.store.LastError(); err != nil {
		state.LastError = err.Error()
		if ok {
			// The page still works, but the config in git is broken. Say so,
			// so a monitor can tell "fine" from "serving a stale copy".
			state.Status = "degraded"
		} else {
			state.Status = "error"
		}
	}

	code := http.StatusOK
	if !ok {
		code = http.StatusServiceUnavailable
	}
	s.writeJSON(w, code, state)
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		s.log.Debug("failed to write json response", "error", err)
	}
}

func bearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(auth) > len(prefix) && strings.EqualFold(auth[:len(prefix)], prefix) {
		return strings.TrimSpace(auth[len(prefix):])
	}
	return ""
}

// clientIP returns a best effort client address for logs. It trusts
// X-Forwarded-For, which is only correct behind a proxy that overwrites it.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if first, _, found := strings.Cut(xff, ","); found || first != "" {
			return strings.TrimSpace(first)
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// statusRecorder captures the status code and size for the access log, and
// forwards Flush so SSE keeps working through the middleware.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}

		level := slog.LevelInfo
		switch {
		case strings.Contains(r.URL.Path, "/assets/"), strings.HasSuffix(r.URL.Path, "/api/icon"):
			level = slog.LevelDebug // a page load fetches many small files
		case strings.HasSuffix(r.URL.Path, "/api/events"):
			// Long lived: duration is the connection lifetime, not latency.
			level = slog.LevelDebug
		case rec.status >= 500:
			level = slog.LevelWarn
		}
		// Deliberately logs the path only, never the query string: /api/icon
		// carries a configured link URL, which may hold a token.
		s.log.Log(r.Context(), level, "http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"bytes", rec.bytes,
			"duration", time.Since(start).Round(time.Millisecond),
			"remote", clientIP(r))
	})
}

// round1 keeps the reported memory figures readable in JSON.
func round1(v float64) float64 { return math.Round(v*10) / 10 }

// DumpWeb writes the embedded UI to dir.
//
// It exists so --web-dir users can start from a real, working copy instead of
// an empty directory: dump, edit one file, point --web-dir at it. Files left
// untouched keep tracking the binary.
func DumpWeb(dir string) error {
	if dir == "" {
		return errors.New("server: a destination directory is required")
	}
	sub, err := fs.Sub(webRoot, "web")
	if err != nil {
		return fmt.Errorf("server: embedded assets: %w", err)
	}
	return fs.WalkDir(sub, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		target := filepath.Join(dir, filepath.FromSlash(path))
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := fs.ReadFile(sub, path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}
