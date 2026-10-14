package status

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// probeOnce runs a single round outside Run. Run only exposes rounds through a
// timer whose floor is five seconds, which would make classification tests
// crawl; the round itself is the unit that decides what a link's state is.
func probeOnce(p *Prober) {
	p.round(context.Background(), p.newRound())
}

// waitFor polls cond until it holds, failing the test if that takes too long.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met within 3s")
}

func TestProbeClassifiesStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/ok", http.StatusFound)
			return
		}
		if r.URL.Path == "/ok" {
			w.WriteHeader(http.StatusOK)
			return
		}
		code, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/"))
		if err != nil {
			code = http.StatusNotFound
		}
		w.WriteHeader(code)
	}))
	defer srv.Close()

	cases := []struct {
		name  string
		path  string
		state State
		code  int
	}{
		{"200 is up", "/200", StateUp, http.StatusOK},
		{"204 is up", "/204", StateUp, http.StatusNoContent},
		{"304 is up", "/304", StateUp, http.StatusNotModified},
		{"redirect to 200 is up", "/redirect", StateUp, http.StatusOK},
		{"401 is up: the service answered", "/401", StateUp, http.StatusUnauthorized},
		{"403 is up: the service answered", "/403", StateUp, http.StatusForbidden},
		{"404 is down", "/404", StateDown, http.StatusNotFound},
		{"418 is down", "/418", StateDown, http.StatusTeapot},
		{"500 is down", "/500", StateDown, http.StatusInternalServerError},
		{"503 is down", "/503", StateDown, http.StatusServiceUnavailable},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target := srv.URL + tc.path
			p := New(Options{Client: srv.Client()})
			p.SetTargets([]string{target})
			probeOnce(p)

			res, ok := p.Results()[target]
			if !ok {
				t.Fatalf("no result recorded for %s", target)
			}
			if res.State != tc.state {
				t.Fatalf("state = %q, want %q (code %d, error %q)", res.State, tc.state, res.Code, res.Error)
			}
			if res.Code != tc.code {
				t.Fatalf("code = %d, want %d", res.Code, tc.code)
			}
			if res.CheckedAt.IsZero() {
				t.Error("CheckedAt was left zero")
			}
			if res.LatencyMS < 0 {
				t.Errorf("LatencyMS = %d, want >= 0", res.LatencyMS)
			}
		})
	}
}

func TestHeadRejectedFallsBackToGet(t *testing.T) {
	for _, code := range []int{http.StatusBadRequest, http.StatusMethodNotAllowed, http.StatusNotImplemented} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			var head, get atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodHead {
					head.Add(1)
					w.WriteHeader(code)
					return
				}
				get.Add(1)
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("ok"))
			}))
			defer srv.Close()

			p := New(Options{Client: srv.Client()})
			p.SetTargets([]string{srv.URL})
			probeOnce(p)

			res := p.Results()[srv.URL]
			if res.State != StateUp || res.Code != http.StatusOK {
				t.Fatalf("state = %q, code = %d, error = %q; want up/200 after the GET fallback",
					res.State, res.Code, res.Error)
			}
			if head.Load() != 1 || get.Load() != 1 {
				t.Fatalf("HEAD = %d, GET = %d; want exactly one of each", head.Load(), get.Load())
			}
		})
	}
}

func TestHeadTransportFailureFallsBackToGet(t *testing.T) {
	var get atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			// Drop the connection on HEAD the way a server that cannot answer
			// HEAD sometimes does, so the client sees a transport error.
			hj, ok := w.(http.Hijacker)
			if !ok {
				return
			}
			conn, _, err := hj.Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		get.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := New(Options{Client: srv.Client()})
	p.SetTargets([]string{srv.URL})
	probeOnce(p)

	res := p.Results()[srv.URL]
	if res.State != StateUp || res.Code != http.StatusOK {
		t.Fatalf("state = %q, code = %d, error = %q; want up/200 after the GET retry",
			res.State, res.Code, res.Error)
	}
	if got := get.Load(); got != 1 {
		t.Fatalf("GET called %d times, want 1", got)
	}
}

func TestProbeTimeoutIsDown(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-time.After(250 * time.Millisecond):
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer func() {
		close(release)
		srv.Close()
	}()

	p := New(Options{Client: srv.Client(), Timeout: 30 * time.Millisecond})
	p.SetTargets([]string{srv.URL})
	probeOnce(p)

	res := p.Results()[srv.URL]
	if res.State != StateDown {
		t.Fatalf("state = %q, want down for a server slower than the timeout", res.State)
	}
	if res.Error == "" {
		t.Error("a timeout must record an error")
	}
	if res.Code != 0 {
		t.Errorf("code = %d, want 0 when nothing answered", res.Code)
	}
}

func TestRedirectLoopIsDown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/loop", http.StatusFound)
	}))
	defer srv.Close()

	// No Client option, so this exercises the transport New builds itself.
	p := New(Options{})
	p.SetTargets([]string{srv.URL})
	probeOnce(p)

	res := p.Results()[srv.URL]
	if res.State != StateDown {
		t.Fatalf("state = %q, want down for a redirect loop", res.State)
	}
	if res.Error == "" {
		t.Error("a redirect loop must record an error")
	}
}

// endlessBody can always supply more bytes and counts how many were read, so a
// test can prove a huge page is never downloaded in full.
type endlessBody struct{ read atomic.Int64 }

func (b *endlessBody) Read(p []byte) (int, error) {
	b.read.Add(int64(len(p)))
	return len(p), nil
}

func (b *endlessBody) Close() error { return nil }

// endlessTransport rejects HEAD and answers GET with an endless body.
type endlessTransport struct{ body *endlessBody }

func (t *endlessTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	header := make(http.Header)
	if req.Method == http.MethodHead {
		return &http.Response{
			StatusCode: http.StatusMethodNotAllowed,
			Header:     header,
			Body:       http.NoBody,
			Request:    req,
		}, nil
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     header,
		Body:       t.body,
		Request:    req,
	}, nil
}

func TestProbeReadsAtMostAFewKB(t *testing.T) {
	body := &endlessBody{}
	p := New(Options{Client: &http.Client{Transport: &endlessTransport{body: body}}})
	p.SetTargets([]string{"http://status.example.test/"})
	probeOnce(p)

	res := p.Results()["http://status.example.test/"]
	if res.State != StateUp || res.Code != http.StatusOK {
		t.Fatalf("state = %q, code = %d, error = %q; want up/200", res.State, res.Code, res.Error)
	}
	if got := body.read.Load(); got != maxBodyBytes {
		t.Fatalf("read %d bytes of the body, want exactly %d", got, maxBodyBytes)
	}
}

func TestSetTargetsFiltersAndDedupes(t *testing.T) {
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := New(Options{Client: srv.Client()})
	p.SetTargets([]string{
		srv.URL,
		srv.URL,               // duplicate
		"  " + srv.URL + "  ", // duplicate after trimming
		"mailto:ops@example.com",
		"tel:+15551234",
		"ssh://git@example.com",
		"vscode://file/tmp",
		"ftp://example.com",
		"http://[::1", // malformed
		"",
	})

	if targets, up, down := p.Stats(); targets != 1 || up != 0 || down != 0 {
		t.Fatalf("Stats() = %d/%d/%d, want 1/0/0", targets, up, down)
	}
	probeOnce(p)
	if got := requests.Load(); got != 1 {
		t.Fatalf("server saw %d requests, want 1 (only the http link)", got)
	}

	results := p.Results()
	if len(results) != 1 {
		t.Fatalf("Results() has %d entries, want 1: %v", len(results), results)
	}
	for _, skipped := range []string{"mailto:ops@example.com", "tel:+15551234", "ssh://git@example.com", "vscode://file/tmp"} {
		if _, ok := results[skipped]; ok {
			t.Errorf("%s should never be probed or reported", skipped)
		}
	}
	if res := results[srv.URL]; res.State != StateUp {
		t.Errorf("state = %q, want up", res.State)
	}
}

func TestRoundStopsLaunchingWhenCancelled(t *testing.T) {
	release := make(chan struct{})
	var started atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started.Add(1)
		select {
		case <-release:
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer func() {
		close(release)
		srv.Close()
	}()

	// Concurrency 1 with several targets parks the launch loop on the
	// semaphore, so cancelling mid-round has to break out of it.
	p := New(Options{Client: srv.Client(), Timeout: time.Minute, Concurrency: 1})
	targets := make([]string, 5)
	for i := range targets {
		targets[i] = fmt.Sprintf("%s/?probe=%d", srv.URL, i)
	}
	p.SetTargets(targets)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.round(ctx, p.newRound())
		close(done)
	}()

	waitFor(t, func() bool { return started.Load() >= 1 })
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("round did not return after the context was cancelled")
	}

	// A round cut short must not invent failures: unprobed links stay unknown.
	results := p.Results()
	if len(results) != len(targets) {
		t.Fatalf("Results() has %d entries, want %d", len(results), len(targets))
	}
	for _, res := range results {
		if res.State != StateUnknown {
			t.Fatalf("%s reported %q after a cancelled round, want unknown", res.URL, res.State)
		}
	}
}

func TestMalformedTargetDegradesCleanly(t *testing.T) {
	p := New(Options{})
	for _, target := range []string{"http://[::1", "http://", "://nonsense"} {
		res := p.probe(context.Background(), target)
		if res.State != StateDown {
			t.Errorf("probe(%q).State = %q, want down", target, res.State)
		}
		if res.Error == "" {
			t.Errorf("probe(%q) recorded no error", target)
		}
		if res.Code != 0 {
			t.Errorf("probe(%q).Code = %d, want 0", target, res.Code)
		}
	}
}

func TestStatsCounts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/down" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := New(Options{Client: srv.Client()})
	p.SetTargets([]string{srv.URL + "/up", srv.URL + "/down", "mailto:ops@example.com"})

	if targets, up, down := p.Stats(); targets != 2 || up != 0 || down != 0 {
		t.Fatalf("before a round: %d/%d/%d, want 2/0/0", targets, up, down)
	}
	probeOnce(p)
	if targets, up, down := p.Stats(); targets != 2 || up != 1 || down != 1 {
		t.Fatalf("after a round: %d/%d/%d, want 2/1/1", targets, up, down)
	}
}

func TestResultsReturnsACopy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := New(Options{Client: srv.Client()})
	p.SetTargets([]string{srv.URL})
	probeOnce(p)

	got := p.Results()
	got["injected"] = Result{URL: "injected", State: StateUp}
	delete(got, srv.URL)

	if again := p.Results(); len(again) != 1 || again[srv.URL].State != StateUp {
		t.Fatalf("Results() must hand out a copy, got %v", again)
	}
}

func TestRunBroadcastsOnRealChangeOnly(t *testing.T) {
	var fail atomic.Bool
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := New(Options{Client: srv.Client()})
	p.interval = 20 * time.Millisecond // below the floor on purpose: keep the test fast

	p.SetTargets([]string{srv.URL})

	var calls atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.Run(ctx, func() { calls.Add(1) })
		close(done)
	}()

	// The first round is a change: nobody has heard a state yet.
	waitFor(t, func() bool { return calls.Load() == 1 })

	// Several identical rounds must not broadcast again, even though they did
	// run and probed the link each time.
	waitFor(t, func() bool { return requests.Load() >= 5 })
	time.Sleep(3 * p.interval)
	if got := calls.Load(); got != 1 {
		t.Fatalf("onChange fired %d times for identical rounds, want 1", got)
	}

	fail.Store(true)
	waitFor(t, func() bool { return calls.Load() == 2 })
	if res := p.Results()[srv.URL]; res.State != StateDown {
		t.Fatalf("state = %q, want down after the server started failing", res.State)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after the context was cancelled")
	}
}

func TestRunProbesImmediatelyAndLogs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	p := New(Options{Client: srv.Client(), Logger: logger, Interval: time.Hour})
	p.SetTargets([]string{srv.URL})

	var calls atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.Run(ctx, func() { calls.Add(1) })
		close(done)
	}()

	// With an hour-long interval, only the immediate round can explain a call.
	waitFor(t, func() bool { return calls.Load() == 1 })
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after the context was cancelled")
	}

	if got := calls.Load(); got != 1 {
		t.Fatalf("onChange fired %d times, want 1", got)
	}
	if !strings.Contains(buf.String(), "service status changed") {
		t.Errorf("round summary was not logged, got %q", buf.String())
	}
}

func TestRunReturnsPromptlyOnCancel(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Block longer than the timeout, so the round is still in flight when
		// the context is cancelled.
		select {
		case <-release:
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer func() {
		close(release)
		srv.Close()
	}()

	p := New(Options{Client: srv.Client(), Timeout: time.Minute})
	p.SetTargets([]string{srv.URL})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.Run(ctx, nil) // a nil callback must be fine
		close(done)
	}()

	time.Sleep(30 * time.Millisecond) // let the first round get in flight
	start := time.Now()
	cancel()

	select {
	case <-done:
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("Run took %v to return after cancel, want promptly", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after the context was cancelled")
	}
}

func TestRunWithCancelledContextReturnsAtOnce(t *testing.T) {
	p := New(Options{})
	p.SetTargets([]string{"http://example.test/"})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		p.Run(ctx, func() { t.Error("onChange must not fire on a cancelled context") })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return for an already cancelled context")
	}
}

func TestRunWithNoTargetsNeverBroadcasts(t *testing.T) {
	p := New(Options{})
	p.interval = 5 * time.Millisecond

	var calls atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.Run(ctx, func() { calls.Add(1) })
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after the context was cancelled")
	}

	if got := calls.Load(); got != 0 {
		t.Fatalf("onChange fired %d times with no targets, want 0", got)
	}
	if targets, up, down := p.Stats(); targets != 0 || up != 0 || down != 0 {
		t.Fatalf("Stats() = %d/%d/%d, want 0/0/0", targets, up, down)
	}
	if got := len(p.Results()); got != 0 {
		t.Fatalf("Results() has %d entries, want 0", got)
	}
}

func TestConcurrentSetTargetsResultsAndRun(t *testing.T) {
	var slow atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if slow.Load() {
			time.Sleep(5 * time.Millisecond)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := New(Options{Client: srv.Client(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	p.interval = 5 * time.Millisecond
	p.SetTargets([]string{srv.URL})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.Run(ctx, func() {})
		close(done)
	}()

	var wg sync.WaitGroup
	for i := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 50 {
				p.SetTargets([]string{
					srv.URL,
					fmt.Sprintf("%s/?probe=%d", srv.URL, j),
					"mailto:ops@example.com",
				})
				results := p.Results()
				for _, r := range results {
					_ = r.State
				}
				_, _, _ = p.Stats()
				_ = p.Snapshot(nil)
			}
			_ = i
		}()
	}
	wg.Wait()
	slow.Store(true)
	time.Sleep(10 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after the context was cancelled")
	}
}

func TestNewDefaultsAndClamps(t *testing.T) {
	if minInterval != 5*time.Second {
		t.Fatalf("minInterval = %v, want 5s", minInterval)
	}

	cases := []struct {
		name            string
		opts            Options
		wantInterval    time.Duration
		wantTimeout     time.Duration
		wantConcurrency int
	}{
		{"zero value", Options{}, defaultInterval, defaultTimeout, defaultConcurrency},
		{"tiny interval is raised to the floor", Options{Interval: time.Millisecond}, minInterval, defaultTimeout, defaultConcurrency},
		{"interval just under the floor is raised", Options{Interval: minInterval - time.Millisecond}, minInterval, defaultTimeout, defaultConcurrency},
		{"interval above the floor is kept", Options{Interval: 90 * time.Second}, 90 * time.Second, defaultTimeout, defaultConcurrency},
		{"negative interval falls back", Options{Interval: -time.Second}, defaultInterval, defaultTimeout, defaultConcurrency},
		{"timeout falls back", Options{Timeout: -time.Second}, defaultInterval, defaultTimeout, defaultConcurrency},
		{"timeout is kept", Options{Timeout: 2 * time.Second}, defaultInterval, 2 * time.Second, defaultConcurrency},
		{"concurrency falls back", Options{Concurrency: -3}, defaultInterval, defaultTimeout, defaultConcurrency},
		{"concurrency is capped", Options{Concurrency: 1000}, defaultInterval, defaultTimeout, maxConcurrency},
		{"concurrency is kept", Options{Concurrency: 3}, defaultInterval, defaultTimeout, 3},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := New(tc.opts)
			if p.interval != tc.wantInterval {
				t.Errorf("interval = %v, want %v", p.interval, tc.wantInterval)
			}
			if p.timeout != tc.wantTimeout {
				t.Errorf("timeout = %v, want %v", p.timeout, tc.wantTimeout)
			}
			if p.concurrency != tc.wantConcurrency {
				t.Errorf("concurrency = %d, want %d", p.concurrency, tc.wantConcurrency)
			}
			if p.log == nil {
				t.Error("logger must never be nil")
			}
			if p.client == nil {
				t.Error("client must never be nil")
			}
		})
	}

	custom := &http.Client{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	p := New(Options{Client: custom, Logger: logger})
	if p.client != custom {
		t.Error("a provided client must be used as is")
	}
	if p.log != logger {
		t.Error("a provided logger must be used as is")
	}
}
