// Package icons resolves and caches site icons on the server.
//
// Pointing an <img> at https://<host>/favicon.ico leaks every visitor's IP to
// every linked site and renders a broken image wherever there is no favicon.
// Resolving the icon here costs one upstream fetch per host and keeps the
// browser talking to ghome alone.
//
// Lookup would be an open proxy if it fetched whatever it was asked for, so the
// allow list registered by Allow is the security boundary: a host that no
// config load has mentioned is never contacted.
package icons

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Defaults for the zero Options. They are sized for a small VPS: the whole
// cache is bounded to a few megabytes and no single fetch can hold a request
// open for long.
const (
	defaultTTL = 24 * time.Hour
	// defaultFailureTTL is zero, which means a failure is remembered for the
	// life of the process: an icon that could not be fetched is not retried.
	// "This site has no /favicon.ico" is a fact about the site, and re-asking on
	// every page load is exactly the traffic a navigation page should not make.
	// A restart is how a fixed icon is picked up, which is also the moment the
	// config itself is re-read. FailureTTL is still settable for a caller that
	// wants retries; the CLI just does not offer one.
	defaultFailureTTL = time.Duration(0)
	defaultMaxEntries = 512
	// MaxEntries alone bounds nothing useful: 512 entries x 512KiB is 256MiB,
	// which is not what "a few megabytes of icon cache" should mean on a box
	// that otherwise idles at twelve. The byte budget is what actually bounds it.
	defaultMaxTotalBytes = 8 << 20
	defaultMaxBytes      = 512 << 10
	defaultTimeout       = 8 * time.Second
	defaultUserAgent     = "ghome-icons/1.0 (+https://github.com/vrocwang/ghome)"
)

const (
	// maxRedirects stops a redirect loop from burning the whole timeout.
	maxRedirects = 5
	// warmWorkers bounds Warm's fan-out so a cold start cannot open hundreds
	// of sockets at once.
	warmWorkers = 8
)

// localExts are the file names accepted inside Options.Dir, in preference
// order. SVG comes first because it is what hand-made icons usually are.
var localExts = []string{".svg", ".png", ".ico", ".webp", ".jpg", ".jpeg"}

// Options configures a Resolver. The zero value is usable.
type Options struct {
	// Dir is an optional directory of local icon files. A file named
	// <host>.svg/.png/.ico/.webp/.jpg/.jpeg wins over fetching the site.
	Dir string
	// NoFetch refuses to fetch icons from the network, serving Dir and nothing
	// else: a host with no local file resolves to no icon, and the browser falls
	// back to a letter avatar.
	//
	// It is named for the exception rather than the rule on purpose. Fetching is
	// what this package is for, so the zero value has to mean "fetch" -- a field
	// called Auto would make every careless construction of Options silently
	// stop fetching, which is a hard symptom to trace back to a missing field.
	NoFetch bool
	// TTL is how long a successfully resolved icon stays fresh. Default 24h.
	TTL time.Duration
	// FailureTTL is how long a negative result is remembered. Zero -- the
	// default -- means never: a host whose icon could not be fetched is not
	// asked again for the life of the process. Set it to retry instead, with the
	// retry rate that implies for every icon-less host in the config.
	FailureTTL time.Duration
	// MaxEntries bounds the cache. Default 512.
	MaxEntries int
	// MaxBytes bounds a single icon body. Default 512KB.
	MaxBytes int64

	// MaxTotalBytes bounds the whole cache. Default 8MB.
	MaxTotalBytes int64
	// Timeout bounds a single upstream request. Default 8s.
	Timeout time.Duration
	// UserAgent is sent upstream. Give it a sane default.
	UserAgent string
	// Logger receives fetch details. Nil uses slog.Default().
	Logger *slog.Logger
}

// Icon is a resolved favicon: the bytes and the media type to serve them as.
type Icon struct {
	// Data is shared with the cache and must not be modified by the caller.
	Data        []byte
	ContentType string
}

// Resolver resolves and caches site icons. It is safe for concurrent use.
type Resolver struct {
	opts   Options
	log    *slog.Logger
	client *http.Client

	allowMu sync.RWMutex
	// allowed holds lowercased host names without ports. It is replaced as a
	// whole on every Allow, so a lookup never sees a half-updated list.
	allowed map[string]struct{}

	mu     sync.Mutex
	lru    *list.List
	byKey  map[string]*list.Element
	bytes  int64 // total cached icon bytes, kept in step with the list
	hits   uint64
	misses uint64

	flights flightGroup

	// warmSem bounds upstream fetches across every Warm call. The per-call
	// worker count does not bound the total on its own: each config reload
	// starts its own pool, so the number of sockets climbs with the number of
	// reloads.
	warmSem chan struct{}
}

// New builds a Resolver. Options are filled with defaults in place; Dir and
// Logger are read once and a missing Dir is not an error, it just means
// everything is fetched.
func New(opts Options) *Resolver {
	if opts.TTL <= 0 {
		opts.TTL = defaultTTL
	}
	// FailureTTL is deliberately not defaulted here: its "unset" value and its
	// "never retry" value are the same zero, so normalising it would only hide
	// which one the caller meant.
	if opts.MaxEntries <= 0 {
		opts.MaxEntries = defaultMaxEntries
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = defaultMaxBytes
	}
	if opts.MaxTotalBytes <= 0 {
		opts.MaxTotalBytes = defaultMaxTotalBytes
	}
	if opts.Timeout <= 0 {
		opts.Timeout = defaultTimeout
	}
	if opts.UserAgent == "" {
		opts.UserAgent = defaultUserAgent
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}

	r := &Resolver{
		opts:    opts,
		log:     opts.Logger,
		allowed: map[string]struct{}{},
		lru:     list.New(),
		byKey:   map[string]*list.Element{},
		warmSem: make(chan struct{}, warmWorkers),
	}
	// The client needs the allow list, not just the timeout: a redirect is
	// chosen by the far end, so without this check any allowed host could hand
	// the fetch to an address the config never named.
	r.client = newClient(opts.Timeout, r.isAllowed)
	return r
}

// Allow registers the page URLs the resolver is permitted to serve. It is
// called on every config load and must be safe for concurrent use.
//
// The list replaces the previous one rather than adding to it: a link removed
// from nav.yaml must stop being reachable immediately. Only http and https
// hosts are kept, since nothing else is ever fetched.
//
// It bounds every hop, not just the first: page markup naming an icon on
// another host is refused, and so is a redirect that leaves the list. The cost
// is that an icon served from a CDN is not fetched; --icon-dir is the answer
// for those.
func (r *Resolver) Allow(pageURLs []string) {
	next := make(map[string]struct{}, len(pageURLs))
	for _, raw := range pageURLs {
		if host, ok := hostOf(raw); ok {
			next[host] = struct{}{}
		}
	}

	r.allowMu.Lock()
	r.allowed = next
	r.allowMu.Unlock()
}

// Lookup returns the icon for a page URL. It returns cached bytes when fresh,
// otherwise it fetches. ok is false when the URL is unusable, its host was
// never allowed, or nothing that qualifies as an image could be resolved.
func (r *Resolver) Lookup(ctx context.Context, pageURL string) (Icon, bool) {
	if ctx == nil {
		ctx = context.Background()
	}

	u, err := url.Parse(strings.TrimSpace(pageURL))
	if err != nil || !isHTTP(u) {
		return Icon{}, false
	}
	host := strings.ToLower(u.Hostname())
	if !r.isAllowed(host) {
		return Icon{}, false
	}
	// A local file is the operator's explicit answer for this host, so it wins
	// over both the cache and the network.
	if icon, ok := r.local(host); ok {
		r.countHit()
		return icon, true
	}

	// With fetching off there is nothing left that could answer: the caller gets
	// the same "no icon" it would get for a site whose favicon cannot be
	// resolved, and the browser falls back to a letter avatar.
	if r.opts.NoFetch {
		return Icon{}, false
	}

	key := u.String()
	if icon, ok, fresh := r.get(key); fresh {
		r.countHit()
		return icon, ok
	}
	r.countMiss()

	// The cache is filled inside the flight, before the flight is forgotten:
	// otherwise a lookup that slipped past the map could fetch the same icon
	// again behind the leader's back. The re-check below closes the same window
	// for a lookup that missed the cache while another flight was storing it.
	return r.flights.do(key, func() (Icon, bool) {
		if icon, ok, fresh := r.get(key); fresh {
			return icon, ok
		}
		icon, ok, cacheable := r.resolve(ctx, u)
		if cacheable {
			// A failure goes in with FailureTTL, which is zero by default: the
			// miss is remembered for the life of the process, so a host without
			// an icon is fetched exactly once rather than on every page load.
			ttl := r.opts.TTL
			if !ok {
				ttl = r.opts.FailureTTL
			}
			r.put(key, icon, ok, ttl)
		}
		return icon, ok
	})
}

// Warm resolves icons for the given URLs in the background with bounded
// concurrency, so the first page load already has most icons cached.
// It returns immediately; work stops early if ctx is cancelled.
func (r *Resolver) Warm(ctx context.Context, pageURLs []string) {
	if ctx == nil {
		ctx = context.Background()
	}
	// Nothing here could be served: Lookup refuses to fetch in this mode too, so
	// a warm pass would only fetch pages and icons nobody can ever be shown.
	if r.opts.NoFetch {
		return
	}
	pending := r.pending(pageURLs)
	if len(pending) == 0 {
		return
	}
	go r.warm(ctx, pending)
}

// Stats reports counters for the /healthz endpoint. entries counts every
// cached answer, negative ones included.
func (r *Resolver) Stats() (entries int, hits, misses uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lru.Len(), r.hits, r.misses
}

// warm drains urls with at most warmWorkers goroutines. It always finishes:
// the feeder closes jobs, so every worker's range ends even when ctx dies
// mid-flight.
func (r *Resolver) warm(ctx context.Context, urls []string) {
	jobs := make(chan string)

	workers := min(len(urls), warmWorkers)
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for u := range jobs {
				select {
				case r.warmSem <- struct{}{}:
				case <-ctx.Done():
					return
				}
				_, _ = r.Lookup(ctx, u)
				<-r.warmSem
			}
		}()
	}

	go func() {
		defer close(jobs)
		for _, u := range urls {
			select {
			case jobs <- u:
			case <-ctx.Done():
				return
			}
		}
	}()

	wg.Wait()
}

// pending returns the URLs actually worth fetching: allowed, http(s), not
// already cached fresh, deduplicated, and not already answered by a local file.
func (r *Resolver) pending(pageURLs []string) []string {
	seen := make(map[string]struct{}, len(pageURLs))
	out := make([]string, 0, len(pageURLs))

	for _, raw := range pageURLs {
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || !isHTTP(u) || !r.isAllowed(strings.ToLower(u.Hostname())) {
			continue
		}
		key := u.String()
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		if _, _, fresh := r.get(key); fresh {
			continue
		}
		// A local file already answers for this host and is checked before both
		// the cache and the network. Warming it anyway would fetch a page and an
		// icon that can never be served -- which would leave --icon-dir costing
		// exactly the traffic it was set up to avoid.
		if r.hasLocal(u.Hostname()) {
			continue
		}
		out = append(out, key)
	}
	return out
}

// resolve does the network work for one page URL. cacheable is false when the
// outcome says nothing about the site: a cancelled caller must not poison the
// cache that everybody else reads.
func (r *Resolver) resolve(ctx context.Context, page *url.URL) (Icon, bool, bool) {
	target := iconURL(page)
	icon, err := r.download(ctx, target)
	if err != nil {
		r.log.Debug("icons: download failed",
			"host", page.Hostname(), "icon", safeURL(target), "err", err)
		return Icon{}, false, ctx.Err() == nil
	}

	r.log.Debug("icons: resolved",
		"host", page.Hostname(), "type", icon.ContentType, "bytes", len(icon.Data))
	return icon, true, true
}

// iconURL is the conventional icon path on the site's own origin. Host is
// carried over verbatim so a non-default port survives and the allow list --
// which is keyed by hostname -- is checked against exactly what will be dialled.
func iconURL(page *url.URL) *url.URL {
	return &url.URL{Scheme: page.Scheme, Host: page.Host, Path: "/favicon.ico"}
}

// download fetches one icon and refuses anything that is not an image.
func (r *Resolver) download(ctx context.Context, iconURL *url.URL) (Icon, error) {
	// The redirect target is chosen by the far end, so the check cannot live
	// only where the URL was built: without this, one allowed host would be
	// enough to make the resolver fetch any address at all.
	if host := iconURL.Hostname(); !r.isAllowed(host) {
		return Icon{}, fmt.Errorf("icons: %s is not in the config", host)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, iconURL.String(), nil)
	if err != nil {
		return Icon{}, err
	}
	req.Header.Set("User-Agent", r.opts.UserAgent)
	req.Header.Set("Accept", "image/*,*/*;q=0.5")

	resp, err := r.client.Do(req)
	if err != nil {
		return Icon{}, err
	}

	// One byte past the cap so an oversized body is rejected instead of being
	// silently cached as a truncated image.
	data, err := io.ReadAll(io.LimitReader(resp.Body, r.opts.MaxBytes+1))
	if err != nil {
		return Icon{}, err
	}
	if int64(len(data)) > r.opts.MaxBytes {
		return Icon{}, fmt.Errorf("icon body exceeds %d bytes", r.opts.MaxBytes)
	}
	if len(data) == 0 {
		return Icon{}, errors.New("empty icon body")
	}

	ct := detectContentType(resp.Header.Get("Content-Type"), data)
	if !strings.HasPrefix(ct, "image/") {
		return Icon{}, fmt.Errorf("not an image (%q)", ct)
	}
	return Icon{Data: data, ContentType: ct}, nil
}

// local returns the operator supplied icon for a host, if Dir has one.
func (r *Resolver) local(host string) (Icon, bool) {
	if r.opts.Dir == "" || !safeHostName(host) {
		return Icon{}, false
	}
	for _, ext := range localExts {
		data, err := r.readLocal(filepath.Join(r.opts.Dir, host+ext))
		if err != nil {
			continue
		}
		return Icon{Data: data, ContentType: localContentType(ext, data)}, true
	}
	return Icon{}, false
}

// hasLocal reports whether Dir holds an icon for host, without reading the
// bytes. Warm uses it to skip a host that local() would answer before the
// network is ever consulted.
func (r *Resolver) hasLocal(host string) bool {
	if r.opts.Dir == "" || !safeHostName(host) {
		return false
	}
	for _, ext := range localExts {
		if info, err := os.Stat(filepath.Join(r.opts.Dir, host+ext)); err == nil && !info.IsDir() {
			return true
		}
	}
	return false
}

// readLocal reads one local icon, held to the same size cap as a fetched one so
// a stray huge file in Dir cannot be loaded into memory on every page load.
func (r *Resolver) readLocal(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	data, err := io.ReadAll(io.LimitReader(f, r.opts.MaxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > r.opts.MaxBytes {
		return nil, fmt.Errorf("icons: %s exceeds %d bytes", path, r.opts.MaxBytes)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("icons: %s is empty", path)
	}
	return data, nil
}

// detectContentType trusts the server's Content-Type when it means something
// and sniffs the magic bytes otherwise: icons served as
// application/octet-stream are common, and a text/html error page must never be
// cached as an image.
func detectContentType(header string, data []byte) string {
	ct := ""
	if header != "" {
		if mt, _, err := mime.ParseMediaType(header); err == nil {
			ct = strings.ToLower(mt)
		}
	}
	if !strings.Contains(ct, "/") {
		// Anything that is not even a media type carries no information.
		ct = ""
	}
	switch ct {
	case "", "application/octet-stream", "text/plain":
		ct = http.DetectContentType(data)
	}
	return ct
}

// localContentType names a local icon file. Sniffing covers the binary
// formats; the extension is authoritative for SVG, which sniffing can only
// report as generic XML.
func localContentType(ext string, data []byte) string {
	if ct := http.DetectContentType(data); strings.HasPrefix(ct, "image/") {
		return ct
	}
	switch ext {
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	case ".ico":
		return "image/vnd.microsoft.icon"
	case ".webp":
		return "image/webp"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	}
	return "application/octet-stream"
}

// hostOf extracts the normalised host of an allow list entry. Ports are
// dropped so that http://x:3000 and http://x count as the same site, and only
// http(s) URLs can register anything.
func hostOf(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !isHTTP(u) {
		return "", false
	}
	return strings.ToLower(u.Hostname()), true
}

// isHTTP reports whether u is something we are willing to fetch.
func isHTTP(u *url.URL) bool {
	if u == nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Scheme, "http") || strings.EqualFold(u.Scheme, "https")
}

// safeHostName keeps a hostile host name such as ".." from escaping Dir.
func safeHostName(host string) bool {
	return host != "" && host != "." && host != ".." &&
		!strings.ContainsAny(host, `/\`) && !strings.Contains(host, "..")
}

// safeURL renders a URL for logs without userinfo or query string, either of
// which can carry credentials or tracking tokens.
func safeURL(u *url.URL) string {
	if u == nil {
		return ""
	}
	return (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}).String()
}

func (r *Resolver) isAllowed(host string) bool {
	r.allowMu.RLock()
	defer r.allowMu.RUnlock()
	_, ok := r.allowed[strings.ToLower(host)]
	return ok
}

// entry is one cached answer, hit or miss.
type entry struct {
	key     string
	icon    Icon
	ok      bool
	expires time.Time
}

// neverExpires is the head of the expiry range, used for entries that are kept
// until the process ends. It is a real time rather than a bool so that get's
// single comparison stays enough, and it is comfortably inside the representable
// range of time.Time -- unlike time.Now().Add(math.MaxInt64), which overflows.
var neverExpires = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)

// size is what this entry contributes to the resolver's byte budget. A cached
// miss carries no body, so it costs an entry slot but no bytes.
func (e *entry) size() int64 { return int64(len(e.icon.Data)) }

// get returns a fresh cached answer. fresh is false when the key is absent or
// stale, in which case the caller has to do real work.
func (r *Resolver) get(key string) (icon Icon, ok, fresh bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	el, found := r.byKey[key]
	if !found {
		return Icon{}, false, false
	}
	e := el.Value.(*entry)
	if time.Now().After(e.expires) {
		r.removeLocked(el)
		return Icon{}, false, false
	}
	r.lru.MoveToFront(el)
	return e.icon, e.ok, true
}

// put stores an answer and evicts the coldest entries past MaxEntries. A zero
// ttl means the answer never expires: that is how a failure is remembered for
// the life of the process, so a host with no icon is fetched exactly once.
func (r *Resolver) put(key string, icon Icon, ok bool, ttl time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()

	expires := neverExpires
	if ttl > 0 {
		expires = time.Now().Add(ttl)
	}
	if el, found := r.byKey[key]; found {
		e := el.Value.(*entry)
		r.bytes += int64(len(icon.Data)) - e.size()
		e.icon, e.ok, e.expires = icon, ok, expires
		r.lru.MoveToFront(el)
		r.evictLocked()
		return
	}

	e := &entry{key: key, icon: icon, ok: ok, expires: expires}
	r.byKey[key] = r.lru.PushFront(e)
	r.bytes += e.size()
	r.evictLocked()
}

// evictLocked drops least-recently-used entries until both limits hold. Both
// are needed: the count keeps a flood of tiny answers from growing without
// bound, and the byte budget keeps a handful of large ones from doing the same.
func (r *Resolver) evictLocked() {
	for r.lru.Len() > r.opts.MaxEntries || r.bytes > r.opts.MaxTotalBytes {
		oldest := r.lru.Back()
		if oldest == nil {
			return
		}
		r.removeLocked(oldest)
	}
}

func (r *Resolver) removeLocked(el *list.Element) {
	e := el.Value.(*entry)
	r.bytes -= e.size()
	r.lru.Remove(el)
	delete(r.byKey, e.key)
}

func (r *Resolver) countHit() {
	r.mu.Lock()
	r.hits++
	r.mu.Unlock()
}

func (r *Resolver) countMiss() {
	r.mu.Lock()
	r.misses++
	r.mu.Unlock()
}

// call is one in-flight resolution shared by every Lookup that asked for the
// same page while it ran.
type call struct {
	done chan struct{}
	icon Icon
	ok   bool
}

// flightGroup collapses concurrent resolutions of one key into a single
// upstream fetch. golang.org/x/sync is not a dependency of this module, so this
// is the smallest thing that works: a map of in-flight calls under a mutex.
type flightGroup struct {
	mu    sync.Mutex
	calls map[string]*call
}

func (g *flightGroup) do(key string, fn func() (Icon, bool)) (Icon, bool) {
	g.mu.Lock()
	if c, running := g.calls[key]; running {
		g.mu.Unlock()
		<-c.done
		return c.icon, c.ok
	}
	if g.calls == nil {
		g.calls = make(map[string]*call)
	}
	c := &call{done: make(chan struct{})}
	g.calls[key] = c
	g.mu.Unlock()

	c.icon, c.ok = fn()
	close(c.done)

	g.mu.Lock()
	delete(g.calls, key)
	g.mu.Unlock()

	return c.icon, c.ok
}

// newClient builds the upstream client. The redirect policy is the one place a
// hostile response could steer us somewhere we would never fetch directly, so
// anything that is not http(s) is refused.
// newClient builds the HTTP client used for every fetch. allowed decides which
// hosts may still be reached after a redirect: the first request is checked
// against the allow list by the caller, but the redirect target is chosen by
// the server being fetched, so it has to be checked too.
func newClient(timeout time.Duration, allowed func(string) bool) *http.Client {
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("icons: stopped after %d redirects", maxRedirects)
			}
			if !isHTTP(req.URL) {
				return fmt.Errorf("icons: refusing redirect to scheme %q", req.URL.Scheme)
			}
			if host := req.URL.Hostname(); !allowed(host) {
				return fmt.Errorf("icons: refusing redirect to %q, which is not in the config", host)
			}
			return nil
		},
	}
}

// LocalIcons reports how many files in dir are ones this resolver would
// actually use.
//
// It exists so startup can tell "the flag was passed" apart from "the flag is
// doing something". Those differ by exactly one typo, and a typo'd --icon-dir
// is otherwise invisible: every lookup simply falls through to the network.
func LocalIcons(dir string) (int, error) {
	if dir == "" {
		return 0, nil
	}
	info, err := os.Stat(dir)
	if err != nil {
		return 0, err
	}
	if !info.IsDir() {
		return 0, fmt.Errorf("icons: %s is not a directory", dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}

	suffixes := make(map[string]bool, len(localExts))
	for _, ext := range localExts {
		suffixes[ext] = true
	}
	count := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(entry.Name()))
		if !suffixes[ext] {
			continue
		}
		host := strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
		if safeHostName(strings.ToLower(host)) {
			count++
		}
	}
	return count, nil
}
