// Package status probes the URLs in the navigation config so the page can show
// whether a service is reachable.
//
// The prober is optional and free until it is used: New only records options,
// and nothing runs until Run is called. Each probe is deliberately shallow — a
// HEAD request, one GET fallback, and at most a few KB of body — because the
// whole program is expected to idle in about 12MB on a small VPS.
package status

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// State is how a service looks in the page: a green, amber or grey dot.
type State string

// The dot colours.
const (
	// StateUp means the service answered, including 401 and 403: it is running.
	StateUp State = "up"
	// StateDown means it did not answer, or answered as a broken service.
	StateDown State = "down"
	// StateUnknown means nobody probed it: a mailto: or tel: link, or a target
	// no round has reached yet.
	StateUnknown State = "unknown"
)

// Result is the outcome of probing one URL.
type Result struct {
	URL       string    `json:"url"`
	State     State     `json:"state"`
	Code      int       `json:"code,omitempty"`
	LatencyMS int64     `json:"latencyMs,omitempty"`
	Error     string    `json:"error,omitempty"`
	CheckedAt time.Time `json:"checkedAt"`
}

// Options configures a Prober. The zero value is valid: every field falls back
// to a default tuned for a handful of internal services.
type Options struct {
	// Interval between probe rounds. Default 60s, minimum 5s.
	Interval time.Duration
	// Timeout bounds a single probe. Default 5s.
	Timeout time.Duration
	// Concurrency caps in-flight probes per round. Default 8.
	Concurrency int
	// Logger receives round summaries.
	Logger *slog.Logger
	// Client overrides the HTTP client, mainly for tests. When nil a client
	// with a sensible transport is built from Timeout.
	Client *http.Client
}

const (
	defaultInterval    = 60 * time.Second
	minInterval        = 5 * time.Second
	defaultTimeout     = 5 * time.Second
	defaultConcurrency = 8
	// maxConcurrency keeps a careless config from opening hundreds of sockets
	// at once on a box meant to idle.
	maxConcurrency = 64
	// maxRedirects ends a redirect loop with an error instead of following it
	// until the probe deadline.
	maxRedirects = 5
	// maxBodyBytes is all we ever read from a probed service: enough for the
	// server to finish answering, never a whole page.
	maxBodyBytes = 4 << 10
	userAgent    = "ghome-status"
)

// errTooManyRedirects is reported for a redirect chain longer than maxRedirects.
var errTooManyRedirects = errors.New("too many redirects")

// Prober probes a set of URLs on a timer. The zero value is not usable; call
// New.
type Prober struct {
	interval    time.Duration
	timeout     time.Duration
	concurrency int
	client      *http.Client
	log         *slog.Logger

	// mu guards targets and results. Both are published whole under the lock
	// and never mutated afterwards, so a round can compare against the previous
	// snapshot and probe without holding the lock.
	mu      sync.Mutex
	targets []string
	results map[string]Result
}

// New builds a Prober. Zero or negative options are replaced by defaults, and
// an Interval below the five second floor is raised to it: probing a link
// faster than that adds load without telling the operator anything new.
func New(opts Options) *Prober {
	p := &Prober{
		interval:    opts.Interval,
		timeout:     opts.Timeout,
		concurrency: opts.Concurrency,
		log:         opts.Logger,
		client:      opts.Client,
	}
	switch {
	case p.interval <= 0:
		p.interval = defaultInterval
	case p.interval < minInterval:
		p.interval = minInterval
	}
	if p.timeout <= 0 {
		p.timeout = defaultTimeout
	}
	if p.concurrency <= 0 {
		p.concurrency = defaultConcurrency
	}
	if p.concurrency > maxConcurrency {
		p.concurrency = maxConcurrency
	}
	if p.log == nil {
		p.log = slog.Default()
	}
	if p.client == nil {
		p.client = newClient(p.timeout)
	}
	return p
}

// SetTargets replaces the URL set to probe. Safe to call while Run is active.
//
// Only probeable URLs are kept: mailto:, tel:, ssh: and vscode: links are
// navigation shortcuts, not services, so they are dropped rather than reported
// down, and duplicates are collapsed. The order is normalised so rounds and
// logs are deterministic.
func (p *Prober) SetTargets(urls []string) {
	targets := make([]string, 0, len(urls))
	seen := make(map[string]struct{}, len(urls))
	for _, raw := range urls {
		u := strings.TrimSpace(raw)
		if !probeable(u) {
			continue
		}
		if _, dup := seen[u]; dup {
			continue
		}
		seen[u] = struct{}{}
		targets = append(targets, u)
	}
	sort.Strings(targets)

	p.mu.Lock()
	p.targets = targets
	p.mu.Unlock()

	p.log.Debug("status targets updated", "targets", len(targets))
}

// Run probes on a timer until ctx is cancelled. It blocks the calling goroutine
// and must be called at most once per Prober.
//
// One round runs immediately, then one round per Interval. onChange is called
// after a round only when at least one result actually changed, so a state flip
// can be pushed to browsers over SSE without every round broadcasting identical
// data. The first round counts as a change, so clients learn the initial
// states. onChange may be nil.
// Close releases the idle sockets the prober has pooled. A prober that is being
// replaced would otherwise keep its transport alive for the idle timeout, which
// shows up as a slow climb in goroutines on a box that reloads often.
func (p *Prober) Close() {
	if p.client != nil {
		p.client.CloseIdleConnections()
	}
}

func (p *Prober) Run(ctx context.Context, onChange func()) {
	if ctx.Err() != nil {
		return
	}
	st := p.newRound()
	p.roundOnce(ctx, st, onChange)

	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.roundOnce(ctx, st, onChange)
		}
	}
}

// Results returns the latest outcome keyed by URL. The map is a fresh copy, so
// a caller cannot race with a round in flight.
//
// URLs that have not been probed yet — and URLs dropped as unprobeable — are
// absent; callers should treat a missing entry as StateUnknown.
func (p *Prober) Results() map[string]Result {
	p.mu.Lock()
	defer p.mu.Unlock()

	out := make(map[string]Result, len(p.targets))
	for _, t := range p.targets {
		if r, ok := p.results[t]; ok {
			out[t] = r
		}
	}
	return out
}

// Snapshot returns the probe results in the shape the HTTP layer marshals to
// /api/status. It is the same data as Results, typed so *Prober satisfies the
// server's StatusSource interface.
// Snapshot returns the results to publish. allow lists the URLs the caller may
// see, and nil means no restriction.
func (p *Prober) Snapshot(allow map[string]struct{}) any {
	if allow == nil {
		return p.Results()
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	out := make(map[string]Result, len(p.targets))
	for _, t := range p.targets {
		if _, ok := allow[t]; !ok {
			continue
		}
		if r, ok := p.results[t]; ok {
			out[t] = r
		}
	}
	return out
}

// Stats reports counts for the /healthz endpoint: how many URLs are being
// probed and how many are currently up and down.
func (p *Prober) Stats() (targets, up, down int) {
	p.mu.Lock()
	defer p.mu.Unlock()

	targets = len(p.targets)
	for _, t := range p.targets {
		switch p.results[t].State {
		case StateUp:
			up++
		case StateDown:
			down++
		}
	}
	return targets, up, down
}

// roundState is the scratch storage one Run call reuses between rounds: a
// semaphore, one slot per target, and the map being filled. It belongs to the
// goroutine running Run and so needs no lock of its own.
type roundState struct {
	sem   chan struct{}
	slots []Result
	next  map[string]Result
}

// newRound returns the scratch storage reused by every round of one Run call.
func (p *Prober) newRound() *roundState {
	return &roundState{sem: make(chan struct{}, p.concurrency)}
}

// roundOnce runs a round, logs a one-line summary and reports a real change.
func (p *Prober) roundOnce(ctx context.Context, st *roundState, onChange func()) {
	start := time.Now()
	changed := p.round(ctx, st)
	if ctx.Err() != nil {
		return // shutting down: nothing worth logging or broadcasting
	}

	targets, up, down := p.Stats()
	took := time.Since(start).Round(time.Millisecond)
	if !changed {
		p.log.Debug("service status unchanged",
			"targets", targets, "up", up, "down", down, "took", took)
		return
	}
	p.log.Info("service status changed",
		"targets", targets, "up", up, "down", down, "took", took)
	if onChange != nil {
		onChange()
	}
}

// round runs one probe pass and publishes its results, reporting whether the
// result set differs from the previous round.
func (p *Prober) round(ctx context.Context, st *roundState) bool {
	p.mu.Lock()
	targets := p.targets
	prev := p.results
	p.mu.Unlock()

	// Seed every slot with what we already know, and finish the slice before
	// any goroutine starts: appending past its capacity would move the backing
	// array out from under the probes.
	st.slots = st.slots[:0]
	for _, t := range targets {
		if r, ok := prev[t]; ok {
			st.slots = append(st.slots, r)
			continue
		}
		// A target we have never reached is unknown, not down; a round that is
		// cancelled halfway leaves it that way instead of inventing a failure.
		st.slots = append(st.slots, Result{URL: t, State: StateUnknown})
	}

	var wg sync.WaitGroup
launch:
	for i, t := range targets {
		select {
		case st.sem <- struct{}{}:
		case <-ctx.Done():
			break launch
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-st.sem }()
			res := p.probe(ctx, t)
			// A round cut short by shutdown must not look like a state change.
			if ctx.Err() == nil {
				st.slots[i] = res
			}
		}()
	}
	wg.Wait()

	next := st.next
	if next == nil {
		next = make(map[string]Result, len(targets))
	} else {
		clear(next)
	}
	for i, t := range targets {
		next[t] = st.slots[i]
	}

	p.mu.Lock()
	p.results = next
	p.mu.Unlock()

	changed := resultSetChanged(prev, next)
	// The map we just replaced is ours alone to reuse: Results handed out
	// copies, and no other goroutine kept a reference to it.
	st.next = prev
	return changed
}

// resultSetChanged compares the state and code of every URL, plus the URL set
// itself. A different latency or error message is not worth waking every
// connected browser for.
func resultSetChanged(prev, next map[string]Result) bool {
	if len(prev) != len(next) {
		return true
	}
	for url, nr := range next {
		pr, ok := prev[url]
		if !ok || pr.State != nr.State || pr.Code != nr.Code {
			return true
		}
	}
	return false
}

// probe performs one URL probe: HEAD, then a single GET when HEAD is not
// supported or failed below HTTP. It is safe to call from many goroutines.
func (p *Prober) probe(ctx context.Context, target string) Result {
	start := time.Now()

	// One deadline covers both attempts, so a server that ignores HEAD cannot
	// cost two full timeouts.
	pctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	code, err := p.fetch(pctx, target, http.MethodHead)
	if pctx.Err() == nil && (err != nil || headUnsupported(code)) {
		code, err = p.fetch(pctx, target, http.MethodGet)
	}

	res := Result{
		URL:       target,
		LatencyMS: time.Since(start).Milliseconds(),
		CheckedAt: time.Now(),
	}
	if err != nil {
		// A transport failure or a timeout: nothing answered.
		res.State = StateDown
		res.Error = err.Error()
		return res
	}
	res.Code = code
	res.State = classify(code)
	return res
}

// fetch issues one request and returns its status code. The body is capped and
// discarded: a probe only needs to know that the service answered.
func (p *Prober) fetch(ctx context.Context, target, method string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := p.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	// Read a token amount so the server can finish writing, then hang up.
	_, _ = io.CopyN(io.Discard, resp.Body, maxBodyBytes)
	return resp.StatusCode, nil
}

// headUnsupported reports whether a HEAD status means "ask with GET instead"
// rather than "this service is down": plenty of frameworks reject HEAD with a
// 4xx or a 501 no matter how healthy the service is.
func headUnsupported(code int) bool {
	switch code {
	case http.StatusBadRequest, http.StatusMethodNotAllowed, http.StatusNotImplemented:
		return true
	default:
		return false
	}
}

// classify maps a status code to a state. 401 and 403 are up on purpose: a
// login page that answers means the service behind it is alive.
func classify(code int) State {
	switch {
	case code >= 200 && code < 400:
		return StateUp
	case code == http.StatusUnauthorized, code == http.StatusForbidden:
		return StateUp
	default:
		// Any other 4xx, every 5xx and anything unexpected is a service that
		// cannot serve the page.
		return StateDown
	}
}

// probeable reports whether a link can be probed at all. Anything that is not
// plain http(s) with a host — mailto:, tel:, ssh:, vscode:, or garbage — is
// left unknown instead of being reported down.
func probeable(raw string) bool {
	if raw == "" {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		return u.Host != ""
	default:
		return false
	}
}

// newClient builds the client used when Options.Client is nil. The transport is
// deliberately modest — a handful of pooled connections is plenty for internal
// services — and every probe also carries its own deadline, so a hung server
// frees its slot instead of stalling the round.
func newClient(timeout time.Duration) *http.Client {
	dial := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return errTooManyRedirects
			}
			return nil
		},
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           dial.DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          16,
			MaxIdleConnsPerHost:   2,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   timeout,
			ExpectContinueTimeout: time.Second,
		},
	}
}
