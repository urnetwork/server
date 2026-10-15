package server

// net_dial.go — the one outbound dialer for the server (connect/IPV6.md C6).
//
// Every place the server originates a TCP connection to a name should go
// through NewDialer or the http helpers below, so a dual-stack destination is
// raced across families (RFC 8305 happy eyeballs) instead of waiting out a
// broken family's full timeout. The stdlib dialer already races when the
// network is family-agnostic ("tcp") and the name resolves to both families;
// this file makes the delay explicit and puts the behavior in one place.
// Family-specific networks ("tcp4", "tcp6") still dial one family only, which
// is what a caller that pins a family means.

import (
	"context"
	"net"
	"net/http"
	"time"
)

// DialFallbackDelay is the happy-eyeballs delay between the first family's
// connection attempt and the second's. Explicit so the race is part of the
// contract rather than an accident of the Go version's default.
const DialFallbackDelay = 250 * time.Millisecond

// NewDialer returns a dual-stack dialer with the given connect timeout: when
// a name resolves to both families the attempts race with DialFallbackDelay
// between them and the first to connect wins.
func NewDialer(timeout time.Duration) *net.Dialer {
	return &net.Dialer{
		Timeout:       timeout,
		FallbackDelay: DialFallbackDelay,
		KeepAliveConfig: net.KeepAliveConfig{
			Enable: true,
		},
	}
}

// DialContext dials with the default connect timeout and the family race.
func DialContext(ctx context.Context, network string, addr string) (net.Conn, error) {
	return NewDialer(DefaultHttpConnectTimeout).DialContext(ctx, network, addr)
}

// NewHttpTransport mirrors http.DefaultTransport (environment proxy, h2,
// idle pool) with the dual-stack dialer, so a site that used the default
// transport keeps its behavior and gains the family race.
func NewHttpTransport() *http.Transport {
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           NewDialer(DefaultHttpConnectTimeout).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   DefaultHttpTlsTimeout,
		ExpectContinueTimeout: 1 * time.Second,
	}
}

// NewHttpClient replaces a bare `&http.Client{Timeout: timeout}`, which would
// dial through http.DefaultTransport without the explicit race.
func NewHttpClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Transport: NewHttpTransport(),
		Timeout:   timeout,
	}
}
