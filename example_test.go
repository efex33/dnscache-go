package dnscache_test

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/ZhangYoungDev/dnscache-go"
)

func Example() {
	// Simulate a metrics collector
	var cacheMisses int64

	// Create a new resolver instance
	resolver := dnscache.New(dnscache.Config{
		// Observability: Track cache misses
		OnCacheMiss: func(host string) {
			atomic.AddInt64(&cacheMisses, 1)
			fmt.Printf("Metrics: Cache miss recorded for %s\n", host)
		},
	})

	// Create a custom http.Transport that uses the resolver
	transport := &http.Transport{
		DialContext: resolver.DialContext,
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   5 * time.Second,
	}

	// 1. First request: Cache Miss (DNS lookup occurs)
	_, err := client.Get("http://example.com")
	if err != nil {
		fmt.Println(err)
	}

	// 2. Second request: Cache Hit (Served from memory)
	_, err = client.Get("http://example.com")
	if err != nil {
		fmt.Println(err)
	}

	// Print metrics
	fmt.Printf("Total Cache Misses: %d\n", atomic.LoadInt64(&cacheMisses))
}

func ExampleResolver_LookupHost() {
	resolver := dnscache.New(dnscache.Config{})

	// Context with timeout is supported
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Lookup IPs directly
	ips, err := resolver.LookupHost(ctx, "example.com")
	if err != nil {
		fmt.Printf("Lookup failed: %v\n", err)
		return
	}

	if len(ips) > 0 {
		fmt.Printf("Found %d IPs\n", len(ips))
	}
}

func ExampleResolver_Refresh() {
	resolver := dnscache.New(dnscache.Config{})

	// Perform some lookups...
	_, _ = resolver.LookupHost(context.Background(), "example.com")

	// Periodically refresh the cache to update IPs and remove unused entries
	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()

		for range ticker.C {
			// true means remove unused entries
			resolver.Refresh(true)
		}
	}()
}
