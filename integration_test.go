package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/simon/ghome/internal/nav"
	"github.com/simon/ghome/internal/server"
	"github.com/simon/ghome/internal/source"
	"github.com/simon/ghome/internal/store"
	"github.com/simon/ghome/internal/watcher"
)

// These tests wire the real source, store, watcher and HTTP layer together the
// same way main.go does, against a fake Gitea. That matters for the stylesheet
// path in particular: nav.yaml and custom.css are two independent API calls, so
// a "custom.css changed but nav.yaml did not" push is a distinct case that the
// local file source cannot express.

const testWebhookSecret = "integration-secret"

// fakeGitea is a stand-in for the Gitea contents API.
type fakeGitea struct {
	server   *httptest.Server
	mu       sync.Mutex
	files    map[string]string
	requests map[string]int
}

func newFakeGitea(t *testing.T) *fakeGitea {
	t.Helper()

	g := &fakeGitea{files: map[string]string{}, requests: map[string]int{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/repos/{owner}/{repo}/contents/{path...}", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "token test-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		path := r.PathValue("path")

		g.mu.Lock()
		content, ok := g.files[path]
		g.requests[path]++
		g.mu.Unlock()

		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name":     path,
			"path":     path,
			"sha":      blobSHA(content),
			"size":     len(content),
			"encoding": "base64",
			"content":  base64.StdEncoding.EncodeToString([]byte(content)),
		})
	})
	g.server = httptest.NewServer(mux)
	t.Cleanup(g.server.Close)
	return g
}

func (g *fakeGitea) set(path, content string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.files[path] = content
}

func (g *fakeGitea) hits(path string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.requests[path]
}

// blobSHA stands in for a git blob id. All the watcher needs is a marker that
// changes when, and only when, the content does.
func blobSHA(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:20]) // 40 hex chars, like a real blob id
}

const stackedNavDoc = `site:
  title: 集成测试
style:
  accent: "#123456"
  customCss: "custom.css"
groups:
  - name: x
    links:
      - name: A
        url: https://a.example.com
`

// navStack is the assembled application, listening on a real port.
type navStack struct {
	gitea  *fakeGitea
	store  *store.Store
	web    *httptest.Server
	secret string
}

func newNavStack(t *testing.T) *navStack {
	t.Helper()

	gitea := newFakeGitea(t)
	gitea.set("nav.yaml", stackedNavDoc)
	gitea.set("custom.css", "/* v1 */\n.link { color: red; }\n")

	src, err := source.NewGitea(source.GiteaOptions{
		BaseURL: gitea.server.URL, Owner: "simon", Repo: "nav",
		Ref: "main", Path: "nav.yaml", Token: "test-token",
	})
	if err != nil {
		t.Fatalf("NewGitea() error = %v", err)
	}

	st := store.New()
	// An hour long poll interval makes the webhook the only thing that can
	// cause a reload, which is exactly the property under test.
	watch := watcher.New(src, st, time.Hour, discardLogger())

	srv, err := server.New(server.Options{
		Store:         st,
		Trigger:       watch.Kick,
		Branch:        "main",
		WebhookSecret: testWebhookSecret,
	})
	if err != nil {
		t.Fatalf("server.New() error = %v", err)
	}

	web := httptest.NewServer(srv)
	t.Cleanup(web.Close)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go watch.Run(ctx)

	stack := &navStack{gitea: gitea, store: st, web: web, secret: testWebhookSecret}
	waitFor(t, 5*time.Second, "the initial load", func() bool { return st.Loaded() })
	return stack
}

func (s *navStack) get(t *testing.T, path string) string {
	t.Helper()
	res, err := http.Get(s.web.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(body)
}

func (s *navStack) theme(t *testing.T) string { return s.get(t, "/theme.css") }

func (s *navStack) meta(t *testing.T) nav.Meta {
	t.Helper()
	payload, ok := s.store.Get()
	if !ok {
		t.Fatal("the store holds nothing")
	}
	return payload.Meta
}

// push delivers a correctly signed push event the way Gitea would.
func (s *navStack) push(t *testing.T) {
	t.Helper()
	body := `{"ref":"refs/heads/main","after":"0123456789abcdef","repository":{"full_name":"simon/nav"}}`
	mac := hmac.New(sha256.New, []byte(s.secret))
	mac.Write([]byte(body))

	req, err := http.NewRequest(http.MethodPost, s.web.URL+"/api/webhook", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build webhook request: %v", err)
	}
	req.Header.Set("X-Gitea-Event", "push")
	req.Header.Set("X-Gitea-Signature", hex.EncodeToString(mac.Sum(nil)))

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /api/webhook: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusAccepted {
		payload, _ := io.ReadAll(res.Body)
		t.Fatalf("webhook status = %d, want 202 (%s)", res.StatusCode, payload)
	}
}

func (s *navStack) waitForTheme(t *testing.T, substr string) {
	t.Helper()
	// The watcher coalesces webhook bursts for ~1.2s before fetching.
	deadline := time.Now().Add(6 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		last = s.theme(t)
		if strings.Contains(last, substr) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("theme.css never contained %q; last body:\n%s", substr, last)
}

// TestStylesheetOnlyPushIsVisibleToTheBrowser is the case the local file source
// cannot express: the document is untouched, so the config revision does not
// move, and only the stylesheet changes.
func TestStylesheetOnlyPushIsVisibleToTheBrowser(t *testing.T) {
	stack := newNavStack(t)

	if body := stack.theme(t); !strings.Contains(body, "color: red") {
		t.Fatalf("initial stylesheet is wrong:\n%s", body)
	}

	before := stack.meta(t)

	// Touch only custom.css.
	stack.gitea.set("custom.css", "/* v2 */\n.link { color: blue; }\n")
	stack.push(t)
	stack.waitForTheme(t, "color: blue")

	after := stack.meta(t)

	if after.Revision != before.Revision {
		t.Errorf("config revision moved from %q to %q although nav.yaml was not touched",
			before.Revision, after.Revision)
	}
	if after.ThemeRevision == before.ThemeRevision {
		t.Error("ThemeRevision did not change, so a live browser would keep showing the old stylesheet")
	}
	if after.ThemeRevision == "" {
		t.Error("ThemeRevision is empty; the browser would fall back to the config revision and miss this update")
	}
}

// A change to the style block in nav.yaml must move ThemeRevision too, even
// though custom.css is unchanged.
func TestConfigStyleChangeAlsoMovesThemeRevision(t *testing.T) {
	stack := newNavStack(t)
	before := stack.meta(t)

	stack.gitea.set("nav.yaml", strings.Replace(stackedNavDoc, `"#123456"`, `"#abcdef"`, 1))
	stack.push(t)
	stack.waitForTheme(t, "--accent: #abcdef")

	after := stack.meta(t)
	if after.ThemeRevision == before.ThemeRevision {
		t.Error("ThemeRevision did not move after a style token changed in nav.yaml")
	}
	if after.Revision == before.Revision {
		t.Error("config revision should move when nav.yaml changes")
	}
}

// Adding the stylesheet after the fact must work: the watcher has to notice the
// config now names one, and fetch it in the same reload.
func TestCustomCSSAddedLaterIsPickedUp(t *testing.T) {
	stack := newNavStack(t)

	// Start from a config with no stylesheet at all.
	withoutCSS := strings.Replace(stackedNavDoc, "  customCss: \"custom.css\"\n", "", 1)
	stack.gitea.set("nav.yaml", withoutCSS)
	stack.gitea.set("custom.css", "")
	stack.push(t)
	waitFor(t, 6*time.Second, "the config without customCss", func() bool {
		_, _, ok := stack.store.CSS()
		return !ok
	})
	before := stack.meta(t)

	// Now name one.
	stack.gitea.set("nav.yaml", stackedNavDoc)
	stack.gitea.set("custom.css", "/* added later */\n.link { color: green; }\n")
	stack.push(t)
	stack.waitForTheme(t, "color: green")

	if after := stack.meta(t); after.ThemeRevision == before.ThemeRevision {
		t.Error("ThemeRevision did not move when a stylesheet was introduced")
	}
}

// Removing it must work too, and must not leave the old rules behind.
func TestCustomCSSRemovalClearsTheStylesheet(t *testing.T) {
	stack := newNavStack(t)
	if body := stack.theme(t); !strings.Contains(body, "color: red") {
		t.Fatalf("initial stylesheet is wrong:\n%s", body)
	}

	withoutCSS := strings.Replace(stackedNavDoc, "  customCss: \"custom.css\"\n", "", 1)
	stack.gitea.set("nav.yaml", withoutCSS)
	stack.push(t)

	waitFor(t, 6*time.Second, "the stylesheet to be dropped", func() bool {
		return !strings.Contains(stack.theme(t), "color: red")
	})
	// The generated tokens must survive the stylesheet going away.
	if body := stack.theme(t); !strings.Contains(body, "--accent: #123456") {
		t.Errorf("dropping custom.css also lost the style tokens:\n%s", body)
	}
}

// A missing stylesheet is a warning, never a failure: the navigation page must
// still come up.
func TestMissingStylesheetDegradesToAWarning(t *testing.T) {
	gitea := newFakeGitea(t)
	gitea.set("nav.yaml", stackedNavDoc) // names custom.css, which does not exist

	src, err := source.NewGitea(source.GiteaOptions{
		BaseURL: gitea.server.URL, Owner: "simon", Repo: "nav",
		Ref: "main", Path: "nav.yaml", Token: "test-token",
	})
	if err != nil {
		t.Fatalf("NewGitea() error = %v", err)
	}

	st := store.New()
	watch := watcher.New(src, st, time.Hour, discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watch.Run(ctx)

	waitFor(t, 5*time.Second, "the config to load despite the missing stylesheet", func() bool {
		return st.Loaded()
	})
	if _, _, ok := st.CSS(); ok {
		t.Error("store claims to hold a stylesheet that does not exist")
	}

	payload, _ := st.Get()
	var found bool
	for _, warning := range payload.Meta.Warnings {
		if strings.Contains(warning, "customCss") {
			found = true
		}
	}
	if !found {
		t.Errorf("warnings = %v, want one about the missing stylesheet", payload.Meta.Warnings)
	}
	// The page itself is fine.
	if payload.Meta.LinkCount != 1 {
		t.Errorf("LinkCount = %d, want the page to survive", payload.Meta.LinkCount)
	}
}

// The stylesheet must be re-read on every reload. Caching it server side would
// silently freeze it at whatever it looked like when the process started.
func TestStylesheetIsRefetchedOnEveryReload(t *testing.T) {
	stack := newNavStack(t)
	first := stack.gitea.hits("custom.css")
	if first == 0 {
		t.Fatal("the stylesheet was never fetched")
	}

	for i := 0; i < 3; i++ {
		stack.push(t)
		time.Sleep(100 * time.Millisecond)
	}
	waitFor(t, 6*time.Second, "the stylesheet to be re-read", func() bool {
		return stack.gitea.hits("custom.css") > first
	})
}

// Guard the shape the browser depends on: /api/nav has to carry both revisions.
func TestNavPayloadExposesBothRevisions(t *testing.T) {
	stack := newNavStack(t)

	var payload struct {
		Meta struct {
			Revision      string `json:"revision"`
			ThemeRevision string `json:"themeRevision"`
		} `json:"meta"`
	}
	body := stack.get(t, "/api/nav")
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("bad JSON from /api/nav: %v", err)
	}
	if payload.Meta.Revision == "" {
		t.Error("revision is missing")
	}
	if payload.Meta.ThemeRevision == "" {
		t.Fatal("themeRevision is missing: a stylesheet-only push would go unnoticed by the browser")
	}
	if payload.Meta.ThemeRevision == payload.Meta.Revision {
		t.Error("themeRevision equals revision, which means custom.css is not part of it")
	}

	// /healthz has to agree, so the value is checkable from a terminal.
	var health struct {
		Revision      string `json:"revision"`
		ThemeRevision string `json:"themeRevision"`
	}
	if err := json.Unmarshal([]byte(stack.get(t, "/healthz")), &health); err != nil {
		t.Fatalf("bad JSON from /healthz: %v", err)
	}
	if health.ThemeRevision != payload.Meta.ThemeRevision {
		t.Errorf("/healthz themeRevision = %q, /api/nav = %q",
			health.ThemeRevision, payload.Meta.ThemeRevision)
	}
}

func TestThemeRevisionChangesWhenEitherFileChanges(t *testing.T) {
	cases := []struct {
		name           string
		navRev, cssRev string
	}{
		{"both empty", "", ""},
		{"document only", "a", ""},
		{"stylesheet only", "", "b"},
		{"different document", "c", ""},
		{"different stylesheet", "", "d"},
	}
	seen := map[string]string{}
	for _, tc := range cases {
		got := themeRevisionOf(t, tc.navRev, tc.cssRev)
		if prev, dup := seen[got]; dup {
			t.Errorf("%s and %s produced the same ThemeRevision %q; a change to one would be missed",
				prev, tc.name, got)
		}
		seen[got] = tc.name
	}
}

// themeRevisionOf reaches the unexported helper through the store, so the test
// exercises the value the browser actually receives.
func themeRevisionOf(t *testing.T, navRev, cssRev string) string {
	t.Helper()
	st := store.New()
	cfg, warnings, err := nav.Parse([]byte(stackedNavDoc))
	if err != nil {
		t.Fatalf("nav.Parse() error = %v", err)
	}
	st.Publish(cfg, []byte(stackedNavDoc), navRev, "test", warnings, []byte("x"), cssRev)
	payload, _ := st.Get()
	if payload.Meta.ThemeRevision == "" {
		t.Fatal("ThemeRevision is empty")
	}
	return fmt.Sprintf("%s|%s", payload.Meta.ThemeRevision, "")
}
