package server

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vrocwang/ghome/internal/nav"
	"github.com/vrocwang/ghome/internal/status"
	"github.com/vrocwang/ghome/internal/store"
)

const goodDoc = `
groups:
  - name: 开发
    links:
      - name: Gitea
        url: https://git.example.com
`

type triggerCounter struct{ n int }

func (c *triggerCounter) kick() { c.n++ }

func testStore(t *testing.T) *store.Store {
	t.Helper()
	st := store.New()
	if err := st.Load([]byte(goodDoc), "rev-1", "test"); err != nil {
		t.Fatalf("store.Load() error = %v", err)
	}
	return st
}

func newTestServer(t *testing.T, opts Options) (*Server, *triggerCounter) {
	t.Helper()
	if opts.Store == nil {
		opts.Store = testStore(t)
	}
	counter := &triggerCounter{}
	if opts.Trigger == nil {
		opts.Trigger = counter.kick
	}
	srv, err := New(opts)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return srv, counter
}

func sign(t *testing.T, body, secret string) string {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return hex.EncodeToString(mac.Sum(nil))
}

func postWebhook(t *testing.T, srv *Server, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/webhook", strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

const pushMain = `{"ref":"refs/heads/main","after":"0123456789abcdef","repository":{"full_name":"acme/nav"}}`

func TestWebhookIsDisabledWithoutASecret(t *testing.T) {
	srv, counter := newTestServer(t, Options{})

	rec := postWebhook(t, srv, pushMain, nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
	if counter.n != 0 {
		t.Errorf("trigger ran %d times, want 0", counter.n)
	}
}

func TestWebhookRejectsABadSignature(t *testing.T) {
	srv, counter := newTestServer(t, Options{WebhookSecret: "s3cret"})

	rec := postWebhook(t, srv, pushMain, map[string]string{
		"X-Gitea-Event":     "push",
		"X-Gitea-Signature": strings.Repeat("0", 64),
	})
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if counter.n != 0 {
		t.Errorf("trigger ran %d times, want 0", counter.n)
	}
}

func TestWebhookRejectsAMissingSignature(t *testing.T) {
	srv, _ := newTestServer(t, Options{WebhookSecret: "s3cret"})

	if rec := postWebhook(t, srv, pushMain, map[string]string{"X-Gitea-Event": "push"}); rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestWebhookTriggersOnAnyPush(t *testing.T) {
	// There is no branch filter here: the git server's webhook filter decides
	// what is delivered, and ghome reads one ref regardless.
	srv, counter := newTestServer(t, Options{WebhookSecret: "s3cret"})

	rec := postWebhook(t, srv, pushMain, map[string]string{
		"X-Gitea-Event":     "push",
		"X-Gitea-Signature": sign(t, pushMain, "s3cret"),
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %s)", rec.Code, rec.Body)
	}
	if counter.n != 1 {
		t.Errorf("trigger ran %d times, want 1", counter.n)
	}
}

// A push event that does not say which ref moved is still a reason to reload:
// a missed update is worse than a spare fetch, and the watcher stops a fetch
// that changed nothing.
func TestWebhookWithoutARefStillReloads(t *testing.T) {
	srv, counter := newTestServer(t, Options{WebhookSecret: "s3cret"})
	body := `{"repository":{"full_name":"acme/nav"}}`
	postWebhook(t, srv, body, map[string]string{
		"X-Gitea-Event":     "push",
		"X-Gitea-Signature": sign(t, body, "s3cret"),
	})
	if counter.n != 1 {
		t.Errorf("a push event without a ref reloaded %d times, want 1", counter.n)
	}
}

func TestWebhookAnswersGiteaPing(t *testing.T) {
	srv, counter := newTestServer(t, Options{WebhookSecret: "s3cret"})

	body := `{"zen":"Keep it simple."}`
	rec := postWebhook(t, srv, body, map[string]string{
		"X-Gitea-Event":     "ping",
		"X-Gitea-Signature": sign(t, body, "s3cret"),
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	if counter.n != 0 {
		t.Errorf("a ping must not trigger a reload, ran %d times", counter.n)
	}
}

func TestWebhookAcceptsTheGitHubStyleSignature(t *testing.T) {
	srv, counter := newTestServer(t, Options{WebhookSecret: "s3cret"})

	rec := postWebhook(t, srv, pushMain, map[string]string{
		"X-Gitea-Event":       "push",
		"X-Hub-Signature-256": "sha256=" + sign(t, pushMain, "s3cret"),
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %s)", rec.Code, rec.Body)
	}
	if counter.n != 1 {
		t.Errorf("trigger ran %d times, want 1", counter.n)
	}
}

func TestWebhookAcceptsTheLegacySHA1Signature(t *testing.T) {
	srv, _ := newTestServer(t, Options{WebhookSecret: "s3cret"})

	mac := hmac.New(sha1.New, []byte("s3cret"))
	mac.Write([]byte(pushMain))

	rec := postWebhook(t, srv, pushMain, map[string]string{
		"X-Gitea-Event":   "push",
		"X-Hub-Signature": "sha1=" + hex.EncodeToString(mac.Sum(nil)),
	})
	if rec.Code != http.StatusAccepted {
		t.Errorf("status = %d, want 202", rec.Code)
	}
}

func TestNavEndpoint(t *testing.T) {
	t.Run("serves the document once loaded", func(t *testing.T) {
		srv, _ := newTestServer(t, Options{})
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/nav", nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		var payload struct {
			Site struct {
				Title string `json:"title"`
			} `json:"site"`
			Groups []struct {
				Name  string `json:"name"`
				Links []struct {
					Name string `json:"name"`
					URL  string `json:"url"`
				} `json:"links"`
			} `json:"groups"`
			Meta struct {
				Revision  string `json:"revision"`
				LinkCount int    `json:"linkCount"`
			} `json:"meta"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatalf("response is not valid JSON: %v", err)
		}
		if payload.Meta.Revision != "rev-1" || payload.Meta.LinkCount != 1 {
			t.Errorf("meta = %+v", payload.Meta)
		}
		if len(payload.Groups) != 1 || payload.Groups[0].Links[0].URL != "https://git.example.com" {
			t.Errorf("groups = %+v", payload.Groups)
		}
	})

	t.Run("returns 503 before the first load", func(t *testing.T) {
		srv, _ := newTestServer(t, Options{Store: store.New()})
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/nav", nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503", rec.Code)
		}
	})
}

func TestHealthEndpointReportsState(t *testing.T) {
	srv, _ := newTestServer(t, Options{Version: "test"})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var state healthState
	if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if state.Status != "ok" || state.Revision != "rev-1" || state.Version != "test" {
		t.Errorf("state = %+v", state)
	}
}

func TestManualRefreshNeedsTheAdminToken(t *testing.T) {
	srv, counter := newTestServer(t, Options{AdminToken: "letmein"})

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/refresh", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("without a token: status = %d, want 401", rec.Code)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/refresh", nil)
	req.Header.Set("Authorization", "Bearer letmein")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Errorf("with a token: status = %d, want 202", rec.Code)
	}
	if counter.n != 1 {
		t.Errorf("trigger ran %d times, want 1", counter.n)
	}
}

func TestManualRefreshIsDisabledByDefault(t *testing.T) {
	srv, _ := newTestServer(t, Options{})
	req := httptest.NewRequest(http.MethodPost, "/api/refresh", nil)
	req.Header.Set("Authorization", "Bearer anything")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

// TestEventStream covers the whole live update path: connect, receive the
// current revision, then get pushed an event when the document changes.
func TestEventStream(t *testing.T) {
	st := testStore(t)
	srv, _ := newTestServer(t, Options{Store: st})
	httpSrv := httptest.NewServer(srv)
	defer httpSrv.Close()

	resp, err := http.Get(httpSrv.URL + "/api/events")
	if err != nil {
		t.Fatalf("GET /api/events: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	lines := make(chan string, 64)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
	}()

	waitFor := func(substring string) {
		t.Helper()
		timeout := time.After(5 * time.Second)
		for {
			select {
			case line, open := <-lines:
				if !open {
					t.Fatalf("stream closed while waiting for %q", substring)
				}
				if strings.Contains(line, substring) {
					return
				}
			case <-timeout:
				t.Fatalf("timed out waiting for %q", substring)
			}
		}
	}

	waitFor("event: hello")
	waitFor("rev-1")

	if err := st.Load([]byte(goodDoc), "rev-2", "test"); err != nil {
		t.Fatalf("store.Load() error = %v", err)
	}
	waitFor("event: changed")
	waitFor("rev-2")
}

func TestHealthEndpointFailsBeforeTheFirstLoad(t *testing.T) {
	srv, _ := newTestServer(t, Options{Store: store.New()})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

func TestHealthEndpointReportsADegradedConfig(t *testing.T) {
	st := testStore(t)
	// A broken push while a good config is live: the page keeps working, but
	// the state must say something is wrong.
	st.SetError(errors.New("groups[0].links[0].url: scheme \"javascript\" is not allowed"))

	srv, _ := newTestServer(t, Options{Store: st})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: the page is still served", rec.Code)
	}
	var state healthState
	if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if state.Status != "degraded" {
		t.Errorf("Status = %q, want degraded", state.Status)
	}
	if !strings.Contains(state.LastError, "javascript") {
		t.Errorf("LastError = %q", state.LastError)
	}
}

func TestBasePathMountsUnderAPrefix(t *testing.T) {
	srv, _ := newTestServer(t, Options{BasePath: "/nav"})

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nav/api/nav", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("GET /nav/api/nav: status = %d, want 200", rec.Code)
	}

	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/nav", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /api/nav outside the prefix: status = %d, want 404", rec.Code)
	}

	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nav", nil))
	if rec.Code != http.StatusMovedPermanently {
		t.Errorf("GET /nav: status = %d, want 301 so relative URLs resolve", rec.Code)
	}
}

func TestSecurityHeaders(t *testing.T) {
	srv, _ := newTestServer(t, Options{})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") {
		t.Errorf("Content-Security-Policy = %q", csp)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("missing nosniff header")
	}
}

/* ---------- new surface: theme tokens, icons, probes, web-dir ---------- */

func styledStore(t *testing.T, doc string) *store.Store {
	t.Helper()
	st := store.New()
	if err := st.Load([]byte(doc), "rev-1", "test"); err != nil {
		t.Fatalf("store.Load() error = %v", err)
	}
	return st
}

func TestThemeCSSEmitsTokens(t *testing.T) {
	st := styledStore(t, `
style:
  accent: "#ff6600"
  radius: 20
  fontSize: 16
  density: compact
  cardStyle: flat
  font: "Inter, system-ui"
`+goodDoc)

	srv, _ := newTestServer(t, Options{Store: st})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/theme.css", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/css") {
		t.Errorf("Content-Type = %q", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"--accent: #ff6600;",
		"--radius: 20px;",
		"--font-size: 16px;",
		"--font-family: Inter, system-ui;",
		"--card-shadow: none;", // cardStyle: flat
	} {
		if !strings.Contains(body, want) {
			t.Errorf("theme.css is missing %q\n%s", want, body)
		}
	}
	// Nothing generated may carry a dark selector: such a rule would outrank the
	// bare :root of a custom.css the operator appends after this file, so their
	// dark colour or shadow would silently lose. Dark values ride on --dark-*
	// names under plain :root instead, and the built-in stylesheet picks them up.
	if strings.Contains(body, `[data-theme="dark"]`) {
		t.Errorf("the generated stylesheet must not carry a dark selector:\n%s", body)
	}
	if !strings.Contains(body, `:root {
`) {
		t.Errorf("expected a plain :root block:\n%s", body)
	}
}

// cardStyle is a preset, not a light-theme colour, and the built-in stylesheet
// has shadows of its own for dark mode -- so a preset has to state both or the
// built-in wins there. cardStyle: flat used to come back with shadows the moment
// the page went dark.
func TestCardStyleStatesBothThemes(t *testing.T) {
	cases := map[string]struct{ light, dark string }{
		"flat":   {"none", ""},
		"soft":   {"0 1px 2px rgba(16,24,40,.05)", ""},
		"strong": {"0 6px 24px rgba(16,24,40,.12)", ""},
	}
	for cardStyle, want := range cases {
		t.Run(cardStyle, func(t *testing.T) {
			srv, _ := newTestServer(t, Options{Store: styledStore(t, "style:\n  cardStyle: "+cardStyle+"\n"+goodDoc)})
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/theme.css", nil))
			body := rec.Body.String()

			// Both themes, one value: a preset has to survive dark mode (flat
			// used to come back with the built-in dark shadow) and also stay
			// overridable from a custom.css, which a --dark-* resolver rule
			// would have prevented.
			if !strings.Contains(body, "--card-shadow: "+want.light+";") {
				t.Errorf("--card-shadow, want %q:\n%s", want.light, body)
			}
			if strings.Contains(body, "--dark-card-shadow") {
				t.Errorf("a preset must not park its shadow on a --dark-* name:\n%s", body)
			}
		})
	}
}

// The generated dark values only work because the built-in stylesheet consumes
// them. Pin that contract: the two files are written apart but have to agree on
// the --dark-* names and on their fallbacks.
func TestBuiltInStylesheetResolvesGeneratedDarkValues(t *testing.T) {
	css, err := os.ReadFile("web/assets/style.css")
	if err != nil {
		t.Fatalf("read %s: %v", "web/assets/style.css", err)
	}
	body := string(css)
	// The last one, not the first: the built-in palette declares the same
	// selector earlier, and it is the appended resolver that has to name every
	// --dark-* it consumes.
	blocks := blocksFor(body, `:root[data-theme="dark"] {`)
	if len(blocks) == 0 {
		t.Fatal("style.css has no dark block at all")
	}
	dark := blocks[len(blocks)-1]
	for _, want := range []string{
		"--bg: var(--dark-bg, #0d0f13);",
		"--surface: var(--dark-surface, #171a20);",
		"--text: var(--dark-text, #e8eaee);",
		"--muted: var(--dark-muted, #99a2b1);",
		"--border: var(--dark-border, #272c35);",
		"--accent: var(--dark-accent, #6f95ff);",
	} {
		if !strings.Contains(dark, want) {
			t.Errorf("style.css is missing %q; a colour set in style.dark would be ignored", want)
		}
	}
	if strings.Contains(dark, "--card-shadow") {
		t.Error("style.css sets a shadow under the dark selector, which outranks a custom.css shadow")
	}
	lightOnly := blockFor(t, body, ":root {")
	if !strings.Contains(lightOnly, "--card-shadow:") {
		t.Error("style.css does not declare --card-shadow on :root; the fallback look is gone")
	}
}

// style.dark is how a config colours dark mode deliberately. A key it leaves
// out must stay out, so the built-in dark value survives instead of being
// replaced by the light one.
func TestThemeCSSEmitsDarkOverridesOnlyForWhatIsSet(t *testing.T) {
	st := styledStore(t, `
style:
  accent: "#ff6600"
  background: "#ffffff"
  dark:
    accent: "#ffb066"
    surface: "#101216"
    radius: 30
    nonsense: "#123456"
`+goodDoc)

	srv, _ := newTestServer(t, Options{Store: st})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/theme.css", nil))
	body := rec.Body.String()

	if !strings.Contains(body, "--accent: #ff6600;") || !strings.Contains(body, "--bg: #ffffff;") {
		t.Errorf("the light colours are missing:\n%s", body)
	}
	if !strings.Contains(body, "--dark-accent: #ffb066;") || !strings.Contains(body, "--dark-surface: #101216;") {
		t.Errorf("the dark overrides are missing:\n%s", body)
	}
	// --bg was set for light only, so no dark counterpart may exist: the built-in
	// dark background has to win.
	if strings.Contains(body, "--dark-bg:") {
		t.Errorf("a light-only colour got a dark counterpart:\n%s", body)
	}
	// Geometry and typography are the same in both themes: a dark-only radius
	// would be a surprise rather than a feature, so style.dark.radius is not a
	// colour token and never reaches the stylesheet.
	if strings.Contains(body, "--dark-radius") {
		t.Errorf("style.dark.radius became a colour token:\n%s", body)
	}
	if strings.Contains(body, "--radius: 30px;") {
		t.Errorf("style.dark.radius was applied as a colour:\n%s", body)
	}
}

// blocksFor returns the body of every CSS block with this header, in file order.
func blocksFor(css, header string) []string {
	var out []string
	rest := css
	for {
		i := strings.Index(rest, header)
		if i < 0 {
			return out
		}
		rest = rest[i+len(header):]
		j := strings.Index(rest, "}")
		if j < 0 {
			return append(out, rest)
		}
		out = append(out, rest[:j])
		rest = rest[j:]
	}
}

// blockFor returns the first CSS block with this header.
func blockFor(t *testing.T, css, header string) string {
	t.Helper()
	blocks := blocksFor(css, header)
	if len(blocks) == 0 {
		t.Fatalf("no %q block in:\n%s", header, css)
	}
	return blocks[0]
}

func TestThemeCSSIsValidWithNoStyleBlock(t *testing.T) {
	srv, _ := newTestServer(t, Options{Store: testStore(t)})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/theme.css", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestThemeCSSAppendsTheRepositoryStylesheet(t *testing.T) {
	st := store.New()
	cfg, warnings, err := nav.Parse([]byte(goodDoc))
	if err != nil {
		t.Fatalf("nav.Parse() error = %v", err)
	}
	custom := []byte(".group { border-width: 2px }")
	st.Publish(cfg, []byte(goodDoc), "rev-1", "test", warnings, custom, "css-rev-1")

	srv, _ := newTestServer(t, Options{Store: st})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/theme.css", nil))

	body := rec.Body.String()
	if !strings.Contains(body, ".group { border-width: 2px }") {
		t.Errorf("custom.css was not appended:\n%s", body)
	}
	if !strings.Contains(body, "css-rev-1") {
		t.Errorf("the stylesheet revision should be recorded:\n%s", body)
	}
}

// fakeIcons lets the handler be tested without a live icon cache.
type fakeIcons struct {
	data []byte
	ct   string
	ok   bool
	got  string
}

func (f *fakeIcons) Lookup(_ context.Context, pageURL string) ([]byte, string, bool) {
	f.got = pageURL
	return f.data, f.ct, f.ok
}

func (f *fakeIcons) Stats() (int, uint64, uint64) { return 1, 0, 0 }

func TestIconEndpoint(t *testing.T) {
	t.Run("serves an icon for a configured host", func(t *testing.T) {
		icons := &fakeIcons{data: []byte("GIF89a"), ct: "image/gif", ok: true}
		srv, _ := newTestServer(t, Options{Icons: icons})

		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/icon?h=git.example.com&v=rev-1", nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if got := rec.Body.String(); got != "GIF89a" {
			t.Errorf("body = %q", got)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "image/gif" {
			t.Errorf("Content-Type = %q", ct)
		}
		// A third-party SVG served from our origin would otherwise be able to
		// run script if opened directly.
		if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "sandbox") {
			t.Errorf("icon responses need a sandboxing CSP, got %q", csp)
		}
		if icons.got != "https://git.example.com" {
			t.Errorf("lookup got %q, want the configured page url", icons.got)
		}
		// The answer depends on who is asking -- a host only a private entry
		// uses is 404 for everyone else -- so a shared cache must not reuse it.
		if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "private") {
			t.Errorf("Cache-Control = %q, want it marked private", cc)
		}
	})

	t.Run("refuses a host that is not in the config", func(t *testing.T) {
		srv, _ := newTestServer(t, Options{Icons: &fakeIcons{ok: true}})
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/icon?h=evil.example.com", nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404: /api/icon must not be an open proxy", rec.Code)
		}
	})

	t.Run("404s when nothing could be resolved", func(t *testing.T) {
		srv, _ := newTestServer(t, Options{Icons: &fakeIcons{ok: false}})
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/icon?h=git.example.com", nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", rec.Code)
		}
	})

	t.Run("is disabled when no resolver is configured", func(t *testing.T) {
		srv, _ := newTestServer(t, Options{})
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/icon?h=git.example.com", nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", rec.Code)
		}
	})
}

// fakeStatus stands in for the prober. It honours the allow list so that tests
// can prove a withheld link's probe result does not leak.
type fakeStatus struct{ results map[string]status.Result }

func (f fakeStatus) Snapshot(allow map[string]struct{}) any {
	if allow == nil {
		return f.results
	}
	out := make(map[string]status.Result, len(f.results))
	for url, r := range f.results {
		if _, ok := allow[url]; ok {
			out[url] = r
		}
	}
	return out
}
func (f fakeStatus) Stats() (int, int, int) {
	up, down := f.StatsFor(nil)
	return len(f.results), up, down
}

// StatsFor mirrors the real prober: counts only what the caller may see.
func (f fakeStatus) StatsFor(allow map[string]struct{}) (int, int) {
	up, down := 0, 0
	for url, r := range f.results {
		if allow != nil {
			if _, ok := allow[url]; !ok {
				continue
			}
		}
		switch r.State {
		case status.StateUp:
			up++
		case status.StateDown:
			down++
		}
	}
	return up, down
}

func TestStatusEndpoint(t *testing.T) {
	t.Run("reports disabled when the config does not enable probing", func(t *testing.T) {
		srv, _ := newTestServer(t, Options{Status: fakeStatus{}})
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/status", nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		var body struct {
			Enabled bool `json:"enabled"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("bad JSON: %v", err)
		}
		if body.Enabled {
			t.Error("enabled = true, want false: probing is opt-in")
		}
	})

	t.Run("returns results when enabled", func(t *testing.T) {
		st := styledStore(t, "status:\n  enabled: true\n"+goodDoc)
		probes := fakeStatus{results: map[string]status.Result{
			"https://git.example.com": {URL: "https://git.example.com", State: status.StateUp, Code: 200},
		}}
		srv, _ := newTestServer(t, Options{Store: st, Status: probes})

		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/status", nil))

		var body struct {
			Enabled bool                     `json:"enabled"`
			Results map[string]status.Result `json:"results"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("bad JSON: %v", err)
		}
		if !body.Enabled {
			t.Fatal("enabled = false, want true")
		}
		if body.Results["https://git.example.com"].State != status.StateUp {
			t.Errorf("results = %+v", body.Results)
		}
	})
}

func TestHealthzReportsWarnings(t *testing.T) {
	st := styledStore(t, `
titel: 顶层拼错了
groups:
  - name: 开发
    links:
      - name: Gitea
        url: https://git.example.com
`)
	srv, _ := newTestServer(t, Options{Store: st})

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: a warning must not fail the page", rec.Code)
	}
	var state healthState
	if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if state.Status != "ok" {
		t.Errorf("Status = %q, want ok: a tolerated typo is not an outage", state.Status)
	}
	// /healthz is unauthenticated and a warning names the entry it came from
	// ("groups[0].links[0] (日记).hotkey: ..."), so the response carries none of
	// them. The operator still gets the full list in the log.
	if len(state.Warnings) != 0 {
		t.Errorf("warnings = %v, want none on an unauthenticated endpoint", state.Warnings)
	}
	if len(st.Warnings()) == 0 {
		t.Error("the store lost the warnings: they are what the log line reports")
	}
}

func TestWebDirOverridesTheEmbeddedUI(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>overridden</html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A partially populated directory must still work: anything absent falls
	// back to the embedded copy.
	srv, _ := newTestServer(t, Options{WebDir: dir})

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if !strings.Contains(rec.Body.String(), "overridden") {
		t.Errorf("index.html was not taken from --web-dir: %s", rec.Body)
	}

	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/assets/app.js", nil))
	if rec.Code != http.StatusOK || rec.Body.Len() == 0 {
		t.Errorf("app.js should fall back to the embedded copy, got %d with %d bytes", rec.Code, rec.Body.Len())
	}
}

func TestWebDirRejectsAMissingDirectory(t *testing.T) {
	if _, err := New(Options{Store: store.New(), WebDir: "/nonexistent/definitely/not/here"}); err == nil {
		t.Fatal("New() accepted a missing --web-dir")
	}
}

// The overlay must not let a request walk out of the override directory.
func TestOverlayFSConfinesPaths(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ok.txt"), []byte("fine"), 0o600); err != nil {
		t.Fatal(err)
	}
	overlay := overlayFS{dir: dir, base: os.DirFS(dir)}

	for _, name := range []string{"../etc/passwd", "../../etc/passwd", "/../../etc/passwd", "a/../../b"} {
		if full, ok := overlay.resolve(name); ok {
			t.Errorf("resolve(%q) = %q, want it refused", name, full)
		}
	}
	if _, ok := overlay.resolve("ok.txt"); !ok {
		t.Error("a normal path should resolve")
	}
}

func TestIconRequestIsNotLoggedWithItsQueryString(t *testing.T) {
	// /api/icon carries a configured link URL, which may contain a token. The
	// access log must record the path only.
	var logged string
	logger := slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
		logged += string(p)
		return len(p), nil
	}), nil))

	icons := &fakeIcons{data: []byte("x"), ct: "image/png", ok: true}
	srv, _ := newTestServer(t, Options{Icons: icons, Logger: logger})

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/api/icon?h=git.example.com&v="+strings.Repeat("a", 40)+"&token=supersecret", nil))

	if strings.Contains(logged, "supersecret") {
		t.Errorf("the access log leaked a query parameter:\n%s", logged)
	}
}

// writerFunc adapts a function to io.Writer for the log capture above.
type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func TestDumpWebExtractsAWorkingUI(t *testing.T) {
	dir := t.TempDir()
	if err := DumpWeb(dir); err != nil {
		t.Fatalf("DumpWeb() error = %v", err)
	}

	for _, name := range []string{"index.html", "assets/app.js", "assets/style.css", "assets/theme.js", "assets/favicon.svg"} {
		info, err := os.Stat(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil {
			t.Errorf("DumpWeb() did not write %s: %v", name, err)
			continue
		}
		if info.Size() == 0 {
			t.Errorf("DumpWeb() wrote an empty %s", name)
		}
	}

	// The dump must be usable directly as --web-dir.
	srv, _ := newTestServer(t, Options{WebDir: dir})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/assets/app.js", nil))
	if rec.Code != http.StatusOK || rec.Body.Len() == 0 {
		t.Errorf("the dumped directory did not serve app.js: %d, %d bytes", rec.Code, rec.Body.Len())
	}
}

func TestDumpWebRejectsAnEmptyDestination(t *testing.T) {
	if err := DumpWeb(""); err == nil {
		t.Fatal("DumpWeb(\"\") succeeded")
	}
}

// An edited dump must win for the file that changed while everything else keeps
// tracking the binary.
func TestWebDirMixesOverriddenAndEmbeddedFiles(t *testing.T) {
	dir := t.TempDir()
	if err := DumpWeb(dir); err != nil {
		t.Fatalf("DumpWeb() error = %v", err)
	}
	edited := []byte("/* edited */\n")
	if err := os.WriteFile(filepath.Join(dir, "assets", "style.css"), edited, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "assets", "app.js")); err != nil {
		t.Fatal(err)
	}

	srv, _ := newTestServer(t, Options{WebDir: dir})

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/assets/style.css", nil))
	if rec.Body.String() != string(edited) {
		t.Errorf("style.css = %q, want the edited copy", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/assets/app.js", nil))
	if rec.Code != http.StatusOK || rec.Body.Len() < 1000 {
		t.Errorf("a deleted file should fall back to the embedded copy, got %d with %d bytes",
			rec.Code, rec.Body.Len())
	}
}

/* ---------- Forgejo ---------- */

// Forgejo is a Gitea fork. Older releases sign only with the Gitea-named
// header, its documentation shows the Forgejo-named one, and newer releases
// appear to send both -- so both have to be accepted rather than betting on one.
func TestWebhookAcceptsForgejoHeaders(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{
			name: "forgejo signature only",
			headers: map[string]string{
				"X-Forgejo-Event":     "push",
				"X-Forgejo-Signature": sign(t, pushMain, "s3cret"),
			},
			want: http.StatusAccepted,
		},
		{
			name: "gitea signature only (older forgejo)",
			headers: map[string]string{
				"X-Gitea-Event":     "push",
				"X-Gitea-Signature": sign(t, pushMain, "s3cret"),
			},
			want: http.StatusAccepted,
		},
		{
			name: "both families, as newer releases send them",
			headers: map[string]string{
				"X-Forgejo-Event":     "push",
				"X-Forgejo-Signature": sign(t, pushMain, "s3cret"),
				"X-Gitea-Event":       "push",
				"X-Gitea-Signature":   sign(t, pushMain, "s3cret"),
			},
			want: http.StatusAccepted,
		},
		{
			name: "a stray bogus header does not veto a valid one",
			headers: map[string]string{
				"X-Forgejo-Event":     "push",
				"X-Forgejo-Signature": sign(t, pushMain, "s3cret"),
				"X-Gitea-Signature":   strings.Repeat("0", 64),
			},
			want: http.StatusAccepted,
		},
		{
			name: "forgejo signature that does not verify",
			headers: map[string]string{
				"X-Forgejo-Event":     "push",
				"X-Forgejo-Signature": strings.Repeat("a", 64),
			},
			want: http.StatusUnauthorized,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, counter := newTestServer(t, Options{WebhookSecret: "s3cret"})
			rec := postWebhook(t, srv, pushMain, tc.headers)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.want, rec.Body)
			}
			if tc.want == http.StatusAccepted && counter.n != 1 {
				t.Errorf("trigger ran %d times, want 1", counter.n)
			}
			if tc.want == http.StatusUnauthorized && counter.n != 0 {
				t.Errorf("trigger ran %d times, want 0", counter.n)
			}
		})
	}
}

// A Forgejo ping must not be mistaken for a push.
func TestWebhookHandlesForgejoPing(t *testing.T) {
	srv, counter := newTestServer(t, Options{WebhookSecret: "s3cret"})

	body := `{"zen":"Non-blocking is better than blocking."}`
	rec := postWebhook(t, srv, body, map[string]string{
		"X-Forgejo-Event":     "ping",
		"X-Forgejo-Signature": sign(t, body, "s3cret"),
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	if counter.n != 0 {
		t.Errorf("a ping must not trigger a reload, ran %d times", counter.n)
	}
}

// The one moment an operator most needs to see what the process is pointed at
// is when nothing has loaded yet.
func TestHealthzReportsTheSourceBeforeTheFirstLoad(t *testing.T) {
	srv, _ := newTestServer(t, Options{
		Store:  store.New(),
		Source: "forgejo:acme/homepage/nav.yaml@main",
	})

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	var state healthState
	if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if state.Source != "forgejo:acme/homepage/nav.yaml@main" {
		t.Errorf("Source = %q, want the configured source", state.Source)
	}
}

// The warm prefetch and /api/icon have to agree on which URL represents a host.
// If they disagree, warm fetches an icon the endpoint never serves and the
// endpoint fetches one on demand -- a silent performance regression with no
// error anywhere. Both now derive from Config.IconURLs; this pins that.
func TestHostIndexAgreesWithIconURLs(t *testing.T) {
	const doc = `
groups:
  - name: g
    links:
      - {name: a, url: "http://nas:8080/"}
      - {name: b, url: "http://nas:9090/deep"}
      - {name: c, url: "https://other.example.com/"}
`
	st := store.New()
	if err := st.Load([]byte(doc), "rev-1", "test"); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	srv, _ := newTestServer(t, Options{Store: st})

	payload, ok := st.Get()
	if !ok {
		t.Fatal("store is not loaded")
	}
	idx := srv.hostIndex()

	iconURLs := payload.IconURLs()
	if len(iconURLs) != 2 {
		t.Fatalf("IconURLs() = %q, want one per host (2)", iconURLs)
	}
	if len(idx) != len(iconURLs) {
		t.Errorf("hostIndex has %d entries, IconURLs has %d; they must cover the same hosts",
			len(idx), len(iconURLs))
	}
	for _, raw := range iconURLs {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("parse %q: %v", raw, err)
		}
		host := strings.ToLower(u.Hostname())
		if idx[host] != raw {
			t.Errorf("hostIndex[%q] = %q, but IconURLs picked %q; warm and the endpoint disagree",
				host, idx[host], raw)
		}
	}
	// The same host reached by a different port must resolve to the first link.
	if got := idx["nas"]; got != "http://nas:8080/" {
		t.Errorf("hostIndex[nas] = %q, want the first configured url on that host", got)
	}
}

// Only a push can change the config. Other events on the same hook are answered
// 202 "ignored" rather than triggering a reload, and a missing event name is
// still honoured: an unknown payload is no reason to skip a possible update.
func TestWebhookOnlyReloadsOnPush(t *testing.T) {
	cases := []struct {
		event  string
		reload bool
		status int
	}{
		{"push", true, http.StatusAccepted},
		{"Push", true, http.StatusAccepted},
		{"", true, http.StatusAccepted},
		{"issues", false, http.StatusAccepted},
		{"release", false, http.StatusAccepted},
		{"pull_request", false, http.StatusAccepted},
	}
	for _, tc := range cases {
		srv, counter := newTestServer(t, Options{WebhookSecret: "s3cret"})
		headers := map[string]string{"X-Gitea-Signature": sign(t, pushMain, "s3cret")}
		if tc.event != "" {
			headers["X-Gitea-Event"] = tc.event
		}
		rec := postWebhook(t, srv, pushMain, headers)
		if rec.Code != tc.status {
			t.Errorf("event %q: status = %d, want %d", tc.event, rec.Code, tc.status)
		}
		reloaded := counter.n == 1
		if reloaded != tc.reload {
			t.Errorf("event %q: reloaded = %v, want %v (body %s)", tc.event, reloaded, tc.reload, rec.Body)
		}
	}
}

// The ping Gitea sends when a hook is created is answered, not acted on.
func TestWebhookAnswersPingWithoutReloading(t *testing.T) {
	srv, counter := newTestServer(t, Options{WebhookSecret: "s3cret"})
	body := `{"zen":"ghome"}`
	rec := postWebhook(t, srv, body, map[string]string{
		"X-Gitea-Event":     "ping",
		"X-Gitea-Signature": sign(t, body, "s3cret"),
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if counter.n != 0 {
		t.Errorf("a ping triggered %d reloads, want 0", counter.n)
	}
}
