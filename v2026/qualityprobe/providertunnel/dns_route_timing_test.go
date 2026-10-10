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

// Drive the actual resolver, real monitor admission timestamps, and fake time.
// End-to-end DNS/socket outcomes and retry counts must survive diagnostics.
func TestProviderDnsRouteTimingDeterministicControls(t *testing.T) {
	cases := []struct {
		name   string
		route  dnsRouteClass
		result dnsResult
		parts  [3]float64
	}{
		{"warm_route", dnsRouteStable, dnsAnswer, [3]float64{0, 5, 0}},
		{"cold_to_ready", dnsRouteAdmitted, dnsAnswer, [3]float64{3, 2, 0}},
		{"never_ready", dnsRouteUnreadyEndpoints, dnsAuthoritativeEmpty, [3]float64{0, 0, 5}},
		{"lost", dnsRouteLost, dnsCanceled, [3]float64{0, 0, 5}},
		{"closed", dnsRouteClosed, dnsCanceled, [3]float64{0, 0, 5}},
		{"same_id_readmitted", dnsRouteChanged, dnsAnswer, [3]float64{0, 0, 5}},
		{"multiple_routes", dnsRouteChanged, dnsAnswer, [3]float64{0, 0, 5}},
		{"metadata_update", dnsRouteStable, dnsAnswer, [3]float64{0, 5, 0}},
		// A transient route is deliberately invisible at both endpoints.
		// The class must not claim that the tunnel was never ready.
		{"transient_route_disappears", dnsRouteUnreadyEndpoints, dnsAuthoritativeEmpty, [3]float64{0, 0, 5}},
		// The split is only relative to the final route, not first readiness.
		{"transient_then_current", dnsRouteAdmitted, dnsAnswer, [3]float64{3, 2, 0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				monitor := connect.NewRemoteUserNatMultiClientMonitorWithDefaults()
				route, provider, second := connect.NewId(), connect.NewId(), connect.NewId()
				add := func(id connect.Id) {
					monitor.AddProviderEvent(id, connect.ProviderStateAdded, provider, nil, connect.IpFamilyV4Only)
				}
				remove := func(id connect.Id) {
					monitor.AddProviderEvent(id, connect.ProviderStateRemoved, provider, nil, connect.IpFamilyV4Only)
				}
				warm := tc.name == "warm_route" || tc.name == "lost" || tc.name == "closed" || tc.name == "same_id_readmitted" || tc.name == "metadata_update" || tc.name == "multiple_routes"
				if warm {
					add(route)
				}
				if tc.name == "multiple_routes" {
					add(second)
				}
				lost, lose := context.WithCancelCause(t.Context())
				defer lose(ErrTunnelClosed)
				ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
				defer cancel()
				counts := &DnsObservations{}
				queries, dials := 0, 0
				laterSocketError := errors.New("synthetic target socket failure after successful DNS")
				resolver := &providerUrlResolver{
					observations: counts,
					routeState: func() dnsRouteSnapshot {
						window, providers := monitor.Events()
						return dnsRouteFromMonitor(lost, window, providers)
					},
					query: func(context.Context, string, string) ([]netip.Addr, bool) {
						queries++
						if tc.name == "transient_route_disappears" || tc.name == "transient_then_current" {
							time.Sleep(time.Second)
							add(second)
							time.Sleep(time.Second)
							remove(second)
							time.Sleep(time.Second)
						} else {
							time.Sleep(3 * time.Second)
						}
						switch tc.name {
						case "cold_to_ready", "transient_then_current":
							add(route)
						case "same_id_readmitted":
							remove(route)
							add(route)
						case "metadata_update":
							monitor.SetProviderExtenderIps(route, []netip.Addr{netip.MustParseAddr("192.0.2.91")})
						}
						time.Sleep(2 * time.Second)
						if tc.name == "lost" {
							lose(ErrTunnelLost)
							cancel()
							return nil, false
						}
						if tc.name == "closed" {
							lose(ErrTunnelClosed)
							cancel()
							return nil, false
						}
						if tc.name == "never_ready" || tc.name == "transient_route_disappears" {
							return nil, true
						}
						return []netip.Addr{netip.MustParseAddr("192.0.2.41")}, true
					},
					dial: func(context.Context, string, string, []netip.Addr) (net.Conn, error) {
						dials++
						return nil, laterSocketError
					},
				}
				ctx, trace := traceProviderHttpDial(ctx, "tcp", "sample.example:443")
				_, err := resolver.dialContext(ctx, "tcp", "sample.example:443", trace)
				trace.finish(err)
				if queries != 1 {
					t.Fatalf("diagnostic changed query count: %d", queries)
				}
				if tc.result == dnsAnswer {
					if dials != 1 || !errors.Is(err, laterSocketError) {
						t.Fatal("diagnostic changed successful DNS or target socket admission")
					}
				} else if dials != 0 || err == nil {
					t.Fatal("diagnostic changed empty/canceled DNS admission")
				}
				if tc.result == dnsCanceled && !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation outcome changed: %v", err)
				}
				var total uint64
				for _, row := range counts.RouteTimingSnapshot() {
					total += row.Count
					if row.Result == dnsResultLabels[tc.result] && row.Route == dnsRouteLabels[tc.route] {
						actual := [3]float64{row.BeforeCurrentAdmissionSeconds, row.AfterCurrentAdmissionSeconds, row.UnattributedSeconds}
						if row.Count != 1 || actual != tc.parts {
							t.Errorf("route diagnostic=%s/%s count=%d parts=%v, want one %v", row.Result, row.Route, row.Count, actual, tc.parts)
						}
					}
				}
				if total != 1 {
					t.Errorf("diagnostic waves=%d, want1", total)
				}
				var legacyTotal uint64
				for _, row := range counts.Snapshot() {
					legacyTotal += row.Count
				}
				if legacyTotal != 1 {
					t.Error("new diagnostics changed existing wave accounting")
				}
			})
		})
	}
}

// Missing source clocks and snapshot races retain all elapsed time as unknown.
func TestProviderDnsRouteTimingAmbiguityControls(t *testing.T) {
	start := time.Now()
	end := start.Add(5 * time.Second)
	id := connect.NewId()
	empty := dnsRouteSnapshot{known: true, path: dnsPathForming}
	active := dnsRouteSnapshot{known: true, path: dnsPathActive, active: 1, clientId: id, admittedAt: start.Add(-time.Second)}
	zeroClock := active
	zeroClock.admittedAt = time.Time{}
	later := active
	later.admittedAt = end.Add(time.Second)
	missing := dnsRouteSnapshot{}
	cases := []struct {
		name          string
		before, after dnsRouteSnapshot
		want          dnsRouteClass
	}{
		{"missing_clock", zeroClock, zeroClock, dnsRouteChanged},
		{"post_wave_admission", empty, later, dnsRouteChanged},
		{"pre_wave_unseen_admission", empty, active, dnsRouteChanged},
		{"missing_before", missing, active, dnsRouteUnknown},
		{"missing_after", active, missing, dnsRouteUnknown},
		{"route_removed", active, empty, dnsRouteChanged},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			class, parts := classifyDnsRouteTiming(start, end, tc.before, tc.after)
			if class != tc.want || parts != [3]time.Duration{0, 0, 5 * time.Second} {
				t.Fatalf("ambiguous route became attributed: class=%d parts=%v", class, parts)
			}
		})
	}
}

func TestProviderDnsRouteTimingBoundedConcurrentAndNilSafe(t *testing.T) {
	var absent *DnsObservations
	now := time.Now()
	empty := dnsRouteSnapshot{known: true}
	absent.recordRouteTiming(dnsTimeout, now, now.Add(time.Second), empty, empty)
	counts := &DnsObservations{}
	var workers sync.WaitGroup
	for range 10 {
		workers.Go(func() {
			for range 100 {
				counts.recordRouteTiming(dnsTimeout, now, now.Add(time.Second), empty, empty)
			}
		})
	}
	workers.Wait()
	counts.recordRouteTiming(dnsResult(-1), now, now, empty, empty)
	if len(counts.RouteTimingSnapshot()) != 35 || len(absent.RouteTimingSnapshot()) != 35 {
		t.Fatal("unbounded diagnostic schema")
	}
	var total uint64
	var seconds float64
	for _, row := range counts.RouteTimingSnapshot() {
		total += row.Count
		seconds += row.UnattributedSeconds
	}
	if total != 1000 || seconds != 1000 {
		t.Fatalf("concurrent diagnostic count=%d seconds=%v", total, seconds)
	}
}
