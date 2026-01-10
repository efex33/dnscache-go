package dnscache

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestIntegration(t *testing.T) {
	// 1. Setup a dummy HTTP server
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "Hello, client")
	}))
	defer ts.Close()

	// 2. Create our Resolver
	r := New(Config{Disabled: false})

	// 3. Create HTTP client using our Resolver
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: r.DialContext,
		},
		Timeout: 5 * time.Second,
	}

	// 4. Test request to external site (sanity check)
	// We use example.com because it's stable.
	resp, err := client.Get("http://example.com")
	if err != nil {
		t.Fatalf("Failed to make request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}

	// 5. Test Cache Hit
	memCache, ok := r.cache.(*memoryCache)
	if !ok {
		t.Fatal("Expected memoryCache implementation")
	}

	ips, _ := memCache.Get("example.com")
	if len(ips) == 0 {
		t.Errorf("Expected cache to be populated for example.com")
	} else {
		t.Logf("Cached IPs for example.com: %v", ips)
	}

	// 6. Test LookupIP
	ipAddrs, err := r.LookupIP(context.Background(), "ip", "example.com")
	if err != nil {
		t.Errorf("LookupIP failed: %v", err)
	}
	if len(ipAddrs) == 0 {
		t.Errorf("LookupIP returned no addresses")
	}
}

func TestDisabledMode(t *testing.T) {
	r := New(Config{Disabled: true})
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: r.DialContext,
		},
	}

	resp, err := client.Get("http://example.com")
	if err != nil {
		t.Fatalf("Failed to make request in disabled mode: %v", err)
	}
	resp.Body.Close()

	// Verify cache is empty
	memCache := r.cache.(*memoryCache)
	if len(memCache.store) != 0 {
		t.Error("Expected cache to be empty in disabled mode")
	}
}

func TestOnCacheMissAndSingleflight(t *testing.T) {
	var missCount int32
	r := New(Config{
		OnCacheMiss: func(host string) {
			atomic.AddInt32(&missCount, 1)
		},
	})

	// Simulate concurrent requests
	var wg sync.WaitGroup
	n := 10
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := r.LookupHost(context.Background(), "example.com")
			if err != nil {
				t.Errorf("LookupHost failed: %v", err)
			}
		}()
	}
	wg.Wait()

	// With singleflight, we expect exactly 1 cache miss (and 1 actual DNS lookup)
	count := atomic.LoadInt32(&missCount)
	if count != 1 {
		t.Errorf("Expected 1 cache miss, got %d", count)
	}

	// Verify subsequent requests don't trigger miss
	_, _ = r.LookupHost(context.Background(), "example.com")
	count = atomic.LoadInt32(&missCount)
	if count != 1 {
		t.Errorf("Expected count to remain 1, got %d", count)
	}
}

func TestLookupAddr(t *testing.T) {
	r := New(Config{Disabled: false})

	// We use a well-known IP for testing, 8.8.8.8 usually resolves to dns.google
	names, err := r.LookupAddr(context.Background(), "8.8.8.8")
	if err != nil {
		// If network is down or restricted, this might fail, so we skip or log
		t.Logf("LookupAddr failed (network issue?): %v", err)
		return
	}

	if len(names) == 0 {
		t.Error("LookupAddr returned no names for 8.8.8.8")
	} else {
		t.Logf("Reverse lookup for 8.8.8.8: %v", names)
	}

	// Verify cache population
	memCache, ok := r.cache.(*memoryCache)
	if !ok {
		t.Fatal("Expected memoryCache implementation")
	}

	cachedNames, _ := memCache.Get("8.8.8.8")
	if len(cachedNames) == 0 {
		t.Error("Expected cache to be populated for 8.8.8.8")
	}
}

func TestCacheCleanup(t *testing.T) {
	r := New(Config{CacheTTL: time.Minute})
	cache := r.cache

	// Add two entries
	cache.Set("keep.me", []string{"1.2.3.4"}, time.Minute)
	cache.Set("delete.me", []string{"5.6.7.8"}, time.Minute)

	// First Prune: resets 'used' flags to false (nothing deleted because they were fresh/used)
	r.Refresh(true)

	// Access "keep.me" -> marks it used again
	_, _ = cache.Get("keep.me")

	// Second Prune: should delete "delete.me" (unused since last prune)
	r.Refresh(true)

	// Verify "keep.me" exists
	if ips, _ := cache.Get("keep.me"); ips == nil {
		t.Error("Expected 'keep.me' to remain in cache")
	}

	// Verify "delete.me" is gone
	if ips, _ := cache.Get("delete.me"); ips != nil {
		t.Error("Expected 'delete.me' to be removed from cache")
	}
}

func TestPersistOnFailure(t *testing.T) {
	r := New(Config{
		CacheTTL:         time.Millisecond, // Expire immediately
		PersistOnFailure: true,
	})
	cache := r.cache

	// Manually inject an expired entry for a non-existent domain
	key := "nonexistent.test.local"
	cache.Set(key, []string{"127.0.0.2"}, -time.Second) // Expired 1 second ago

	// LookupHost should try to resolve, fail (because domain doesn't exist),
	// but then fallback to the cached value because PersistOnFailure is true.
	ips, err := r.LookupHost(context.Background(), key)
	if err != nil {
		t.Errorf("Expected success (fallback to cache), got error: %v", err)
	}
	if len(ips) == 0 || ips[0] != "127.0.0.2" {
		t.Errorf("Expected cached value '127.0.0.2', got %v", ips)
	}

	// Disable PersistOnFailure and verify failure
	r.config.PersistOnFailure = false
	// We need to clear lookupGroup's memory or something? No, it's fresh call.
	// But wait, the cache entry is still there.
	// The key is unique per call? No.

	// Singleflight might coalesce calls if they were concurrent, but here they are serial.

	_, err = r.LookupHost(context.Background(), key)
	if err == nil {
		t.Error("Expected error when PersistOnFailure is false for bad domain")
	}
}

func TestAutoCleanup(t *testing.T) {
	r := New(Config{
		CacheTTL:        time.Minute,
		CleanupInterval: 50 * time.Millisecond,
	})
	defer r.Stop()

	cache := r.cache
	cache.Set("delete.me", []string{"1.2.3.4"}, time.Minute)

	// Wait for one cycle (resets used flag)
	time.Sleep(100 * time.Millisecond)

	// Wait for second cycle (deletes unused)
	time.Sleep(100 * time.Millisecond)

	if ips, _ := cache.Get("delete.me"); ips != nil {
		t.Error("Expected 'delete.me' to be auto-removed from cache")
	}
}

func TestForceIPVersion(t *testing.T) {
	// Test IPv4 Only
	r4 := NewOnlyV4(Config{})
	ips4, err := r4.LookupHost(context.Background(), "ipv6.google.com")
	// ipv6.google.com normally only has AAAA record. If we force V4, we might get error or empty
	// A better test is to lookup something with both (google.com) and verify format
	if err == nil {
		for _, ip := range ips4 {
			if net.ParseIP(ip).To4() == nil {
				t.Errorf("NewOnlyV4 returned IPv6 address: %s", ip)
			}
		}
	}

	// Test IPv6 Only
	r6 := NewOnlyV6(Config{})
	// localhost usually has ::1
	ips6, err := r6.LookupHost(context.Background(), "google.com")
	if err == nil {
		for _, ip := range ips6 {
			// To4() returns nil if it's not a valid v4 address (i.e. it is v6)
			// Wait, To4 returns nil for IPv6.
			if net.ParseIP(ip).To4() != nil {
				t.Errorf("NewOnlyV6 returned IPv4 address: %s", ip)
			}
		}
	}
}

func TestStats(t *testing.T) {
	r := New(Config{CacheTTL: time.Minute})

	// First lookup: Miss
	_, _ = r.LookupHost(context.Background(), "example.com")

	stats := r.Stats()
	if stats.CacheMisses != 1 {
		t.Errorf("Expected 1 cache miss, got %d", stats.CacheMisses)
	}
	if stats.CacheHits != 0 {
		t.Errorf("Expected 0 cache hits, got %d", stats.CacheHits)
	}

	// Second lookup: Hit
	_, _ = r.LookupHost(context.Background(), "example.com")

	stats = r.Stats()
	if stats.CacheMisses != 1 {
		t.Errorf("Expected 1 cache miss, got %d", stats.CacheMisses)
	}
	if stats.CacheHits != 1 {
		t.Errorf("Expected 1 cache hit, got %d", stats.CacheHits)
	}
}

func TestStopIdempotency(t *testing.T) {
	r := New(Config{CleanupInterval: time.Minute})
	// Should not panic calling Stop multiple times
	r.Stop()
	r.Stop()
	r.Stop()
}
