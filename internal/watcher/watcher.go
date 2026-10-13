// Package watcher pulls the navigation document from a source into a store.
//
// Two triggers feed it: a timer (the safety net) and a webhook (the fast path,
// which makes a `git push` show up in the browser within a second). Failures
// back off exponentially so a small server does not hammer a git host that is
// down or restarting.
package watcher

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/simon/ghome/internal/nav"
	"github.com/simon/ghome/internal/source"
	"github.com/simon/ghome/internal/store"
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

	// OnUpdate, when set, is called with the freshly published config. It is
	// the single hook that keeps derived state in step: the on-disk cache, the
	// icon allow-list and the set of URLs to health probe.
	OnUpdate func(*nav.Config)
}

// New builds a watcher. interval is clamped to at least one second.
func New(src source.Source, st *store.Store, interval time.Duration, log *slog.Logger) *Watcher {
	if interval < time.Second {
		interval = time.Second
	}
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

// Kick requests an immediate refresh from outside the polling loop (a webhook,
// or the manual refresh endpoint). It never blocks.
func (w *Watcher) Kick() {
	select {
	case w.kick <- struct{}{}:
	default: // a refresh is already pending
	}
}

// Run refreshes until ctx is cancelled. It blocks.
func (w *Watcher) Run(ctx context.Context) {
	var backoff time.Duration

	for {
		start := time.Now()
		err := w.refresh(ctx)
		if ctx.Err() != nil {
			return
		}

		var wait time.Duration
		if err != nil {
			backoff = w.nextBackoff(backoff)
			wait = backoff
			w.log.Warn("refresh failed, keeping the last good config",
				"error", err, "retry_in", wait.Round(time.Second))
		} else {
			backoff = 0
			wait = w.interval - time.Since(start)
			if wait < 0 {
				wait = 0
			}
		}

		kicked, ok := w.sleep(ctx, wait)
		if !ok {
			return
		}
		if kicked {
			w.coalesce(ctx)
		}
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

	fingerprint := snap.Rev + "|" + cssRev
	if w.store.Loaded() && fingerprint == w.fingerprint {
		// Clearing the error here means a recovered git host stops being
		// reported as failing on the next poll.
		w.store.SetError(nil)
		w.log.Debug("config unchanged", "revision", snap.Rev)
		return nil
	}

	w.store.Publish(cfg, snap.Data, snap.Rev, w.src.Describe(), warnings, css, cssRev)
	w.fingerprint = fingerprint

	for _, warning := range warnings {
		w.log.Warn("config warning", "detail", warning)
	}
	w.log.Info("navigation config loaded",
		"revision", snap.Rev,
		"source", w.src.Describe(),
		"groups", len(cfg.Groups),
		"links", cfg.LinkCount(),
		"warnings", len(warnings))

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
		if prev, prevRev, ok := w.store.CSS(); ok {
			w.log.Debug("keeping the previous custom css", "path", path, "error", err)
			return prev, prevRev, nil
		}
		return nil, "", []string{"style.customCss: " + err.Error()}
	}
	if len(snap.Data) > maxCustomCSS {
		return nil, "", []string{fmt.Sprintf("style.customCss: %s exceeds %d bytes, ignored", path, maxCustomCSS)}
	}
	return snap.Data, snap.Rev, nil
}
