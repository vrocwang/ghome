package icons

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// MaxEntries alone bounds nothing useful: 512 entries of the maximum icon size
// is a quarter of a gigabyte, which is not what a cache described as "a few
// megabytes" should be able to reach on a box that otherwise idles at twelve.
func TestCacheStaysWithinItsByteBudget(t *testing.T) {
	// A budget that fits only a handful of these icons.
	const iconBytes = 64 << 10
	const budget = 256 << 10 // four icons

	r := testResolver(t, Options{MaxEntries: 1000, MaxTotalBytes: budget})
	r.Allow([]string{"https://a.example.com"})

	for i := 0; i < 40; i++ {
		key := fmt.Sprintf("key-%d", i)
		r.put(key, Icon{Data: make([]byte, iconBytes), ContentType: "image/png"}, true, time.Hour)
	}

	r.mu.Lock()
	got := r.bytes
	entries := r.lru.Len()
	r.mu.Unlock()

	if got > budget {
		t.Errorf("cached bytes = %d, want at most %d", got, budget)
	}
	if entries > 5 {
		t.Errorf("entries = %d, want the byte budget to have evicted most of the 40", entries)
	}
}

// An oversized entry must not be able to wedge the cache: it is refused by
// MaxBytes long before it reaches put, but the eviction loop must still
// terminate rather than spinning on an entry it cannot evict.
func TestCacheSurvivesAnEntryLargerThanTheBudget(t *testing.T) {
	r := testResolver(t, Options{MaxEntries: 10, MaxTotalBytes: 1024})
	r.put("small", Icon{Data: make([]byte, 100), ContentType: "image/png"}, true, time.Hour)

	done := make(chan struct{})
	go func() {
		defer close(done)
		r.put("huge", Icon{Data: make([]byte, 4096), ContentType: "image/png"}, true, time.Hour)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("put() did not return: the eviction loop is spinning")
	}

	// A miss still costs an entry and no bytes, so it must remain storable.
	r.put("miss", Icon{}, false, time.Hour)
	if _, _, fresh := r.get("miss"); !fresh {
		t.Error("a cached miss was not retained")
	}
}

// The per-call worker cap does not bound the total on its own: every config
// reload starts its own pool, so the number of simultaneous upstream requests
// climbs with the number of reloads. A shared semaphore is what actually bounds
// it.
func TestWarmFetchesAreBoundedAcrossCalls(t *testing.T) {
	var mu sync.Mutex
	live, peak := 0, 0

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		live++
		if live > peak {
			peak = live
		}
		mu.Unlock()

		time.Sleep(30 * time.Millisecond) // hold the slot so overlap is observable
		mu.Lock()
		live--
		mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/favicon.ico") {
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(pngBytes)
			return
		}
		_, _ = w.Write([]byte("<html></html>"))
	}))
	defer upstream.Close()

	r := testResolver(t, Options{})
	host := strings.TrimPrefix(upstream.URL, "http://")

	// Ten overlapping Warm calls, each with its own uncached url.
	for call := 0; call < 10; call++ {
		urls := make([]string, 0, 8)
		for i := 0; i < 8; i++ {
			urls = append(urls, fmt.Sprintf("http://%s/c%d-%d", host, call, i))
		}
		r.Allow(urls)
		r.Warm(context.Background(), urls)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		done := live == 0 && peak > 0
		mu.Unlock()
		if done {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	mu.Lock()
	got := peak
	mu.Unlock()
	if got > warmWorkers {
		t.Errorf("peak concurrent upstream requests = %d, want at most warmWorkers (%d)", got, warmWorkers)
	}
	if got == 0 {
		t.Error("no upstream request was made; the test proves nothing")
	}
}
