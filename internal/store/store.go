// Package store holds the newest navigation document that parsed cleanly and
// lets HTTP handlers watch for updates.
//
// The key property: a document that fails to parse never replaces a good one.
// A bad push therefore leaves the live page untouched while its author fixes
// nav.yaml.
package store

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vrocwang/ghome/internal/nav"
)

// Store is a concurrency safe holder for the current document.
type Store struct {
	mu      sync.RWMutex
	payload nav.Payload
	raw     []byte
	css     []byte
	cssRev  string
	loaded  bool
	lastErr error

	subsMu sync.Mutex
	subs   map[int]chan struct{}
	nextID int

	// changes counts notifications. A client that reconnects asks for it and
	// compares: "did anything happen while I was not listening?" is otherwise
	// unanswerable, because a change to custom.css alone leaves the document
	// revision alone and a probe round leaves both alone.
	changes atomic.Int64

	now func() time.Time
}

// New returns an empty store.
func New() *Store {
	return &Store{
		subs: make(map[int]chan struct{}),
		now:  time.Now,
	}
}

// Publish installs an already parsed document as the current one.
//
// The watcher parses and calls this directly, rather than Load, because it has
// to inspect the config first: style.customCss names a sibling file it must
// fetch and hand over in the same atomic step.
//
// css and cssRev carry the optional stylesheet; pass nil and "" when there is
// none.
func (s *Store) Publish(cfg *nav.Config, raw []byte, rev, src string, warnings []string, css []byte, cssRev string) {
	s.mu.Lock()
	s.raw = raw
	s.css = css
	s.cssRev = cssRev
	s.payload = nav.Payload{
		Config: *cfg,
		Meta: nav.Meta{
			Revision:      rev,
			ThemeRevision: themeRevision(rev, cssRev),
			UpdatedAt:     s.now().UTC(),
			Source:        src,
			GroupCount:    len(cfg.Groups),
			LinkCount:     cfg.LinkCount(),
			Warnings:      warnings,
		},
	}
	s.loaded = true
	s.lastErr = nil
	s.mu.Unlock()

	s.notify()
}

// Load parses raw and publishes it. It is used to seed the store from the cache
// file and by tests.
//
// css carries the stylesheet cached alongside the config: css[0] is its bytes
// and css[1] its revision. It is variadic so that seeding a store with a
// document alone stays the short form. Installing both in one Publish is what
// keeps a restored store from serving the page with the styling missing.
func (s *Store) Load(raw []byte, rev, src string, css ...string) error {
	cfg, warnings, err := nav.Parse(raw)
	if err != nil {
		s.SetError(err)
		return err
	}
	var cssData []byte
	var cssRev string
	if len(css) >= 2 {
		cssData, cssRev = []byte(css[0]), css[1]
	}
	s.Publish(cfg, raw, rev, src, warnings, cssData, cssRev)
	return nil
}

// Get returns the current document. ok is false until the first successful
// load. The returned payload is read-only: it is never mutated in place.
func (s *Store) Get() (nav.Payload, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.payload, s.loaded
}

// Raw returns the exact bytes of the current document, for the cache file.
func (s *Store) Raw() []byte {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.raw
}

// CSS returns the stylesheet fetched from the config repository, if any.
func (s *Store) CSS() ([]byte, string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.css == nil {
		return nil, "", false
	}
	return s.css, s.cssRev, true
}

// Loaded reports whether any document has ever loaded successfully.
func (s *Store) Loaded() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.loaded
}

// GetWithCSS returns the document and its stylesheet from one revision, under a
// single lock.
//
// Reading them with Get and CSS in turn can mix them: a publish between the two
// calls hands out one revision's tokens next to the next revision's stylesheet.
// The document and the stylesheet are installed together, so they have to be
// read together.
func (s *Store) GetWithCSS() (nav.Payload, []byte, string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.payload, s.css, s.cssRev, s.loaded
}

// LastError returns the most recent fetch or parse failure, if any.
func (s *Store) LastError() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastErr
}

// SetError records a failure. Passing nil clears it.
func (s *Store) SetError(err error) {
	s.mu.Lock()
	s.lastErr = err
	s.mu.Unlock()
}

// Subscribe returns a channel that receives a value whenever the document
// changes. The channel is buffered with capacity one and updates are dropped
// rather than queued, so a slow reader can never block a refresh: for a
// navigation page the newest state is the only one worth showing.
//
// Call the returned function to unsubscribe.
func (s *Store) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)

	s.subsMu.Lock()
	id := s.nextID
	s.nextID++
	s.subs[id] = ch
	s.subsMu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			s.subsMu.Lock()
			delete(s.subs, id)
			s.subsMu.Unlock()
			close(ch)
		})
	}
	return ch, cancel
}

// Notify wakes every subscriber without changing the document. The health
// prober uses it to push fresh probe results over the same event stream.
func (s *Store) Notify() { s.notify() }

// Changes counts the notifications broadcast so far. It is what a reconnecting
// event stream compares to decide whether it has to re-read: the document
// revision only moves for a nav.yaml edit, so a stylesheet-only push or a probe
// result that flipped while nobody was listening would otherwise be missed
// forever.
func (s *Store) Changes() int64 { return s.changes.Load() }

func (s *Store) notify() {
	s.changes.Add(1)

	s.subsMu.Lock()
	defer s.subsMu.Unlock()
	for _, ch := range s.subs {
		select {
		case ch <- struct{}{}:
		default: // a pending notification is enough
		}
	}
}

// SubscriberCount reports how many listeners are attached, for /healthz.
func (s *Store) SubscriberCount() int {
	s.subsMu.Lock()
	defer s.subsMu.Unlock()
	return len(s.subs)
}

// themeRevision derives a short marker covering both the document and its
// stylesheet, so a change to either one is visible to the browser as a new
// cache key for /theme.css.
func themeRevision(navRev, cssRev string) string {
	sum := sha256.Sum256([]byte(navRev + "|" + cssRev))
	return "t" + hex.EncodeToString(sum[:6])
}
