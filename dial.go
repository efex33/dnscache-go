package dnscache

import (
	"context"
	"net"
)

// DialContext connects to the address on the named network using the provided context.
func (r *Resolver) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if r.config.Disabled {
		return r.dialer.DialContext(ctx, network, address)
	}

	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}

	ips, err := r.LookupHost(ctx, host)
	if err != nil {
		return nil, err
	}

	var lastErr error
	for _, ip := range ips {
		targetAddr := net.JoinHostPort(ip, port)
		conn, err := r.dialer.DialContext(ctx, network, targetAddr)
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}

	if lastErr != nil {
		return nil, lastErr
	}

	// Should not happen if ips is not empty, but handle strictly
	return nil, &net.OpError{Op: "dial", Net: network, Source: nil, Addr: nil, Err: net.UnknownNetworkError("no addresses found")}
}
