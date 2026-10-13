package icons

import (
	"fmt"
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
