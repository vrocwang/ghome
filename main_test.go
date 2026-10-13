package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.xhupo.com/simon/ghome/internal/nav"
	"git.xhupo.com/simon/ghome/internal/status"
	"git.xhupo.com/simon/ghome/internal/store"
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
		sourceKind: "gitea", giteaURL: "https://git.example.com",
		giteaOwner: "simon", giteaRepo: "nav",
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
