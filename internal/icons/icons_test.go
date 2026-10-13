package icons

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The byte slices below carry real magic numbers so http.DetectContentType
// recognises them, which is what the sniffing tests exercise.
var (
	pngBytes = append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0x90}, 40)...)
	gifBytes = append([]byte("GIF89a"), bytes.Repeat([]byte{0x11}, 24)...)
	icoBytes = append([]byte{0x00, 0x00, 0x01, 0x00}, bytes.Repeat([]byte{0x22}, 24)...)
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testResolver(t *testing.T, opts Options) *Resolver {
	t.Helper()
	if opts.Logger == nil {
		opts.Logger = discardLogger()
	}
	return New(opts)
}

// site is a fake upstream that records every path it was asked for.
type site struct {
	*httptest.Server

	mu    sync.Mutex
	paths []string
}

func newSite(t *testing.T, routes map[string]http.HandlerFunc) *site {
	t.Helper()

	s := &site{}
	mux := http.NewServeMux()
	for pattern, h := range routes {
		handler := h
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			s.record(r.URL.Path)
			handler(w, r)
		})
	}
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Server.Close)
	return s
}

func (s *site) record(path string) {
	s.mu.Lock()
	s.paths = append(s.paths, path)
	s.mu.Unlock()
}

func (s *site) hits(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, p := range s.paths {
		if p == path {
			n++
		}
	}
	return n
}

func (s *site) total() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.paths)
}

func (s *site) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.paths...)
}

// page serves an HTML document, for both the page and any icon route.
func page(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, body)
	}
}

// image serves icon bytes with an explicit media type.
func image(data []byte, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write(data)
	}
}

func TestLookupResolvesIconLink(t *testing.T) {
	s := newSite(t, map[string]http.HandlerFunc{
		"/":      page(`<html><head><link rel="icon" href="/a.png"></head><body>ok</body></html>`),
		"/a.png": image(pngBytes, "image/png"),
	})
	r := testResolver(t, Options{})
	r.Allow([]string{s.URL})

	icon, ok := r.Lookup(context.Background(), s.URL)
	if !ok {
		t.Fatalf("Lookup() = false, want the linked icon (paths: %v)", s.seen())
	}
	if !bytes.Equal(icon.Data, pngBytes) {
		t.Errorf("Lookup() data = %q, want the PNG body", icon.Data)
	}
	if icon.ContentType != "image/png" {
		t.Errorf("Lookup() content type = %q, want image/png", icon.ContentType)
	}

	// A second lookup must come from the cache.
	before := s.total()
	if _, ok := r.Lookup(context.Background(), s.URL); !ok {
		t.Fatal("second Lookup() = false")
	}
	if s.total() != before {
		t.Errorf("second Lookup() hit the network again: %v", s.seen())
	}
	if _, hits, _ := r.Stats(); hits == 0 {
		t.Error("Stats() reported no cache hits")
	}
}

func TestLookupResolvesRelativeHrefAgainstBase(t *testing.T) {
	s := newSite(t, map[string]http.HandlerFunc{
		"/":             page(`<html><head><base href="/static/"><link rel="icon" href="b.png"></head></html>`),
		"/static/b.png": image(gifBytes, "image/gif"),
	})
	r := testResolver(t, Options{})
	r.Allow([]string{s.URL})

	icon, ok := r.Lookup(context.Background(), s.URL)
	if !ok {
		t.Fatalf("Lookup() = false, want the icon under <base> (paths: %v)", s.seen())
	}
	if !bytes.Equal(icon.Data, gifBytes) {
		t.Errorf("Lookup() data = %q, want the GIF under /static/", icon.Data)
	}
	if got := s.hits("/static/b.png"); got != 1 {
		t.Errorf("requests for /static/b.png = %d, want 1 (paths: %v)", got, s.seen())
	}
}

func TestLookupPrefersLargestSizes(t *testing.T) {
	small := append([]byte("\x89PNG\r\n\x1a\n"), []byte("small")...)
	big := append([]byte("\x89PNG\r\n\x1a\n"), []byte("big")...)

	s := newSite(t, map[string]http.HandlerFunc{
		"/": page(`<html><head>
			<link rel="icon" sizes="32x32" href="/small.png">
			<link rel="apple-touch-icon" sizes="192x192" href="/big.png">
			<link rel="icon" sizes="16x16" href="/small.png">
		</head></html>`),
		"/small.png": image(small, "image/png"),
		"/big.png":   image(big, "image/png"),
	})
	r := testResolver(t, Options{})
	r.Allow([]string{s.URL})

	icon, ok := r.Lookup(context.Background(), s.URL)
	if !ok {
		t.Fatalf("Lookup() = false (paths: %v)", s.seen())
	}
	if !bytes.Equal(icon.Data, big) {
		t.Errorf("Lookup() picked %q, want the 192x192 icon", icon.Data)
	}
	if got := s.hits("/small.png"); got != 0 {
		t.Errorf("requests for the smaller icon = %d, want 0", got)
	}
}

func TestLookupFallsBackToFaviconIco(t *testing.T) {
	s := newSite(t, map[string]http.HandlerFunc{
		"/":            page(`<html><head><title>no icons here</title></head><body>x</body></html>`),
		"/favicon.ico": image(icoBytes, "image/x-icon"),
	})
	r := testResolver(t, Options{})
	r.Allow([]string{s.URL})

	icon, ok := r.Lookup(context.Background(), s.URL)
	if !ok {
		t.Fatalf("Lookup() = false, want the /favicon.ico fallback (paths: %v)", s.seen())
	}
	if !bytes.Equal(icon.Data, icoBytes) {
		t.Errorf("Lookup() data = %q, want the ICO body", icon.Data)
	}
	if got := s.hits("/favicon.ico"); got != 1 {
		t.Errorf("requests for /favicon.ico = %d, want 1", got)
	}
}

func TestLookupRejectsNonImage(t *testing.T) {
	s := newSite(t, map[string]http.HandlerFunc{
		"/":      page(`<link rel="icon" href="/a.png">`),
		"/a.png": page(`<html><body>this is an error page</body></html>`),
	})
	r := testResolver(t, Options{})
	r.Allow([]string{s.URL})

	icon, ok := r.Lookup(context.Background(), s.URL)
	if ok {
		t.Fatalf("Lookup() = true for a text/html icon, want false")
	}
	if icon.Data != nil || icon.ContentType != "" {
		t.Errorf("Lookup() = %+v, want the zero Icon", icon)
	}
	// A link was found, so the conventional path is not tried as well.
	if got := s.hits("/favicon.ico"); got != 0 {
		t.Errorf("requests for /favicon.ico = %d, want 0 after a matched link failed", got)
	}
}

func TestLookupRefusesUnallowedHost(t *testing.T) {
	s := newSite(t, map[string]http.HandlerFunc{
		"/":       page(`<link rel="icon" href="/a.png">`),
		"/a.png":  image(pngBytes, "image/png"),
		"/f.ico":  image(icoBytes, "image/x-icon"),
		"/safe":   page(`<link rel="icon" href="/a.png">`),
		"/unused": page(`<link rel="icon" href="/a.png">`),
	})
	r := testResolver(t, Options{})
	r.Allow([]string{s.URL})

	// httptest listens on 127.0.0.1; "localhost" is the same machine but a
	// different host name, so it was never registered.
	other := strings.Replace(s.URL, "127.0.0.1", "localhost", 1)
	if other == s.URL {
		t.Fatalf("test setup: could not build an alternative host from %q", s.URL)
	}

	if _, ok := r.Lookup(context.Background(), other); ok {
		t.Errorf("Lookup(%q) = true for a host that was never allowed", other)
	}
	if s.total() != 0 {
		t.Errorf("an unallowed host was contacted: %v", s.seen())
	}

	if _, ok := r.Lookup(context.Background(), s.URL); !ok {
		t.Errorf("Lookup(%q) = false for an allowed host (paths: %v)", s.URL, s.seen())
	}
}

func TestAllowNormalisesHosts(t *testing.T) {
	r := testResolver(t, Options{})
	r.Allow([]string{
		"HTTP://Example.COM:8443/nav",
		"https://example.com",
		"ftp://files.example.com",
		"not a url",
		"http://",
	})

	for _, host := range []string{"example.com", "EXAMPLE.com"} {
		if !r.isAllowed(host) {
			t.Errorf("isAllowed(%q) = false, want true", host)
		}
	}
	for _, host := range []string{"files.example.com", "other.com", ""} {
		if r.isAllowed(host) {
			t.Errorf("isAllowed(%q) = true, want false", host)
		}
	}

	// Allow is called on every config load and replaces the previous list: a
	// link that left nav.yaml must stop being reachable.
	r.Allow([]string{"https://other.com"})
	if r.isAllowed("example.com") {
		t.Error("Allow did not replace the previous allow list")
	}
}

func TestLookupIgnoresPortInAllowList(t *testing.T) {
	s := newSite(t, map[string]http.HandlerFunc{
		"/":      page(`<link rel="icon" href="/a.png">`),
		"/a.png": image(pngBytes, "image/png"),
	})
	r := testResolver(t, Options{})
	// Registered with an unrelated port: http://x:3000 and http://x are the
	// same site.
	r.Allow([]string{"http://127.0.0.1:1/"})

	if _, ok := r.Lookup(context.Background(), s.URL); !ok {
		t.Errorf("Lookup(%q) = false, want the allowed host (paths: %v)", s.URL, s.seen())
	}
}

func TestLookupUsesLocalDirWithoutNetwork(t *testing.T) {
	s := newSite(t, map[string]http.HandlerFunc{
		"/":      page(`<link rel="icon" href="/a.png">`),
		"/a.png": image(pngBytes, "image/png"),
	})

	dir := t.TempDir()
	local := []byte(`<svg xmlns="http://www.w3.org/2000/svg"><rect/></svg>`)
	if err := os.WriteFile(filepath.Join(dir, "127.0.0.1.svg"), local, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	r := testResolver(t, Options{Dir: dir})
	r.Allow([]string{s.URL})

	icon, ok := r.Lookup(context.Background(), s.URL)
	if !ok {
		t.Fatal("Lookup() = false, want the local icon")
	}
	if !bytes.Equal(icon.Data, local) {
		t.Errorf("Lookup() data = %q, want the local SVG", icon.Data)
	}
	if icon.ContentType != "image/svg+xml" {
		t.Errorf("Lookup() content type = %q, want image/svg+xml", icon.ContentType)
	}
	if s.total() != 0 {
		t.Errorf("a local icon still hit the network: %v", s.seen())
	}
	if _, hits, _ := r.Stats(); hits == 0 {
		t.Error("Stats() reported no hits for a locally served icon")
	}
}

func TestLookupIgnoresOversizedLocalIcon(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "127.0.0.1.png"), bytes.Repeat([]byte{0x01}, 4096), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	s := newSite(t, map[string]http.HandlerFunc{
		"/":      page(`<link rel="icon" href="/a.png">`),
		"/a.png": image(pngBytes, "image/png"),
	})
	r := testResolver(t, Options{Dir: dir, MaxBytes: 1024})
	r.Allow([]string{s.URL})

	// The local file is past MaxBytes, so it is ignored and the site is asked.
	icon, ok := r.Lookup(context.Background(), s.URL)
	if !ok {
		t.Fatal("Lookup() = false, want the fetched icon")
	}
	if !bytes.Equal(icon.Data, pngBytes) {
		t.Errorf("Lookup() served the oversized local file")
	}
	if s.total() == 0 {
		t.Error("the network was never consulted")
	}
}

func TestLookupIgnoresTraversalInHost(t *testing.T) {
	dir := t.TempDir()
	// A file one level up must never be reachable through a crafted host.
	if err := os.WriteFile(filepath.Join(filepath.Dir(dir), "escape.png"), pngBytes, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	r := testResolver(t, Options{Dir: dir})
	r.Allow([]string{"http://../escape/"})

	if _, ok := r.Lookup(context.Background(), "http://../escape/"); ok {
		t.Error("Lookup() = true for a host that escapes Dir, want false")
	}
}

func TestNegativeCaching(t *testing.T) {
	s := newSite(t, map[string]http.HandlerFunc{
		"/": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		},
	})
	r := testResolver(t, Options{FailureTTL: time.Minute})
	r.Allow([]string{s.URL})

	if _, ok := r.Lookup(context.Background(), s.URL); ok {
		t.Fatal("Lookup() = true for a 500 page, want false")
	}
	first := s.total()
	if first == 0 {
		t.Fatal("no upstream request was made")
	}

	for i := 0; i < 5; i++ {
		if _, ok := r.Lookup(context.Background(), s.URL); ok {
			t.Fatalf("Lookup() #%d = true, want false", i+2)
		}
	}
	if got := s.total(); got != first {
		t.Fatalf("upstream requests grew from %d to %d; the failure was not cached", first, got)
	}

	entries, hits, misses := r.Stats()
	if entries != 1 || misses != 1 || hits != 5 {
		t.Errorf("Stats() = (%d entries, %d hits, %d misses), want (1, 5, 1)", entries, hits, misses)
	}
}

func TestNegativeCacheExpires(t *testing.T) {
	s := newSite(t, map[string]http.HandlerFunc{
		"/": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		},
	})
	r := testResolver(t, Options{FailureTTL: 30 * time.Millisecond})
	r.Allow([]string{s.URL})

	if _, ok := r.Lookup(context.Background(), s.URL); ok {
		t.Fatal("Lookup() = true, want false")
	}
	first := s.total()

	time.Sleep(80 * time.Millisecond)
	if _, ok := r.Lookup(context.Background(), s.URL); ok {
		t.Fatal("Lookup() = true after the failure TTL, want false")
	}
	if s.total() == first {
		t.Fatal("a stale failure was served past FailureTTL")
	}
}

func TestTTLExpiry(t *testing.T) {
	s := newSite(t, map[string]http.HandlerFunc{
		"/":      page(`<link rel="icon" href="/a.png">`),
		"/a.png": image(pngBytes, "image/png"),
	})
	r := testResolver(t, Options{TTL: 40 * time.Millisecond})
	r.Allow([]string{s.URL})

	if _, ok := r.Lookup(context.Background(), s.URL); !ok {
		t.Fatal("first Lookup() = false")
	}
	if _, ok := r.Lookup(context.Background(), s.URL); !ok {
		t.Fatal("second Lookup() = false")
	}
	if got := s.hits("/"); got != 1 {
		t.Fatalf("page fetches = %d, want 1 while the entry is fresh", got)
	}

	time.Sleep(80 * time.Millisecond)
	if _, ok := r.Lookup(context.Background(), s.URL); !ok {
		t.Fatal("third Lookup() = false")
	}
	if got := s.hits("/"); got != 2 {
		t.Fatalf("page fetches = %d, want 2 after the entry expired", got)
	}
}

func TestConcurrentLookupFetchesOnce(t *testing.T) {
	var pages int32
	s := newSite(t, map[string]http.HandlerFunc{
		"/": func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&pages, 1)
			// Hold the flight open so every other caller has to join it.
			time.Sleep(100 * time.Millisecond)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = io.WriteString(w, `<link rel="icon" href="/a.png">`)
		},
		"/a.png": image(pngBytes, "image/png"),
	})
	r := testResolver(t, Options{})
	r.Allow([]string{s.URL})

	// Allow is called on every config load while requests are in flight.
	stop := make(chan struct{})
	var allowWG sync.WaitGroup
	allowWG.Add(1)
	go func() {
		defer allowWG.Done()
		for {
			select {
			case <-stop:
				return
			default:
				r.Allow([]string{s.URL})
			}
		}
	}()

	const n = 24
	var (
		wg      sync.WaitGroup
		start   = make(chan struct{})
		results = make([]bool, n)
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, ok := r.Lookup(context.Background(), s.URL)
			results[i] = ok
		}(i)
	}
	close(start)
	wg.Wait()
	close(stop)
	allowWG.Wait()

	for i, ok := range results {
		if !ok {
			t.Fatalf("Lookup() #%d = false, want the shared icon", i)
		}
	}
	if got := atomic.LoadInt32(&pages); got != 1 {
		t.Errorf("page fetches = %d, want 1 for %d concurrent lookups", got, n)
	}
	if got := s.hits("/a.png"); got != 1 {
		t.Errorf("icon fetches = %d, want 1 for %d concurrent lookups", got, n)
	}
}

func TestLookupRejectsBadURLs(t *testing.T) {
	r := testResolver(t, Options{})
	r.Allow([]string{"http://127.0.0.1/"})

	for _, raw := range []string{
		"",
		"://nope",
		"http://[::1",
		"127.0.0.1/nav",
		"javascript:alert(1)",
		"data:text/html,<script>alert(1)</script>",
		"file:///etc/passwd",
		"http:///nopath",
		"http://\x7f/",
		"http://127.0.0.1:notaport/",
	} {
		if _, ok := r.Lookup(context.Background(), raw); ok {
			t.Errorf("Lookup(%q) = true, want a clean refusal", raw)
		}
	}
}

func TestLookupHostilePages(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"javascript href", `<link rel="icon" href="javascript:alert(1)">`},
		{"vbscript href", `<link rel="icon" href="vbscript:msgbox(1)">`},
		{"data uri href", `<link rel="icon" href="data:image/png;base64,AAAA">`},
		{"javascript base", `<base href="javascript:alert(1)"><link rel="icon" href="a.png">`},
		{"base without href", `<base target="_blank"><link rel="icon" href="/a.png">`},
		{"unterminated tag", `<html><head><link rel="shortcut`},
		{"truncated mid attribute", `<html><head><link rel="icon" href="/a.png`},
		{"angle bracket flood", "<html><head>" + strings.Repeat("<", 4096)},
		{"quote flood", `<link rel="icon" href="` + strings.Repeat(`"`, 512)},
		{"no markup", `just text`},
		{"empty body", ``},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newSite(t, map[string]http.HandlerFunc{"/": page(tc.body)})
			r := testResolver(t, Options{})
			r.Allow([]string{s.URL})

			// Nothing here resolves to an image: the catch-all page route
			// answers text/html for every fallback path too.
			if icon, ok := r.Lookup(context.Background(), s.URL); ok {
				t.Fatalf("Lookup() = true (%q), want a clean refusal", icon.Data)
			}
		})
	}

	// A nil context must not panic either.
	s := newSite(t, map[string]http.HandlerFunc{"/": page(``)})
	r := testResolver(t, Options{})
	r.Allow([]string{s.URL})
	//lint:ignore SA1012 deliberately hostile: a nil context must not panic
	if _, ok := r.Lookup(nil, s.URL); ok {
		t.Error("Lookup(nil, ...) = true, want false")
	}
}

func TestTruncatedPageStillFindsIcon(t *testing.T) {
	// The document is cut off, but the link tag itself is complete: leniency
	// here is what makes a truncated read harmless rather than fatal.
	s := newSite(t, map[string]http.HandlerFunc{
		"/":      page(`<html><head><link rel="icon" href="/a.png">`),
		"/a.png": image(pngBytes, "image/png"),
	})
	r := testResolver(t, Options{})
	r.Allow([]string{s.URL})

	if _, ok := r.Lookup(context.Background(), s.URL); !ok {
		t.Errorf("Lookup() = false for a truncated document with a complete link (paths: %v)", s.seen())
	}
}

func TestLookupRejectsOversizedIcon(t *testing.T) {
	huge := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0x01}, 4096)...)
	s := newSite(t, map[string]http.HandlerFunc{
		"/":      page(`<link rel="icon" href="/a.png">`),
		"/a.png": image(huge, "image/png"),
	})
	r := testResolver(t, Options{MaxBytes: 1024})
	r.Allow([]string{s.URL})

	if _, ok := r.Lookup(context.Background(), s.URL); ok {
		t.Error("Lookup() = true for an icon past MaxBytes, want false")
	}
}

func TestLookupTruncatesLongHTML(t *testing.T) {
	// The icon link sits past MaxHTMLBytes, so it is never read; the resolver
	// must still finish instead of buffering the page.
	filler := strings.Repeat("<!-- filler -->", 200)
	s := newSite(t, map[string]http.HandlerFunc{
		"/":      page(filler + `<link rel="icon" href="/a.png">`),
		"/a.png": image(pngBytes, "image/png"),
		"/favicon.ico": func(w http.ResponseWriter, r *http.Request) {
			http.NotFound(w, r)
		},
	})
	r := testResolver(t, Options{MaxHTMLBytes: 512})
	r.Allow([]string{s.URL})

	if _, ok := r.Lookup(context.Background(), s.URL); ok {
		t.Error("Lookup() = true for a link past MaxHTMLBytes, want false")
	}
	if got := s.hits("/favicon.ico"); got != 1 {
		t.Errorf("requests for the fallback = %d, want 1", got)
	}
}

func TestContentTypeSniffing(t *testing.T) {
	cases := []struct {
		name   string
		header string
		// nil means "no Content-Type header at all".
		omit   bool
		body   []byte
		wantOK bool
		wantCT string
	}{
		{"png header", "image/png", false, pngBytes, true, "image/png"},
		{"octet stream is sniffed", "application/octet-stream", false, pngBytes, true, "image/png"},
		{"text plain is sniffed", "text/plain", false, pngBytes, true, "image/png"},
		{"missing header is sniffed", "", true, gifBytes, true, "image/gif"},
		{"header parameters are kept out of the type", "image/png; charset=binary", false, pngBytes, true, "image/png"},
		{"html is refused", "text/html; charset=utf-8", false, []byte("<html></html>"), false, ""},
		{"garbage header falls back to sniffing", "!!!", false, pngBytes, true, "image/png"},
		{"empty body is refused", "image/png", false, []byte{}, false, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			iconHandler := func(w http.ResponseWriter, r *http.Request) {
				if tc.omit {
					// A present-but-nil key suppresses net/http's own sniffing.
					w.Header()["Content-Type"] = nil
				} else {
					w.Header().Set("Content-Type", tc.header)
				}
				_, _ = w.Write(tc.body)
			}
			s := newSite(t, map[string]http.HandlerFunc{
				"/":      page(`<link rel="icon" href="/a.png">`),
				"/a.png": iconHandler,
			})
			r := testResolver(t, Options{})
			r.Allow([]string{s.URL})

			icon, ok := r.Lookup(context.Background(), s.URL)
			if ok != tc.wantOK {
				t.Fatalf("Lookup() ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && icon.ContentType != tc.wantCT {
				t.Errorf("Lookup() content type = %q, want %q", icon.ContentType, tc.wantCT)
			}
		})
	}
}

func TestLRUBoundsEntries(t *testing.T) {
	s := newSite(t, map[string]http.HandlerFunc{
		"/":      page(`<link rel="icon" href="/a.png">`),
		"/a.png": image(pngBytes, "image/png"),
	})
	r := testResolver(t, Options{MaxEntries: 3})
	r.Allow([]string{s.URL})

	key := func(i int) string { return fmt.Sprintf("%s/?i=%d", s.URL, i) }
	for i := 0; i < 10; i++ {
		if _, ok := r.Lookup(context.Background(), key(i)); !ok {
			t.Fatalf("Lookup(%q) = false", key(i))
		}
		if entries, _, _ := r.Stats(); entries > 3 {
			t.Fatalf("Stats() entries = %d after %d lookups, want <= 3", entries, i+1)
		}
	}

	entries, _, _ := r.Stats()
	if entries != 3 {
		t.Fatalf("Stats() entries = %d, want 3", entries)
	}
	if _, ok, fresh := r.get(key(9)); !fresh || !ok {
		t.Error("the most recently used entry was evicted")
	}
	if _, _, fresh := r.get(key(0)); fresh {
		t.Error("the coldest entry survived eviction")
	}
}

func TestRelTokens(t *testing.T) {
	cases := []struct {
		rel  string
		want bool
	}{
		{"icon", true},
		{"shortcut icon", true},
		{"apple-touch-icon", true},
		{"apple-touch-icon-precomposed", true},
		{"mask-icon", true},
		{"ICON", true},
		{"Apple-Touch-Icon-Precomposed", true},
		{"  icon  ", true},
		{"alternate icon", true},
		{"stylesheet", false},
		{"iconify", false},
		{"shortcut", false},
		{"", false},
	}

	for _, tc := range cases {
		if got := iconRel(tc.rel); got != tc.want {
			t.Errorf("iconRel(%q) = %v, want %v", tc.rel, got, tc.want)
		}
	}
}

func TestIconSize(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"192x192", 192 * 192},
		{"32x32", 32 * 32},
		{"16x16 32x32", 32 * 32},
		// "any" is not a number, so it cannot outrank a sized icon; it is only
		// preferred by being listed first.
		{"any", 0},
		{"ANY", 0},
		{"32X32", 32 * 32},
		{"banana", 0},
		{"0x0", 0},
		{"-1x-1", 0},
	}

	for _, tc := range cases {
		if got := iconSize(tc.in); got != tc.want {
			t.Errorf("iconSize(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestParseIconLink(t *testing.T) {
	pageURL, err := url.Parse("http://example.com/dir/page.html")
	if err != nil {
		t.Fatalf("url.Parse() error = %v", err)
	}

	cases := []struct {
		name string
		doc  string
		want string
	}{
		{
			name: "absolute",
			doc:  `<link rel="icon" href="https://cdn.example.com/i.png">`,
			want: "https://cdn.example.com/i.png",
		},
		{
			name: "root relative",
			doc:  `<link rel="icon" href="/i.png">`,
			want: "http://example.com/i.png",
		},
		{
			name: "document relative",
			doc:  `<link rel="icon" href="i.png">`,
			want: "http://example.com/dir/i.png",
		},
		{
			name: "base root relative",
			doc:  `<base href="/static/"><link rel="icon" href="i.png">`,
			want: "http://example.com/static/i.png",
		},
		{
			name: "base absolute cross host",
			doc:  `<base href="https://cdn.example.com/x/"><link rel="icon" href="i.png">`,
			want: "https://cdn.example.com/x/i.png",
		},
		{
			name: "javascript base",
			doc:  `<base href="javascript:alert(1)"><link rel="icon" href="i.png">`,
			want: "",
		},
		{
			name: "javascript href",
			doc:  `<link rel="icon" href="javascript:alert(1)">`,
			want: "",
		},
		{
			name: "protocol relative",
			doc:  `<link rel="icon" href="//cdn.example.com/i.png">`,
			want: "http://cdn.example.com/i.png",
		},
		{
			name: "largest sizes wins",
			doc: `<link rel="icon" sizes="16x16" href="/s.png">
			      <link rel="icon" sizes="512x512" href="/l.png">`,
			want: "http://example.com/l.png",
		},
		{
			name: "first of equal sizes wins",
			doc: `<link rel="icon" sizes="32x32" href="/first.png">
			      <link rel="icon" sizes="32x32" href="/second.png">`,
			want: "http://example.com/first.png",
		},
		{
			name: "unsized beats nothing only when there is nothing",
			doc:  `<link rel="icon" href="/only.png">`,
			want: "http://example.com/only.png",
		},
		{
			name: "no icon link",
			doc:  `<link rel="stylesheet" href="/a.css">`,
			want: "",
		},
		{
			name: "iconify is not an icon",
			doc:  `<link rel="iconify" href="/a.png">`,
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseIconLink(strings.NewReader("<html><head>"+tc.doc+"</head></html>"), pageURL)
			if tc.want == "" {
				if got != nil {
					t.Fatalf("parseIconLink() = %q, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("parseIconLink() = nil, want %q", tc.want)
			}
			if got.String() != tc.want {
				t.Errorf("parseIconLink() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestWarmResolvesAndBoundsConcurrency(t *testing.T) {
	const n = 24

	var inFlight, maxInFlight int32
	s := newSite(t, map[string]http.HandlerFunc{
		"/": func(w http.ResponseWriter, r *http.Request) {
			cur := atomic.AddInt32(&inFlight, 1)
			for {
				peak := atomic.LoadInt32(&maxInFlight)
				if cur <= peak || atomic.CompareAndSwapInt32(&maxInFlight, peak, cur) {
					break
				}
			}
			time.Sleep(30 * time.Millisecond)
			atomic.AddInt32(&inFlight, -1)

			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = io.WriteString(w, `<link rel="icon" href="/a.png">`)
		},
		"/a.png": image(pngBytes, "image/png"),
	})
	r := testResolver(t, Options{})
	r.Allow([]string{s.URL})

	urls := make([]string, 0, n)
	for i := 0; i < n; i++ {
		urls = append(urls, fmt.Sprintf("%s/?i=%d", s.URL, i))
	}
	r.Warm(context.Background(), urls)

	deadline := time.Now().Add(15 * time.Second)
	for {
		if entries, _, _ := r.Stats(); entries == n {
			break
		}
		if time.Now().After(deadline) {
			entries, _, _ := r.Stats()
			t.Fatalf("Warm resolved %d of %d icons", entries, n)
		}
		time.Sleep(5 * time.Millisecond)
	}

	if peak := atomic.LoadInt32(&maxInFlight); peak > warmWorkers {
		t.Errorf("peak concurrent fetches = %d, want <= %d", peak, warmWorkers)
	} else if peak < 2 {
		t.Errorf("peak concurrent fetches = %d, want a pool wider than 1", peak)
	}

	for _, u := range urls {
		if _, ok := r.Lookup(context.Background(), u); !ok {
			t.Fatalf("Lookup(%q) = false after Warm", u)
		}
	}
}

func TestWarmIgnoresDisallowedAndFresh(t *testing.T) {
	s := newSite(t, map[string]http.HandlerFunc{
		"/":      page(`<link rel="icon" href="/a.png">`),
		"/a.png": image(pngBytes, "image/png"),
	})
	r := testResolver(t, Options{})
	r.Allow([]string{s.URL})

	if _, ok := r.Lookup(context.Background(), s.URL); !ok {
		t.Fatal("Lookup() = false")
	}
	before := s.total()

	// Already cached, duplicated, disallowed, and unparseable: none of it may
	// produce upstream traffic.
	other := strings.Replace(s.URL, "127.0.0.1", "localhost", 1)
	r.Warm(context.Background(), []string{
		s.URL,
		s.URL,
		other,
		other + "/deep?a=1",
		"://nope",
		"",
	})
	//lint:ignore SA1012 deliberately hostile: a nil context must not panic
	r.Warm(nil, []string{s.URL})

	time.Sleep(200 * time.Millisecond)
	if got := s.total(); got != before {
		t.Errorf("upstream requests = %d after warming cached and disallowed URLs, want %d", got, before)
	}
}

func TestWarmDoesNotLeakOnCancelledContext(t *testing.T) {
	r := testResolver(t, Options{})
	r.Allow([]string{"http://icons.invalid/"})

	urls := make([]string, 0, 32)
	for i := 0; i < 32; i++ {
		urls = append(urls, fmt.Sprintf("http://icons.invalid/?i=%d", i))
	}

	before := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for i := 0; i < 20; i++ {
		r.Warm(ctx, urls)
	}

	// Twenty calls would park ~160 workers forever if the pool did not wind
	// down on its own.
	deadline := time.Now().Add(10 * time.Second)
	after := runtime.NumGoroutine()
	for after > before+2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		after = runtime.NumGoroutine()
	}
	if after > before+2 {
		t.Errorf("goroutines grew from %d to %d: Warm leaked its workers", before, after)
	}
}

func TestStatsCounts(t *testing.T) {
	s := newSite(t, map[string]http.HandlerFunc{
		"/":      page(`<link rel="icon" href="/a.png">`),
		"/a.png": image(pngBytes, "image/png"),
	})
	r := testResolver(t, Options{})
	r.Allow([]string{s.URL})

	if entries, hits, misses := r.Stats(); entries != 0 || hits != 0 || misses != 0 {
		t.Fatalf("Stats() on a fresh resolver = (%d, %d, %d), want zeroes", entries, hits, misses)
	}
	if _, ok := r.Lookup(context.Background(), s.URL); !ok {
		t.Fatal("Lookup() = false")
	}
	if _, ok := r.Lookup(context.Background(), s.URL); !ok {
		t.Fatal("Lookup() = false")
	}
	entries, hits, misses := r.Stats()
	if entries != 1 || hits != 1 || misses != 1 {
		t.Errorf("Stats() = (%d, %d, %d), want (1, 1, 1)", entries, hits, misses)
	}
}

func TestDefaults(t *testing.T) {
	r := testResolver(t, Options{})
	if r.opts.TTL != defaultTTL || r.opts.FailureTTL != defaultFailureTTL {
		t.Errorf("TTL defaults = %v/%v, want %v/%v", r.opts.TTL, r.opts.FailureTTL, defaultTTL, defaultFailureTTL)
	}
	if r.opts.MaxEntries != defaultMaxEntries || r.opts.MaxBytes != defaultMaxBytes ||
		r.opts.MaxHTMLBytes != defaultMaxHTMLBytes || r.opts.Timeout != defaultTimeout {
		t.Errorf("numeric defaults = %+v, want the package defaults", r.opts)
	}
	if r.opts.UserAgent == "" {
		t.Error("UserAgent default is empty")
	}
}

func TestLocalIconsCountsOnlyUsableFiles(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, data []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	write("a.example.svg", []byte("<svg/>"))
	write("b.example.png", []byte("x"))
	write("c.example.ico", []byte("x"))
	write("notes.txt", []byte("ignored: wrong extension"))
	write("d.example", []byte("ignored: no extension"))
	if err := os.MkdirAll(filepath.Join(dir, "sub.example.png"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := LocalIcons(dir)
	if err != nil {
		t.Fatalf("LocalIcons() error = %v", err)
	}
	if got != 3 {
		t.Errorf("LocalIcons() = %d, want 3 (only the three usable icon files)", got)
	}
}

// The count is what tells an operator that a typo'd --icon-dir is doing
// nothing, so it has to be able to report "this path is wrong".
func TestLocalIconsReportsBadPaths(t *testing.T) {
	if n, err := LocalIcons(""); err != nil || n != 0 {
		t.Errorf("LocalIcons(\"\") = %d, %v; want 0, nil", n, err)
	}
	if _, err := LocalIcons(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("LocalIcons() accepted a missing directory")
	}

	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LocalIcons(file); err == nil {
		t.Error("LocalIcons() accepted a regular file")
	}
}

func TestLocalIconsRejectsTraversalNames(t *testing.T) {
	dir := t.TempDir()
	// A file whose "host" part contains a traversal must not be counted, since
	// the resolver would never look it up that way.
	if err := os.WriteFile(filepath.Join(dir, "..evil.svg"), []byte("<svg/>"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LocalIcons(dir)
	if err != nil {
		t.Fatalf("LocalIcons() error = %v", err)
	}
	if got != 0 {
		t.Errorf("LocalIcons() = %d, want 0", got)
	}
}
