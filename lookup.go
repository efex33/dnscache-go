package dnscache

import (
	"context"
	"fmt"
	"net"
	"net/http/httptrace"
	"sync/atomic"
	"time"
)

// LookupHost looks up the given host using the local cache or upstream resolver.
// It returns a slice of that host's addresses.
func (r *Resolver) LookupHost(ctx context.Context, host string) ([]string, error) {
	if r.config.Disabled {
		return r.upstream.LookupHost(ctx, host)
	}

	return r.lookup(ctx, host, func(ctx context.Context, key string) ([]string, error) {
		return r.lookupAndCache(ctx, key, r.upstream.LookupHost)
	})
}

// LookupAddr performs a reverse lookup for the given address, returning a list
// of names mapping to that address.
func (r *Resolver) LookupAddr(ctx context.Context, addr string) ([]string, error) {
	if r.config.Disabled {
		return r.upstream.LookupAddr(ctx, addr)
	}

	return r.lookup(ctx, addr, func(ctx context.Context, key string) ([]string, error) {
		return r.lookupAndCache(ctx, key, r.upstream.LookupAddr)
	})
}

// lookup implements the common caching and singleflight logic
func (r *Resolver) lookup(ctx context.Context, key string, doFetch func(context.Context, string) ([]string, error)) ([]string, error) {
	values, expireAt := r.cache.Get(key)
	if values != nil {
		if time.Now().Before(expireAt) {
			// Cache hit and not expired
			atomic.AddUint64(&r.stats.CacheHits, 1)
			if r.config.EnableAutoRefresh {
				ttl := r.config.CacheTTL
				if ttl > 0 && time.Until(expireAt) < ttl/2 {
					// Async refresh
					go func() {
						_, _, _ = r.lookupGroup.Do(key, func() (interface{}, error) {
							// Use background context for async refresh
							return doFetch(context.Background(), key)
						})
					}()
				}
			}
			return values, nil
		}
		// Expired, fall through to refresh
	}

	atomic.AddUint64(&r.stats.CacheMisses, 1)

	// Cache misses check
	v, err, shared := r.lookupGroup.Do(key, func() (interface{}, error) {
		// Double check cache inside singleflight
		if values, expireAt := r.cache.Get(key); values != nil {
			if time.Now().Before(expireAt) {
				return values, nil
			}
		}
		return doFetch(ctx, key)
	})

	if err != nil {
		// If the error is context cancelled/timeout, we should not cache it (implicitly handled by not setting cache on error)
		// But we should also consider if we want to return stale data if available.
		if r.config.PersistOnFailure {
			if values, _ := r.cache.Get(key); values != nil {
				return values, nil
			}
		}
		return nil, err
	}

	// If shared and ctx is cancelled, singleflight might return result but our context is dead.
	// Actually singleflight.Do returns err if the fn returns err.
	// If the fn succeeded, we get value.

	if values, ok := v.([]string); ok {
		// If this was a shared request, wait, doFetch sets the cache? Yes, lookupAndCache sets the cache.
		// But if we are a "follower" in singleflight, we didn't execute doFetch.
		// We just got the result. The "leader" set the cache.

		// Optimization: if we are a follower (shared=true), we don't need to do anything else.
		_ = shared
		return values, nil
	}
	return nil, fmt.Errorf("unexpected type from singleflight: %T", v)
}

// lookupAndCache performs the actual upstream lookup and updates the cache.
// It matches the signature expected by lookup's doFetch but also handles the specific
// upstream call (LookupHost vs LookupAddr) via the fetcher parameter.
func (r *Resolver) lookupAndCache(ctx context.Context, key string, fetcher func(context.Context, string) ([]string, error)) ([]string, error) {
	if r.config.OnCacheMiss != nil {
		r.config.OnCacheMiss(key)
	}

	trace := httptrace.ContextClientTrace(ctx)
	// Note: We use DNSStart/DNSDone for both LookupHost and LookupAddr for simplicity.
	// For LookupAddr, 'Host' in DNSStartInfo will be the IP address string, which is acceptable.
	if trace != nil && trace.DNSStart != nil {
		trace.DNSStart(httptrace.DNSStartInfo{Host: key})
	}

	results, err := fetcher(ctx, key)

	if trace != nil && trace.DNSDone != nil {
		trace.DNSDone(httptrace.DNSDoneInfo{
			Addrs: make([]net.IPAddr, 0),
			Err:   err,
		})
	}

	if err != nil {
		return nil, err
	}

	// Don't cache if context is cancelled or deadline exceeded,
	// because the result might be partial or it might just be an error propagation.
	// Actually, fetcher returns err if ctx is done. So err != nil above handles it.
	// But just to be safe and explicit:
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	r.cache.Set(key, results, r.config.CacheTTL)
	return results, nil
}

// LookupIP looks up host using the local cache or upstream resolver.
// It returns a slice of that host's IPv4 and IPv6 addresses.
// The network parameter can be "ip", "ip4" or "ip6".
func (r *Resolver) LookupIP(ctx context.Context, network, host string) ([]net.IP, error) {
	if r.config.Disabled {
		return r.upstream.LookupIP(ctx, network, host)
	}

	// We reuse LookupHost's cache (which stores strings)
	// This might be slightly less efficient than storing net.IP directly,
	// but it keeps the cache unified.
	ipStrings, err := r.LookupHost(ctx, host)
	if err != nil {
		return nil, err
	}

	var ips []net.IP
	for _, s := range ipStrings {
		ip := net.ParseIP(s)
		if ip == nil {
			continue
		}

		// Filter based on network
		switch network {
		case "ip4":
			if ip.To4() != nil {
				ips = append(ips, ip)
			}
		case "ip6":
			if ip.To4() == nil && ip.To16() != nil {
				ips = append(ips, ip)
			}
		default: // "ip"
			ips = append(ips, ip)
		}
	}

	return ips, nil
}
