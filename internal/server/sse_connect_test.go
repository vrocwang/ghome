package server

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vrocwang/ghome/internal/nav"
)

// blockingFlusher holds the handler inside its first Flush, which is exactly the
// window between writing "hello" and entering the event loop.
type blockingFlusher struct {
	header http.Header

	mu   sync.Mutex
	buf  bytes.Buffer
	code int

	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func newBlockingFlusher() *blockingFlusher {
	return &blockingFlusher{
		header:  http.Header{},
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (b *blockingFlusher) Header() http.Header { return b.header }

func (b *blockingFlusher) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *blockingFlusher) WriteHeader(code int) { b.code = code }

// Flush blocks the first time and returns immediately afterwards, so the test
// controls the window without wedging the event loop.
func (b *blockingFlusher) Flush() {
	b.once.Do(func() {
		close(b.entered)
		<-b.release
	})
}

func (b *blockingFlusher) body() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// An update published while the handler is between its snapshot and its
// subscription used to be announced to nobody. The client's hello carried the
// revision it already had, the watcher does not publish the same fingerprint
// twice, and a healthy stream never reconnects -- so the page stayed on the old
// revision indefinitely. Subscribing first is what closes the window.
func TestEventStreamDoesNotLoseAnUpdateDuringConnect(t *testing.T) {
	st := testStore(t)
	srv, _ := newTestServer(t, Options{Store: st})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/events", nil).WithContext(ctx)

	w := newBlockingFlusher()
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.handleEvents(w, req)
	}()

	select {
	case <-w.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("the handler never reached its first flush")
	}

	// Publish while the handler is parked in that window.
	cfg, _, err := nav.Parse([]byte(goodDoc))
	if err != nil {
		t.Fatalf("parse the fixture: %v", err)
	}
	st.Publish(cfg, []byte(goodDoc), "rev-2", "test", nil, nil, "css-rev-2")
	close(w.release)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(w.body(), "event: changed") {
			cancel()
			<-done
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	t.Errorf("stream never carried a changed event; body was:\n%s", w.body())
}

// The stream's first frame has to say whether anything happened that this client
// has not seen. The document revision cannot say it: a push that touched only
// custom.css moves themeRevision, and a probe round moves neither. A client
// comparing revisions alone keeps a stale stylesheet or stale status lights
// until something unrelated happens, and its fallback poll is already stopped.
func TestEventStreamHelloCarriesTheNotificationCounter(t *testing.T) {
	st := testStore(t)
	srv, _ := newTestServer(t, Options{Store: st})

	want := st.Changes() + 1
	st.Notify() // the prober's path: no document change at all

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/events", nil).WithContext(ctx)

	w := newBlockingFlusher()
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.handleEvents(w, req)
	}()

	select {
	case <-w.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("the handler never reached its first flush")
	}
	body := w.body()
	close(w.release)
	cancel()
	<-done

	if !strings.Contains(body, "event: hello") {
		t.Fatalf("no hello frame:\n%s", body)
	}
	if marker := fmt.Sprintf(`"changes":%d`, want); !strings.Contains(body, marker) {
		t.Errorf("hello = %q, want it to carry %s", body, marker)
	}
}
