// Resolution retries precede sockets and remain inside one request owner.
package providertunnel

import (
	"context"
	"errors"
	"net"
	"net/http/httptrace"
	"net/netip"
	"testing"
	"testing/synctest"
	"time"
)

// Two transient lookups cannot turn a subsequently reachable URL into failure.
func TestProviderResolutionRetriesBeforeSocket(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		queries, dials := 0, 0
		target := netip.MustParseAddr("192.0.2.15")
		left, right := net.Pipe()
		defer left.Close()
		defer right.Close()
		resolver := &providerUrlResolver{
			query: func(ctx context.Context, kind, host string) ([]netip.Addr, bool) {
				queries++
				if kind != "A" || host != "sample.example" || httptrace.ContextClientTrace(ctx) != nil {
					t.Error("resolver escaped target/family/trace boundary")
				}
				if queries < 3 {
					return nil, false
				}
				return []netip.Addr{target}, true
			},
			dial: func(_ context.Context, network, host string, addrs []netip.Addr) (net.Conn, error) {
				dials++
				if queries != 3 || host != "sample.example:443" || network != "tcp" || len(addrs) != 1 || addrs[0] != target {
					return nil, errors.New("wrong resolution handoff")
				}
				return left, nil
			},
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		ctx, observation := traceProviderHttpDial(ctx, "tcp", "sample.example:443")
		conn, err := resolver.dialContext(ctx, "tcp", "sample.example:443", observation)
		observation.finish(err)
		if err != nil || conn != left || queries != 3 || dials != 1 {
			t.Fatalf("retry handoff queries=%d dials=%d err=%v", queries, dials, err)
		}
	})
}

// A never-answering resolver gets finite retries, no target socket, and no
// borrowed HTTP trace; the hard outer deadline cannot be renewed by a retry.
func TestProviderResolutionTimeoutsStayBounded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		queries := 0
		resolver := &providerUrlResolver{
			query: func(ctx context.Context, _, _ string) ([]netip.Addr, bool) {
				queries++
				<-ctx.Done()
				return nil, false
			},
			dial: func(context.Context, string, string, []netip.Addr) (net.Conn, error) {
				t.Error("unresolved target reached socket phase")
				return nil, errors.New("unexpected dial")
			},
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		ctx, observation := traceProviderHttpDial(ctx, "tcp", "sample.example:443")
		start := time.Now()
		_, err := resolver.dialContext(ctx, "tcp", "sample.example:443", observation)
		if stage := observation.finish(err); err == nil || stage != "dial_dns" || queries != 3 || time.Since(start) > 10*time.Second {
			t.Fatalf("unbounded/misattributed resolution: queries=%d duration=%s stage=%s err=%v", queries, time.Since(start), stage, err)
		}
	})
}

// Repeating a cached authoritative negative cannot discover anything new.
// The sampled load's existing spaced retries retain the recovery opportunity.
func TestProviderResolutionAuthoritativeMissDoesNotAmplify(t *testing.T) {
	queries := 0
	resolver := &providerUrlResolver{
		query: func(context.Context, string, string) ([]netip.Addr, bool) { queries++; return nil, true },
		dial: func(context.Context, string, string, []netip.Addr) (net.Conn, error) {
			t.Error("negative answer dialed")
			return nil, errors.New("unexpected dial")
		},
	}
	ctx, observation := traceProviderHttpDial(context.Background(), "tcp", "sample.example:443")
	_, err := resolver.dialContext(ctx, "tcp", "sample.example:443", observation)
	if stage := observation.finish(err); queries != 1 || err == nil || stage != "dial_dns" {
		t.Fatalf("negative amplified: queries=%d stage=%s err=%v", queries, stage, err)
	}
}

// One timed-out lookup leaves bounded time for a later answer and one socket.
func TestProviderResolutionRecoversAfterLookupTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		queries, dials := 0, 0
		target := netip.MustParseAddr("192.0.2.21")
		left, right := net.Pipe()
		defer left.Close()
		defer right.Close()
		resolver := &providerUrlResolver{
			query: func(ctx context.Context, _, _ string) ([]netip.Addr, bool) {
				queries++
				if queries == 1 {
					<-ctx.Done()
					return nil, false
				}
				return []netip.Addr{target}, true
			},
			dial: func(context.Context, string, string, []netip.Addr) (net.Conn, error) {
				dials++
				return left, nil
			},
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		ctx, observation := traceProviderHttpDial(ctx, "tcp", "sample.example:443")
		start := time.Now()
		conn, err := resolver.dialContext(ctx, "tcp", "sample.example:443", observation)
		observation.finish(err)
		if err != nil || conn != left || queries != 2 || dials != 1 || time.Since(start) >= 10*time.Second {
			t.Fatalf("lookup recovery lost: queries=%d dials=%d elapsed=%s err=%v", queries, dials, time.Since(start), err)
		}
	})
}
