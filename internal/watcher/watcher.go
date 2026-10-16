// Package watcher pulls the navigation document from a source into a store.
//
// Two triggers feed it: a timer (the safety net) and a webhook (the fast path,
// which makes a `git push` show up in the browser within a second). Failures
// back off exponentially so a small server does not hammer a git host that is
// down or restarting.
package watcher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/vrocwang/ghome/internal/nav"
	"github.com/vrocwang/ghome/internal/source"
	"github.com/vrocwang/ghome/internal/store"
)

const (
	// minBackoff is the first retry delay after a failure.
	minBackoff = 5 * time.Second
	// kickQuiet is how long we wait for a burst of webhooks to finish before
	// fetching. A single push can fire several.
	kickQuiet = 1200 * time.Millisecond
	// maxKickWait caps that coalescing window.
	maxKickWait = 5 * time.Second
	// maxCustomCSS bounds the sibling stylesheet. Anything larger is a
	// mistake, and reading it would waste memory on a small box.
	maxCustomCSS = 512 << 10
)

// Watcher keeps a store in sync with a source.
type Watcher struct {
	src      source.Source
	store    *store.Store
	interval time.Duration
	log      *slog.Logger
	kick     chan struct{}

	// fingerprint combines the document revision with the stylesheet revision,
	// so a change to either one reloads the page.
	fingerprint string

	// lastNavRev and lastCSSRev are the revisions of the last publish, kept only
	// so the reload log can say which file moved. A reload publishes for reasons
	// a revision cannot express (a changed warning, the first load), so they are
	// a courtesy to the operator rather than a control signal: the fingerprint
	// above is what decides whether anything is published at all.
	lastNavRev string
	lastCSSRev string

	// OnUpdate, when set, is called with the freshly published config. It is
	// the single hook that keeps derived state in step: the on-disk cache, the
	// icon allow-list and the set of URLs to health probe.
	OnUpdate func(*nav.Config)
}

// New builds a watcher.
//
// A positive interval below a second is rounded up: asking for a faster cadence
// than that is never intended. Zero or negative means no polling at all, which
// is a real mode -- refresh on webhook or manual request only -- rather than a
// very fast one. It used to be clamped up to one second along with everything
// else, so "--poll=0" quietly meant "poll every second".
func New(src source.Source, st *store.Store, interval time.Duration, log *slog.Logger) *Watcher {
	interval = EffectiveInterval(interval)
	if log == nil {
		log = slog.Default()
	}
	return &Watcher{
		src:      src,
		store:    st,
		interval: interval,
		log:      log,
		kick:     make(chan struct{}, 1),
	}
}

// EffectiveInterval is the cadence the watcher will really use. A positive
// value under a second is rounded up; zero or below means no polling at all.
// Callers that report the cadence must go through this, or they announce a
// number the watcher does not honour.
func EffectiveInterval(d time.Duration) time.Duration {
	if d > 0 && d < time.Second {
		return time.Second
	}
	return d
}

// polling reports whether Run should also re-check on a timer. When false the
// only ways to refresh are Kick and the initial fetch at startup.
func (w *Watcher) polling() bool { return w.interval > 0 }

// Kick requests an immediate refresh from outside the polling loop (a webhook,
// or the manual refresh endpoint). It never blocks.
func (w *Watcher) Kick() {
	select {
	case w.kick <- struct{}{}:
	default: // a refresh is already pending
	}
}

// Run refreshes until ctx is cancelled. It blocks.
//
// It always refreshes once before waiting, so a webhook-only watcher still has
// the current config at startup and a webhook that never arrives does not leave
// the page empty.
func (w *Watcher) Run(ctx context.Context) {
	var backoff time.Duration

	for {
		start := time.Now()
		err := w.refresh(ctx)
		if ctx.Err() != nil {
			return
		}

		var (
			wait    time.Duration
			onTimer bool
		)
		switch {
		case err != nil && w.polling():
			backoff = w.nextBackoff(backoff)
			wait, onTimer = backoff, true
			w.log.Warn("refresh failed, keeping the last good config",
				"error", err, "retry_in", wait.Round(time.Second))
		case err != nil:
			// Polling is off, so there is no timer to retry on. Saying so
			// matters: the failure would otherwise look like a hang.
			w.log.Warn("refresh failed, keeping the last good config; "+
				"polling is off, so this waits for the next push", "error", err)
		case w.polling():
			backoff = 0
			wait, onTimer = max(w.interval-time.Since(start), 0), true
		default:
			backoff = 0 // polling is off: wait for a kick instead
		}

		var kicked bool
		if onTimer {
			var ok bool
			if kicked, ok = w.sleep(ctx, wait); !ok {
				return
			}
		} else if !w.waitForKick(ctx) {
			return
		} else {
			kicked = true
		}
		if kicked {
			w.coalesce(ctx)
		}
	}
}

// waitForKick blocks until something asks for a refresh, or ctx is done. It is
// what the loop does in place of sleeping when polling is switched off.
func (w *Watcher) waitForKick(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case <-w.kick:
		return true
	}
}

// nextBackoff doubles the retry delay after each consecutive failure, capped at
// the poll interval. The cap matters: an operator who asked for a 2s cadence
// should not be made to wait 5s because of a floor meant for slow cadences.
func (w *Watcher) nextBackoff(current time.Duration) time.Duration {
	if current <= 0 {
		current = minBackoff
	} else {
		current *= 2
	}
	if current > w.interval {
		return w.interval
	}
	return current
}

// coalesce waits for the webhook burst to go quiet so one push causes one
// fetch rather than five.
func (w *Watcher) coalesce(ctx context.Context) {
	deadline := time.Now().Add(maxKickWait)
	for {
		quiet := kickQuiet
		if remaining := time.Until(deadline); remaining < quiet {
			quiet = remaining
		}
		if quiet <= 0 {
			return
		}
		timer := time.NewTimer(quiet)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-w.kick:
			timer.Stop() // another push arrived, restart the quiet window
		case <-timer.C:
			return
		}
	}
}

// sleep waits for d, returning early on ctx cancellation or a kick. The second
// result is false when the context is done.
func (w *Watcher) sleep(ctx context.Context, d time.Duration) (kicked bool, ok bool) {
	if d <= 0 {
		select {
		case <-ctx.Done():
			return false, false
		case <-w.kick:
			return true, true
		default:
			return false, true
		}
	}

	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false, false
	case <-timer.C:
		return false, true
	case <-w.kick:
		return true, true
	}
}

// changedFiles names the files this reload picked up, for the log line an
// operator reads after a push: "config reloaded: custom.css" says what happened,
// where a blob SHA only says that something did.
//
// The names are the configured short ones (nav.yaml, custom.css) rather than
// paths, because that is how they are written in nav.yaml and how the operator
// thinks of them. cssPath is the stylesheet the config being loaded names, not
// the one still in the store: a reload can be the one that introduces it.
//
// It compares against lastNavRev and lastCSSRev and changes nothing, so the
// caller owns the state it reads -- reporting on a reload must not be what
// decides what the next reload is compared against.
func (w *Watcher) changedFiles(navRev, cssRev, cssPath string) string {
	docName := w.docName()
	cssName := baseName(cssPath)

	// A process that has never published has nothing to compare against: report
	// both files rather than claim only one of them was touched.
	if w.fingerprint == "" {
		names := []string{docName}
		if cssName != "" {
			names = append(names, cssName)
		}
		return strings.Join(names, ", ")
	}

	var changed []string
	if navRev != w.lastNavRev {
		changed = append(changed, docName)
	}
	if cssRev != w.lastCSSRev && cssName != "" {
		changed = append(changed, cssName)
	}
	if len(changed) == 0 {
		// Published for a reason no revision can show: a warning appeared or
		// went away while both files stayed as they were.
		return "warnings changed"
	}
	return strings.Join(changed, ", ")
}

// docName is the configured name of the navigation document: "nav.yaml" inside
// the repository, or the base name of the path in file mode.
func (w *Watcher) docName() string {
	if multi, ok := w.src.(source.MultiFile); ok {
		if name := baseName(multi.NavPath()); name != "" {
			return name
		}
	}
	return "nav.yaml"
}

func baseName(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// fingerprintOf is the change marker the early return compares. It covers the
// document revision, the stylesheet revision and the warnings, so exactly one
// publish happens per real change and none per poll. Warnings are ordered
// deterministically by the parser, so the same conditions produce the same
// marker.
func fingerprintOf(rev, cssRev string, warnings []string) string {
	sum := sha256.Sum256([]byte(rev + "|" + cssRev + "|" + strings.Join(warnings, "\n")))
	return hex.EncodeToString(sum[:12])
}

// refresh performs one fetch and publishes it if anything changed.
func (w *Watcher) refresh(ctx context.Context) error {
	snap, err := w.src.Fetch(ctx)
	if err != nil {
		w.store.SetError(err)
		return err
	}

	// The document is parsed even when its revision looks unchanged, because
	// the config is what names the sibling stylesheet: nothing can be declared
	// unchanged until that reference has been followed. Parsing a few KB is
	// far cheaper than the request that produced it.
	cfg, warnings, err := nav.Parse(snap.Data)
	if err != nil {
		w.store.SetError(err)
		return err
	}

	css, cssRev, extraWarnings := w.fetchExtras(ctx, cfg)
	warnings = append(warnings, extraWarnings...)

	fingerprint := fingerprintOf(snap.Rev, cssRev, warnings)
	// The warnings take part, rather than the check being skipped whenever any
	// of them exists. A kept-back stylesheet leaves the document and the
	// stylesheet revision alone -- that is the point of keeping it -- so the
	// warning has to be able to move the fingerprint on its own, or it would
	// never reach the store. Republishing on every poll instead (which is what
	// "any warning means republish" amounts to) moved UpdatedAt and woke every
	// connected browser once per interval for a config that had not changed.
	if w.store.Loaded() && fingerprint == w.fingerprint {
		// Clearing the error here means a recovered git host stops being
		// reported as failing on the next poll.
		w.store.SetError(nil)
		w.log.Debug("config unchanged", "revision", snap.Rev)
		return nil
	}

	published := w.store.Publish(cfg, snap.Data, snap.Rev, w.src.Describe(), warnings, css, cssRev)

	// Both of these read the state the previous reload left behind, so they run
	// before this one overwrites it.
	changed := w.changedFiles(snap.Rev, cssRev, cfg.Style.CustomCSS)
	w.fingerprint = fingerprint
	w.lastNavRev, w.lastCSSRev = snap.Rev, cssRev

	for _, warning := range warnings {
		w.log.Warn("config warning", "detail", warning)
	}
	// The info line names the file that changed, because that is the question an
	// operator has after a push. The revisions answer a different one -- "which
	// commit is live" -- and a blob SHA never said which file had moved anyway:
	// a push that touched only custom.css left the document revision identical
	// to the previous reload, so the line looked like nothing had happened.
	w.log.Info("config reloaded: "+changed,
		"groups", len(cfg.Groups),
		"links", cfg.LinkCount(),
		"warnings", len(warnings))
	// The identifiers stay, one level down: this is the line to ask for when the
	// question is "is the running binary serving the commit I just pushed".
	w.log.Debug("config revision details",
		"revision", snap.Rev,
		"theme_revision", published.Meta.ThemeRevision,
		"source", w.src.Describe())

	if w.OnUpdate != nil {
		w.OnUpdate(cfg)
	}
	return nil
}

// fetchExtras reads the optional stylesheet named by style.customCss.
//
// A failure here is a warning, never an error: a missing stylesheet must not
// stop the navigation page itself from loading.
func (w *Watcher) fetchExtras(ctx context.Context, cfg *nav.Config) (css []byte, rev string, warnings []string) {
	path := cfg.Style.CustomCSS
	if path == "" {
		return nil, "", nil
	}

	multi, ok := w.src.(source.MultiFile)
	if !ok {
		return nil, "", []string{"style.customCss: this source cannot read sibling files, ignored"}
	}

	snap, err := multi.FetchPath(ctx, path)
	if err != nil {
		// Keep serving the copy already in the store rather than dropping the
		// styling over a transient git failure.
		if _, prev, prevRev, ok := w.store.GetWithCSS(); ok && prev != nil {
			// Keep styling rather than dropping it over what may be a transient
			// failure -- but report it. Staying quiet makes a permanently wrong
			// path indistinguishable from a working one: nav.yaml names one file
			// while /theme.css serves another, indefinitely, with no warning
			// anywhere for an operator to notice.
			w.log.Warn("keeping the previous custom css", "path", path, "error", err)
			return prev, prevRev, []string{fmt.Sprintf(
				"style.customCss: %s could not be read (%v); still serving the previous stylesheet",
				path, err)}
		}
		return nil, "", []string{"style.customCss: " + err.Error()}
	}
	if len(snap.Data) > maxCustomCSS {
		// Same trade as a failed read above: an unusable stylesheet is a reason
		// to warn, not a reason to strip the styling off a live page. Silently
		// returning nil here deleted it.
		warning := fmt.Sprintf("style.customCss: %s exceeds %d bytes, ignored", path, maxCustomCSS)
		if _, prev, prevRev, ok := w.store.GetWithCSS(); ok && prev != nil {
			w.log.Warn("keeping the previous custom css", "path", path,
				"limit", maxCustomCSS, "size", len(snap.Data))
			return prev, prevRev, []string{warning + "; still serving the previous stylesheet"}
		}
		return nil, "", []string{warning}
	}
	return snap.Data, snap.Rev, nil
}
