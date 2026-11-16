// DNS observations are synchronous fixed counters, not per-provider state.
package providertunnel

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// A first timeout and a later answer retain their own contemporaneous path
// states; no final-run guard or long retry chain delays these observations.
func TestProviderDnsWavesObserveTheirOwnPath(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		counts := &DnsObservations{}
		path := dnsPathPlatformUnreachable
		queries := 0
		resolver := &providerUrlResolver{
			observations: counts,
			pathState:    func() dnsPathState { return path },
			query: func(ctx context.Context, _, _ string) ([]netip.Addr, bool) {
				queries++
				if queries == 1 {
					<-ctx.Done()
					return nil, false
				}
				if counts.counts[dnsTimeout][dnsPathPlatformUnreachable].Load() != 1 {
					t.Error("first wave was not observed before retry")
				}
				path = dnsPathActive
				return []netip.Addr{netip.MustParseAddr("192.0.2.41")}, true
			},
			dial: func(context.Context, string, string, []netip.Addr) (net.Conn, error) {
				return nil, errors.New("synthetic later socket failure")
			},
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		ctx, trace := traceProviderHttpDial(ctx, "tcp", "sample.example:443")
		_, err := resolver.dialContext(ctx, "tcp", "sample.example:443", trace)
		trace.finish(err)
		if queries != 2 || counts.counts[dnsAnswer][dnsPathActive].Load() != 1 || counts.counts[dnsTimeout][dnsPathPlatformUnreachable].Load() != 1 {
			t.Fatal("DNS waves were collapsed, delayed, or attributed to target TCP")
		}
	})
}

// QueryResult does not expose its internal transport error. Only this wave's
// expired deadline is a timeout; an early empty result stays unanswered.
func TestProviderDnsWavesPreserveUnknownAndCanceledOutcomes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cases := []struct {
			authoritative bool
			cancel        bool
			want          dnsResult
			queries       uint64
		}{
			{authoritative: true, want: dnsAuthoritativeEmpty, queries: 1},
			{want: dnsUnanswered, queries: 3},
			{cancel: true, want: dnsCanceled, queries: 1},
		}
		for _, test := range cases {
			counts := &DnsObservations{}
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			resolver := &providerUrlResolver{
				observations: counts,
				query: func(context.Context, string, string) ([]netip.Addr, bool) {
					if test.cancel {
						cancel()
					}
					return nil, test.authoritative
				},
				dial: func(context.Context, string, string, []netip.Addr) (net.Conn, error) {
					t.Error("empty DNS observation changed socket admission")
					return nil, errors.New("unexpected dial")
				},
			}
			ctx, trace := traceProviderHttpDial(ctx, "tcp", "empty.example:443")
			_, err := resolver.dialContext(ctx, "tcp", "empty.example:443", trace)
			trace.finish(err)
			cancel()
			if err == nil || counts.counts[test.want][dnsPathUnknown].Load() != test.queries {
				t.Fatalf("query result %d lost its classification", test.want)
			}
			var total uint64
			for _, value := range counts.Snapshot() {
				total += value.Count
			}
			if total != test.queries {
				t.Fatalf("query result %d was double counted", test.want)
			}
		}
	})
}

// A literal target performs no name lookup, and must not manufacture a DNS
// answer or path-health control from a socket-only request.
func TestProviderDnsLiteralDoesNotManufactureWave(t *testing.T) {
	counts := &DnsObservations{}
	resolver := &providerUrlResolver{
		observations: counts,
		query: func(context.Context, string, string) ([]netip.Addr, bool) {
			t.Error("literal target performed a DNS lookup")
			return nil, false
		},
		dial: func(context.Context, string, string, []netip.Addr) (net.Conn, error) {
			return nil, errors.New("synthetic socket failure")
		},
	}
	ctx, trace := traceProviderHttpDial(t.Context(), "tcp", "192.0.2.45:443")
	_, err := resolver.dialContext(ctx, "tcp", "192.0.2.45:443", trace)
	trace.finish(err)
	for _, value := range counts.Snapshot() {
		if value.Count != 0 {
			t.Fatal("literal target manufactured a DNS observation")
		}
	}
}

// A healthy current path wins over stale failure reasons. Unknown reasons
// never become labels, and closure is not an accusation against a provider.
func TestProviderDnsPathUsesBoundedCurrentState(t *testing.T) {
	cases := []struct {
		reason string
		want   dnsPathState
	}{
		{reason: connect.WindowStallPlatformUnreachable, want: dnsPathPlatformUnreachable},
		{reason: connect.WindowStallProvidersUnresponsive, want: dnsPathProviderUnresponsive},
		{reason: connect.WindowStallRateLimited, want: dnsPathRateLimited},
		{reason: connect.WindowStallAuthFailing, want: dnsPathAuthFailing},
		{reason: "unrecognized.example", want: dnsPathForming},
	}
	for _, test := range cases {
		window := &connect.WindowExpandEvent{Reason: test.reason}
		if got := dnsPathFromMonitor(nil, window, nil); got != test.want {
			t.Fatalf("path=%d, want %d", got, test.want)
		}
		providers := map[connect.Id]*connect.ProviderEvent{
			connect.NewId(): {State: connect.ProviderStateAdded},
		}
		if got := dnsPathFromMonitor(nil, window, providers); got != dnsPathActive {
			t.Fatal("an active path inherited a stale failure reason")
		}
		lost, lose := context.WithCancelCause(t.Context())
		lose(ErrTunnelLost)
		if got := dnsPathFromMonitor(lost, window, providers); got != dnsPathLost {
			t.Fatal("stale active event hid terminal path loss")
		}
		closed, closePath := context.WithCancelCause(t.Context())
		closePath(ErrTunnelClosed)
		if got := dnsPathFromMonitor(closed, window, providers); got != dnsPathClosed {
			t.Fatal("intentional close became a failed provider path")
		}
	}
}

// Aggregate observation does not serialize or retain provider lifecycles.
func TestProviderDnsObservationsAreConcurrentBoundedAndNilSafe(t *testing.T) {
	var absent *DnsObservations
	absent.record(dnsTimeout, dnsPathUnknown)
	counts := &DnsObservations{}
	var workers sync.WaitGroup
	for range 10 {
		workers.Go(func() {
			for range 100 {
				counts.record(dnsTimeout, dnsPathActive)
			}
		})
	}
	workers.Wait()
	counts.record(dnsResult(-1), dnsPathActive)
	counts.record(dnsTimeout, dnsPathState(99))
	values := counts.Snapshot()
	if len(values) != 45 || len(absent.Snapshot()) != 45 {
		t.Fatal("observation cardinality changed")
	}
	var total uint64
	for _, value := range values {
		total += value.Count
	}
	if total != 1000 {
		t.Fatalf("observation total=%d, want1000", total)
	}
}
