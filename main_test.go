package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vrocwang/ghome/internal/nav"
	"github.com/vrocwang/ghome/internal/status"
	"github.com/vrocwang/ghome/internal/store"
)

func TestDisplayURL(t *testing.T) {
	cases := []struct {
		addr     string
		basePath string
		want     string
	}{
		{":8080", "", "http://127.0.0.1:8080/"},
		{"0.0.0.0:9000", "", "http://127.0.0.1:9000/"},
		{"127.0.0.1:8080", "/nav", "http://127.0.0.1:8080/nav/"},
		{"127.0.0.1:8080", "nav/", "http://127.0.0.1:8080/nav/"},
		{"127.0.0.1:8080", "/", "http://127.0.0.1:8080/"},
	}
	for _, c := range cases {
		if got := displayURL(c.addr, c.basePath); got != c.want {
			t.Errorf("displayURL(%q, %q) = %q, want %q", c.addr, c.basePath, got, c.want)
		}
	}
}

func TestEnvHelpers(t *testing.T) {
	if got := env("GHOME_DOES_NOT_EXIST", "fallback"); got != "fallback" {
		t.Errorf("env() = %q, want fallback", got)
	}
	t.Setenv("GHOME_TEST_VALUE", "set")
	if got := env("GHOME_TEST_VALUE", "fallback"); got != "set" {
		t.Errorf("env() = %q, want set", got)
	}

	if got := envDuration("GHOME_DOES_NOT_EXIST", 42*time.Second); got != 42*time.Second {
		t.Errorf("envDuration() = %v, want 42s", got)
	}

	t.Setenv("GHOME_TEST_DURATION", "90s")
	if got := envDuration("GHOME_TEST_DURATION", time.Second); got != 90*time.Second {
		t.Errorf("envDuration() = %v, want 90s", got)
	}

	t.Setenv("GHOME_TEST_DURATION", "not-a-duration")
	if got := envDuration("GHOME_TEST_DURATION", time.Second); got != time.Second {
		t.Errorf("envDuration() = %v, want the fallback for a bad value", got)
	}
}

func TestBuildSourceRejectsAnUnknownKind(t *testing.T) {
	if _, err := buildSource(options{sourceKind: "rsync"}); err == nil {
		t.Fatal("buildSource() accepted an unknown source kind")
	}
}

func TestBuildSourceGiteaNeedsCoordinates(t *testing.T) {
	if _, err := buildSource(options{sourceKind: "gitea"}); err == nil {
		t.Fatal("buildSource() accepted gitea without a url")
	}
}

func TestBuildSourceBuildsBothKinds(t *testing.T) {
	if _, err := buildSource(options{sourceKind: "file", file: "nav.yaml"}); err != nil {
		t.Errorf("file source: %v", err)
	}
	if _, err := buildSource(options{
		sourceKind: "gitea", forgeURL: "https://git.example.com",
		forgeOwner: "acme", forgeRepo: "nav",
	}); err != nil {
		t.Errorf("gitea source: %v", err)
	}
}

func TestEnabledLabel(t *testing.T) {
	if enabledLabel(true) != "enabled" || enabledLabel(false) != "disabled" {
		t.Error("enabledLabel() is wrong")
	}
}

func TestLooksLikeCommit(t *testing.T) {
	sha1Ref := strings.Repeat("0123456789abcdef", 2) + "01234567" // 40 chars
	cases := map[string]bool{
		"main":                   false,
		"v1.2.3":                 false,
		"feature/nav":            false,
		sha1Ref:                  true,  // full SHA-1
		strings.ToUpper(sha1Ref): true,  // upper case
		strings.Repeat("0", 64):  true,  // SHA-256
		sha1Ref[:39]:             false, // one short
		sha1Ref[:39] + "z":       false, // not hex
	}
	for ref, want := range cases {
		if got := looksLikeCommit(ref); got != want {
			t.Errorf("looksLikeCommit(%q) = %v, want %v", ref, got, want)
		}
	}
}

/* ---------- status prober supervisor ---------- */

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func parseCfg(t *testing.T, doc string) *nav.Config {
	t.Helper()
	cfg, _, err := nav.Parse([]byte(doc))
	if err != nil {
		t.Fatalf("nav.Parse() error = %v", err)
	}
	return cfg
}

func TestSameProbeOptions(t *testing.T) {
	base := status.Options{Interval: time.Minute, Timeout: 5 * time.Second, Concurrency: 8}

	if !sameProbeOptions(base, base) {
		t.Error("identical options compared unequal")
	}
	changed := base
	changed.Interval = 2 * time.Minute
	if sameProbeOptions(base, changed) {
		t.Error("a changed interval compared equal: the prober would keep the old cadence")
	}
}

// The supervisor exists so a cadence change in nav.yaml takes effect without
// throwing away results when nothing relevant changed.
func TestProberSupervisor(t *testing.T) {
	// Point the probe at a local server so the test never touches the network.
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	doc := func(interval string) string {
		return "status:\n  enabled: true\n  interval: " + interval +
			"\ngroups:\n  - name: x\n    links:\n      - name: svc\n        url: " + target.URL + "\n"
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	st := store.New()
	p := newProber(ctx, st, discardLogger())

	// Nothing applied yet: the HTTP layer must still get a usable answer.
	if p.Snapshot() == nil {
		t.Error("Snapshot() = nil before any config was applied")
	}
	if targets, _, _ := p.Stats(); targets != 0 {
		t.Errorf("Stats() targets = %d before any config, want 0", targets)
	}

	p.apply(parseCfg(t, doc("15s")))
	first := p.current
	if first == nil {
		t.Fatal("no prober was started")
	}

	// Same cadence: reuse, so in-flight results are not discarded.
	p.apply(parseCfg(t, doc("15s")))
	if p.current != first {
		t.Error("the prober was rebuilt even though its cadence did not change")
	}

	// Different cadence: rebuild.
	p.apply(parseCfg(t, doc("45s")))
	if p.current == first {
		t.Error("a changed interval did not produce a new prober")
	}

	// Disabling must leave nothing to probe.
	disabled := parseCfg(t, "status:\n  enabled: false\n  interval: 45s"+
		"\ngroups:\n  - name: x\n    links:\n      - name: svc\n        url: "+target.URL+"\n")
	p.apply(disabled)
	waitFor(t, 3*time.Second, "probe targets to be cleared", func() bool {
		targets, _, _ := p.Stats()
		return targets == 0
	})

	// stop() must be safe to call twice and must not panic.
	p.stop()
	p.stop()
}

func TestProberSupervisorUsesTheConfigCadence(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := newProber(ctx, store.New(), discardLogger())
	cfg := parseCfg(t, "status:\n  enabled: true\n  interval: 20s\n  timeout: 3s\n  concurrency: 4\n"+minimalNav)
	p.apply(cfg)

	if p.applied.Interval != 20*time.Second {
		t.Errorf("applied interval = %v, want the config's 20s", p.applied.Interval)
	}
	if p.applied.Timeout != 3*time.Second {
		t.Errorf("applied timeout = %v, want the config's 3s", p.applied.Timeout)
	}
	if p.applied.Concurrency != 4 {
		t.Errorf("applied concurrency = %d, want the config's 4", p.applied.Concurrency)
	}
	p.stop()
}

// waitFor is a small polling helper shared by the supervisor tests.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

const minimalNav = "groups:\n  - name: x\n    links:\n      - name: a\n        url: https://a.example.com\n"

/* ---------- --icon-dir startup diagnostics ---------- */

// A typo'd --icon-dir used to fail in complete silence: every lookup just fell
// through to the network, so the only symptom was "why are my icons wrong".
func TestReportIconDirSaysWhatWillHappen(t *testing.T) {
	capture := func(dir string) string {
		t.Helper()
		var buf bytes.Buffer
		reportIconDir(dir, slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
		return buf.String()
	}

	t.Run("unset says nothing", func(t *testing.T) {
		if got := capture(""); got != "" {
			t.Errorf("reportIconDir(\"\") logged %q, want silence", got)
		}
	})

	t.Run("missing directory warns", func(t *testing.T) {
		got := capture(filepath.Join(t.TempDir(), "nope"))
		if !strings.Contains(got, "WARN") || !strings.Contains(got, "unusable") {
			t.Errorf("reportIconDir() logged %q, want a warning about an unusable path", got)
		}
	})

	t.Run("empty directory warns that it has no effect", func(t *testing.T) {
		got := capture(t.TempDir())
		if !strings.Contains(got, "WARN") || !strings.Contains(got, "no usable icon files") {
			t.Errorf("reportIconDir() logged %q, want a warning about an empty directory", got)
		}
	})

	t.Run("a working directory reports its count", func(t *testing.T) {
		dir := t.TempDir()
		for _, name := range []string{"a.test.svg", "b.test.png"} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte("<svg/>"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		got := capture(dir)
		if !strings.Contains(got, "ready") || !strings.Contains(got, "icons=2") {
			t.Errorf("reportIconDir() logged %q, want the icon count", got)
		}
	})
}

/* ---------- 命令行：命名与别名 ---------- */

func TestIsForge(t *testing.T) {
	for _, kind := range []string{"gitea", "forgejo", "Gitea", " FORGEJO "} {
		if !isForge(kind) {
			t.Errorf("isForge(%q) = false, want true", kind)
		}
	}
	for _, kind := range []string{"", "file", "gitlab"} {
		if isForge(kind) {
			t.Errorf("isForge(%q) = true, want false", kind)
		}
	}
}

// The aliases exist so existing deployments keep working, but showing two names
// for every flag would make --help harder to read than the rename is worth.
// Every alias has to name a real flag: registration looks the target up and
// dereferences it, so a stale entry would panic on startup rather than degrade
// quietly. This pins the mapping itself.

/* ---------- 容器健康检查 ---------- */

// A scratch image has no shell and no curl, so `ghome --healthcheck` is the only
// way to probe the process from inside.
func TestRunHealthcheck(t *testing.T) {
	serve := func(code int) (*httptest.Server, options) {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/healthz" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(code)
			if code != http.StatusOK {
				_, _ = w.Write([]byte(`{"status":"error"}`))
			}
		}))
		t.Cleanup(srv.Close)
		// Point --listen at the test server so the derived URL hits it.
		return srv, options{listen: strings.TrimPrefix(srv.URL, "http://")}
	}

	t.Run("200 is healthy", func(t *testing.T) {
		_, o := serve(http.StatusOK)
		if err := runHealthcheck(o); err != nil {
			t.Errorf("runHealthcheck() = %v, want nil", err)
		}
	})

	// 503 means "running but no usable config yet". That must not read as
	// healthy, or a container would sit there serving nothing.
	t.Run("503 is unhealthy", func(t *testing.T) {
		_, o := serve(http.StatusServiceUnavailable)
		err := runHealthcheck(o)
		if err == nil {
			t.Fatal("runHealthcheck() = nil, want an error for 503")
		}
		if !strings.Contains(err.Error(), "503") {
			t.Errorf("error = %v, want it to name the status", err)
		}
	})

	t.Run("nothing listening is unhealthy", func(t *testing.T) {
		err := runHealthcheck(options{listen: "127.0.0.1:1"})
		if err == nil {
			t.Error("runHealthcheck() = nil against a closed port")
		}
	})

	t.Run("a base path is respected", func(t *testing.T) {
		var got string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got = r.URL.Path
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()
		o := options{listen: strings.TrimPrefix(srv.URL, "http://"), basePath: "/nav"}
		if err := runHealthcheck(o); err != nil {
			t.Fatalf("runHealthcheck() = %v", err)
		}
		if got != "/nav/healthz" {
			t.Errorf("probed %q, want /nav/healthz", got)
		}
	})
}

/* ---------- 配置面与文档的一致性 ---------- */

// deploy/ghome.env.example is the only place an operator sees the whole
// configuration surface at once, and it has silently drifted before: the token
// assignment went missing while its explanatory comment stayed behind, which
// reads as complete at a glance.
//
// So the check is for an *assignment*, not a mention. The first version of this
// test looked for the name anywhere and happily passed on exactly that bug.
func TestEnvExampleMatchesTheConfigurationSurface(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	example, err := os.ReadFile("deploy/ghome.env.example")
	if err != nil {
		t.Fatalf("read deploy/ghome.env.example: %v", err)
	}

	declared := map[string]bool{}
	for _, m := range regexp.MustCompile(`"(GHOME_[A-Z_]+)"`).FindAllStringSubmatch(string(src), -1) {
		declared[m[1]] = true
	}

	// `# NAME=value` counts: that is how optional settings are shown. A line
	// that merely names the variable does not.
	assigned := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^#?\s*(GHOME_[A-Z_]+)\s*=`).FindAllStringSubmatch(string(example), -1) {
		assigned[m[1]] = true
	}

	// Without these, a broken pattern would make the test pass forever.
	if len(declared) < 15 {
		t.Fatalf("found only %d GHOME_* variables in main.go; the pattern is wrong", len(declared))
	}
	if len(assigned) < 15 {
		t.Fatalf("found only %d assignments in the env example; the pattern is wrong", len(assigned))
	}

	// Every variable the program reads must be shown being set.
	for _, name := range slices.Sorted(maps.Keys(declared)) {
		if !assigned[name] {
			t.Errorf("deploy/ghome.env.example documents %s but never assigns it", name)
		}
	}
	// And nothing stale may survive a rename.
	for _, name := range slices.Sorted(maps.Keys(assigned)) {
		if !declared[name] {
			t.Errorf("deploy/ghome.env.example sets %s, which nothing reads", name)
		}
	}
}
