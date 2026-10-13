package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Every open stream costs a goroutine and a socket, and the endpoint is
// reachable by anyone who can reach the page. Past the cap the answer is a
// refusal, not another stream.
func TestEventStreamsAreCapped(t *testing.T) {
	srv, _ := newTestServer(t, Options{Store: testStore(t)})
	// Pretend the cap is already reached rather than opening 256 sockets.
	srv.streams.Store(maxEventStreams)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/events", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d past the cap", rec.Code, http.StatusServiceUnavailable)
	}
	if srv.streams.Load() != maxEventStreams {
		t.Errorf("streams = %d, want the refused connection not to be counted", srv.streams.Load())
	}
}

// http.Server.Shutdown does not cancel in-flight request contexts, and a stream
// is never idle, so a stream that is not asked to stop makes every shutdown wait
// out the whole grace period and report a timeout.
func TestCloseEventStreamsEndsOpenStreams(t *testing.T) {
	srv, _ := newTestServer(t, Options{Store: testStore(t)})
	httpSrv := httptest.NewServer(srv)
	defer httpSrv.Close()

	resp, err := http.Get(httpSrv.URL + "/api/events")
	if err != nil {
		t.Fatalf("GET /api/events: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	srv.CloseEventStreams()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(io.Discard, resp.Body)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the stream did not end after CloseEventStreams")
	}
	if got := srv.streams.Load(); got != 0 {
		t.Errorf("streams = %d after the stream ended, want 0", got)
	}

	// Closing twice must be safe: shutdown paths run more than once in practice.
	srv.CloseEventStreams()
}

// A listing only advertises what happens to be on disk, and nothing in the UI
// asks for a directory.
func TestAssetsDirectoryIsNotListed(t *testing.T) {
	srv, _ := newTestServer(t, Options{Store: testStore(t)})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/assets/", nil))

	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /assets/ = %d, want 404", rec.Code)
	}
	if body := rec.Body.String(); strings.Contains(body, "app.js") {
		t.Errorf("the directory was listed: %s", body)
	}
}

// A config may name an image url for a link icon or the site logo. Restricting
// img-src to 'self' silently turned those into broken images.
func TestCSPAllowsConfiguredImageURLs(t *testing.T) {
	srv, _ := newTestServer(t, Options{Store: testStore(t)})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	csp := rec.Header().Get("Content-Security-Policy")
	for _, want := range []string{"img-src 'self' data: http: https:"} {
		if !strings.Contains(csp, want) {
			t.Errorf("Content-Security-Policy = %q, want it to contain %q", csp, want)
		}
	}
	// The parts that must not have loosened.
	for _, want := range []string{"default-src 'self'", "script-src 'self'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("Content-Security-Policy lost %q: %q", want, csp)
		}
	}
}
