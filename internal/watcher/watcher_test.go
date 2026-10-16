package watcher

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vrocwang/ghome/internal/nav"
	"github.com/vrocwang/ghome/internal/source"
	"github.com/vrocwang/ghome/internal/store"
)

const (
	docA = "groups:\n  - name: A\n    links:\n      - name: One\n        url: https://one.example.com\n"
	docB = "groups:\n  - name: B\n    links:\n      - name: Two\n        url: https://two.example.com\n"
)

// fakeSource stands in for Gitea. It records how many times it was polled so a
// test can prove that unchanged content is skipped.
type fakeSource struct {
	mu    sync.Mutex
	data  []byte
	rev   string
	err   error
	calls int
}

func (f *fakeSource) Fetch(context.Context) (source.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return source.Snapshot{}, f.err
	}
	return source.Snapshot{Data: f.data, Rev: f.rev}, nil
}

func (f *fakeSource) Describe() string { return "fake" }

func (f *fakeSource) set(data, rev string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data, f.rev = []byte(data), rev
}

func (f *fakeSource) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeSource) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

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

func revision(t *testing.T, st *store.Store) string {
	t.Helper()
	payload, ok := st.Get()
	if !ok {
		return ""
	}
	return payload.Meta.Revision
}

// TestRunLoadsThenKickRefreshes exercises the real control flow: an immediate
// first fetch, then a webhook driven reload well before the poll interval.
func TestRunLoadsThenKickRefreshes(t *testing.T) {
	src := &fakeSource{data: []byte(docA), rev: "rev-a"}
	st := store.New()
	// An hour long interval means only the kick can cause the second load.
	w := New(src, st, time.Hour, discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx)
	}()

	waitFor(t, 3*time.Second, "the first load", func() bool { return st.Loaded() })
	if got := revision(t, st); got != "rev-a" {
		t.Fatalf("revision = %q, want rev-a", got)
	}

	src.set(docB, "rev-b")
	w.Kick()

	waitFor(t, 5*time.Second, "the kick driven reload", func() bool {
		return revision(t, st) == "rev-b"
	})

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run() did not return after the context was cancelled")
	}
}

func TestRefreshSkipsUnchangedContent(t *testing.T) {
	src := &fakeSource{data: []byte(docA), rev: "rev-a"}
	st := store.New()
	w := New(src, st, time.Minute, discardLogger())

	updates := 0
	w.OnUpdate = func(*nav.Config) { updates++ }

	for i := 0; i < 3; i++ {
		if err := w.refresh(context.Background()); err != nil {
			t.Fatalf("refresh() error = %v", err)
		}
	}

	if src.count() != 3 {
		t.Errorf("Fetch() ran %d times, want 3", src.count())
	}
	if updates != 1 {
		t.Errorf("OnUpdate ran %d times, want 1: an unchanged revision must not reload", updates)
	}
}

func TestRefreshHandlesNewContent(t *testing.T) {
	src := &fakeSource{data: []byte(docA), rev: "rev-a"}
	st := store.New()
	w := New(src, st, time.Minute, discardLogger())

	if err := w.refresh(context.Background()); err != nil {
		t.Fatalf("refresh() error = %v", err)
	}
	src.set(docB, "rev-b")
	if err := w.refresh(context.Background()); err != nil {
		t.Fatalf("refresh() error = %v", err)
	}

	payload, _ := st.Get()
	if payload.Meta.Revision != "rev-b" {
		t.Errorf("revision = %q, want rev-b", payload.Meta.Revision)
	}
	if payload.Groups[0].Name != "B" {
		t.Errorf("group = %q, want B", payload.Groups[0].Name)
	}
	if st.LastError() != nil {
		t.Errorf("LastError() = %v, want nil", st.LastError())
	}
}

func TestRefreshKeepsTheLastGoodDocumentWhenFetchingFails(t *testing.T) {
	src := &fakeSource{data: []byte(docA), rev: "rev-a"}
	st := store.New()
	w := New(src, st, time.Minute, discardLogger())

	if err := w.refresh(context.Background()); err != nil {
		t.Fatalf("refresh() error = %v", err)
	}

	src.setErr(errors.New("connection refused"))
	if err := w.refresh(context.Background()); err == nil {
		t.Fatal("refresh() reported success while the source was failing")
	}

	if got := revision(t, st); got != "rev-a" {
		t.Errorf("revision = %q, want rev-a: a fetch failure must not clear the page", got)
	}
	if st.LastError() == nil {
		t.Error("LastError() = nil, want the fetch failure")
	}
}

func TestRefreshKeepsTheLastGoodDocumentWhenTheConfigIsInvalid(t *testing.T) {
	src := &fakeSource{data: []byte(docA), rev: "rev-a"}
	st := store.New()
	w := New(src, st, time.Minute, discardLogger())

	if err := w.refresh(context.Background()); err != nil {
		t.Fatalf("refresh() error = %v", err)
	}

	src.set("groups: [", "rev-bad")
	if err := w.refresh(context.Background()); err == nil {
		t.Fatal("refresh() accepted an unparseable document")
	}
	if got := revision(t, st); got != "rev-a" {
		t.Errorf("revision = %q, want rev-a: a broken push must not go live", got)
	}
	if st.LastError() == nil {
		t.Error("LastError() = nil, want the parse failure")
	}
}

func TestRefreshRecoversFromAFailure(t *testing.T) {
	src := &fakeSource{data: []byte(docA), rev: "rev-a", err: errors.New("git is down")}
	st := store.New()
	w := New(src, st, time.Minute, discardLogger())

	_ = w.refresh(context.Background())
	if st.LastError() == nil {
		t.Fatal("LastError() = nil, want the fetch failure")
	}

	src.setErr(nil)
	if err := w.refresh(context.Background()); err != nil {
		t.Fatalf("refresh() error = %v", err)
	}
	if st.LastError() != nil {
		t.Errorf("LastError() = %v, want nil once the source recovers", st.LastError())
	}
}

func TestNewClampsATinyInterval(t *testing.T) {
	// A small positive interval is rounded up; asking for faster than a second
	// is never intended.
	w := New(&fakeSource{}, store.New(), 250*time.Millisecond, discardLogger())
	if w.interval != time.Second {
		t.Errorf("interval = %v, want 1s", w.interval)
	}
	if !w.polling() {
		t.Error("polling() = false for a positive interval")
	}
}

// Zero used to be clamped up to one second along with everything else, so
// "--poll=0" quietly polled once a second -- the opposite of switching polling
// off, and the worst possible reading of what the operator asked for.
func TestNewTreatsZeroAndNegativeAsPollingOff(t *testing.T) {
	for _, interval := range []time.Duration{0, -time.Second} {
		w := New(&fakeSource{}, store.New(), interval, discardLogger())
		if w.polling() {
			t.Errorf("interval %v: polling() = true, want false", interval)
		}
	}
}

// With polling off, startup still loads once and then nothing happens on a
// timer. The 2.5s wait is well past the one second zero used to become, so a
// surviving timer would show up as several extra calls.
func TestPollingOffRefreshesOnceThenWaitsForAKick(t *testing.T) {
	src := &fakeSource{data: []byte(docA), rev: "rev-a"}
	st := store.New()
	w := New(src, st, 0, discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); w.Run(ctx) }()

	waitFor(t, 3*time.Second, "the first load", func() bool { return st.Loaded() })

	time.Sleep(2500 * time.Millisecond)
	if got := src.count(); got != 1 {
		t.Errorf("Fetch called %d times in 2.5s with polling off, want 1", got)
	}

	src.set(docB, "rev-b")
	w.Kick()
	waitFor(t, 5*time.Second, "the kick driven reload", func() bool {
		return revision(t, st) == "rev-b"
	})
	if got := src.count(); got != 2 {
		t.Errorf("Fetch called %d times after one kick, want 2", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run() did not return after the context was cancelled")
	}
}

// A failure with polling off must wait for a kick rather than retrying. A tight
// loop here would hit the forge far harder than any poll interval ever would,
// which would make the "off" switch the most expensive setting available.
func TestPollingOffDoesNotRetryInALoopWhenFailing(t *testing.T) {
	src := &fakeSource{err: errors.New("forge is down")}
	w := New(src, store.New(), 0, discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	time.Sleep(1500 * time.Millisecond)
	if got := src.count(); got != 1 {
		t.Errorf("Fetch called %d times in 1.5s while failing with polling off, want 1", got)
	}
	cancel()
}

func TestBackoffNeverExceedsThePollInterval(t *testing.T) {
	// A fast cadence wins over the backoff floor.
	fast := New(&fakeSource{}, store.New(), 2*time.Second, discardLogger())
	if got := fast.nextBackoff(0); got != 2*time.Second {
		t.Errorf("nextBackoff(0) = %v, want the 2s interval", got)
	}
	if got := fast.nextBackoff(time.Second); got != 2*time.Second {
		t.Errorf("nextBackoff(1s) = %v, want the 2s interval", got)
	}

	// A slow cadence ramps up from the floor instead of jumping to 60s.
	slow := New(&fakeSource{}, store.New(), time.Minute, discardLogger())
	if got := slow.nextBackoff(0); got != minBackoff {
		t.Errorf("nextBackoff(0) = %v, want %v", got, minBackoff)
	}
	if got := slow.nextBackoff(minBackoff); got != 2*minBackoff {
		t.Errorf("nextBackoff(%v) = %v, want %v", minBackoff, got, 2*minBackoff)
	}
	if got := slow.nextBackoff(40 * time.Second); got != time.Minute {
		t.Errorf("nextBackoff(40s) = %v, want the 1m interval", got)
	}
}

// TestRunRecoversQuicklyAfterAFailure guards the case that matters in practice:
// a short outage should not leave the page stale for a whole backoff window.
func TestRunRecoversQuicklyAfterAFailure(t *testing.T) {
	src := &fakeSource{err: errors.New("git is down")}
	st := store.New()
	w := New(src, st, 2*time.Second, discardLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	waitFor(t, 3*time.Second, "the first failure", func() bool { return st.LastError() != nil })

	src.setErr(nil)
	src.set(docA, "rev-a")
	waitFor(t, 4*time.Second, "recovery", func() bool { return st.Loaded() })
}

// multiSource is a fakeSource that can also read sibling files, which is what
// style.customCss needs.
type multiSource struct {
	fakeSource
	css    []byte
	cssErr error
}

func (m *multiSource) FetchPath(context.Context, string) (source.Snapshot, error) {
	if m.cssErr != nil {
		return source.Snapshot{}, m.cssErr
	}
	if m.css == nil {
		return source.Snapshot{}, errors.New("no such file")
	}
	return source.Snapshot{Data: m.css, Rev: "css-rev"}, nil
}

func (m *multiSource) setCSS(css []byte) { m.css = css }

const docWithCSS = "style:\n  customCss: \"custom.css\"\ngroups:\n  - name: A\n    links:\n      - name: One\n        url: https://one.example.com\n"

// An unusable stylesheet is a reason to warn, not a reason to strip the styling
// off a live page. A failed read already kept the previous copy; an oversized
// one used to delete it instead.
func TestOversizedStylesheetKeepsThePreviousCopy(t *testing.T) {
	src := &multiSource{css: []byte(".link{color:red}\n")}
	src.set(docWithCSS, "rev-a")
	st := store.New()
	w := New(src, st, time.Minute, discardLogger())

	if err := w.refresh(context.Background()); err != nil {
		t.Fatalf("refresh() error = %v", err)
	}
	if _, css, _, _ := st.GetWithCSS(); !strings.Contains(string(css), "color:red") {
		t.Fatalf("the first stylesheet did not load: css=%q", css)
	}

	// Now the file grows past the limit. The config itself is unchanged.
	src.setCSS([]byte(strings.Repeat("a", maxCustomCSS+1)))
	if err := w.refresh(context.Background()); err != nil {
		t.Fatalf("refresh() error = %v", err)
	}

	if _, css, _, _ := st.GetWithCSS(); !strings.Contains(string(css), "color:red") {
		t.Errorf("an oversized stylesheet wiped the live one: len=%d", len(css))
	}
}

// The same reasoning for a read failure, which was already correct: pin it so
// the two paths cannot drift apart again.
func TestFailedStylesheetReadKeepsThePreviousCopy(t *testing.T) {
	src := &multiSource{css: []byte(".link{color:red}\n")}
	src.set(docWithCSS, "rev-a")
	st := store.New()
	w := New(src, st, time.Minute, discardLogger())

	if err := w.refresh(context.Background()); err != nil {
		t.Fatalf("refresh() error = %v", err)
	}
	src.cssErr = errors.New("404: custom.css not found")
	src.set(docWithCSS, "rev-b")
	if err := w.refresh(context.Background()); err != nil {
		t.Fatalf("refresh() error = %v", err)
	}

	if _, css, _, _ := st.GetWithCSS(); !strings.Contains(string(css), "color:red") {
		t.Errorf("a failed read wiped the live stylesheet: len=%d", len(css))
	}
}

// A kept-back stylesheet leaves the fingerprint alone -- that is the point of
// keeping it -- so an unchanged-fingerprint early return would keep the new
// warning out of the store, and out of the page banner, forever.
func TestWarningsReachTheStoreEvenWhenNothingElseChanged(t *testing.T) {
	src := &multiSource{css: []byte(".link{color:red}\n")}
	src.set(docWithCSS, "rev-a")
	st := store.New()
	w := New(src, st, time.Minute, discardLogger())

	if err := w.refresh(context.Background()); err != nil {
		t.Fatalf("refresh() error = %v", err)
	}
	if w := mustWarnings(t, st); len(w) != 0 {
		t.Fatalf("warnings = %v, want none after a clean load", w)
	}

	// The same document and the same stylesheet revision, but now the file is
	// too big to use, so the previous copy stays in service and a warning exists.
	src.setCSS([]byte(strings.Repeat("a", maxCustomCSS+1)))
	if err := w.refresh(context.Background()); err != nil {
		t.Fatalf("refresh() error = %v", err)
	}

	warnings := mustWarnings(t, st)
	if len(warnings) == 0 {
		t.Fatal("the oversized-stylesheet warning never reached the store")
	}
	if _, css, _, _ := st.GetWithCSS(); !strings.Contains(string(css), "color:red") {
		t.Errorf("the previous stylesheet should still be in service: css=%q", css)
	}
}

// ...and a warning that stays the same must not republish on every poll.
// "There are warnings, so publish" moved UpdatedAt once per interval and sent
// every connected browser a "changed" event for a config nobody had edited.
func TestAStableWarningIsNotRepublished(t *testing.T) {
	src := &multiSource{css: []byte(".link{color:red}\n")}
	src.set(docWithCSS, "rev-a")
	st := store.New()
	w := New(src, st, time.Minute, discardLogger())

	if err := w.refresh(context.Background()); err != nil {
		t.Fatalf("refresh() error = %v", err)
	}
	// Oversized now, so the previous copy stays in service and a warning exists.
	src.setCSS([]byte(strings.Repeat("a", maxCustomCSS+1)))
	if err := w.refresh(context.Background()); err != nil {
		t.Fatalf("refresh() error = %v", err)
	}
	if len(mustWarnings(t, st)) == 0 {
		t.Fatal("the warning under test never appeared")
	}

	updates := 0
	w.OnUpdate = func(*nav.Config) { updates++ }
	before, _ := st.Get()

	for i := 0; i < 3; i++ {
		if err := w.refresh(context.Background()); err != nil {
			t.Fatalf("refresh() error = %v", err)
		}
	}

	if updates != 0 {
		t.Errorf("OnUpdate ran %d times for an unchanged config, want 0", updates)
	}
	if after, _ := st.Get(); !after.Meta.UpdatedAt.Equal(before.Meta.UpdatedAt) {
		t.Errorf("updatedAt moved from %v to %v although nothing changed",
			before.Meta.UpdatedAt, after.Meta.UpdatedAt)
	}
}

func mustWarnings(t *testing.T, st *store.Store) []string {
	t.Helper()
	payload, ok := st.Get()
	if !ok {
		t.Fatal("the store holds nothing")
	}
	return payload.Meta.Warnings
}
