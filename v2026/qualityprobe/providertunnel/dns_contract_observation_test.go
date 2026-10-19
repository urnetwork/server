package providertunnel

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
	"testing/synctest"
	"time"
)

// The resolver and timeout are real; the SDK witness is a structural seam.
// Connect's contract-readiness regression owns the real writer/credit proof.
// A false witness is deliberately not labeled provider contact or provider fault.
func TestProviderDnsContractWitnessUsesExactWaveBeforeCleanup(t *testing.T) {
	for _, mode := range []string{"local_wait", "provider_contact_or_unknown", "unobserved", "healthy_answer", "owner_canceled"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				ctx = urlPhaseTestContext(t, ctx, "contract.example")
				counts := &DnsObservations{}
				queries, dials, witnessCalls := 0, 0, 0
				var queryContext context.Context
				start := time.Now()
				stop := errors.New("target socket boundary")
				resolver := &providerUrlResolver{
					observations: counts,
					query: func(q context.Context, _, _ string) ([]netip.Addr, bool) {
						queries++
						queryContext = q
						if mode == "healthy_answer" {
							time.Sleep(time.Second)
							return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, true
						}
						if mode == "owner_canceled" {
							time.AfterFunc(time.Second, cancel)
						}
						<-q.Done()
						return nil, false
					},
					dial: func(context.Context, string, string, []netip.Addr) (net.Conn, error) { dials++; return nil, stop },
				}
				if mode != "unobserved" {
					resolver.contractUnavailable = func() bool {
						witnessCalls++
						if mode == "healthy_answer" && queryContext.Err() != nil {
							t.Error("diagnostic sampled after cleanup cancellation")
						}
						return mode == "local_wait" || mode == "owner_canceled"
					}
				}
				ctx, trace := traceProviderHttpDial(ctx, "tcp", "contract.example:443")
				_, err := resolver.dialContext(ctx, "tcp", "contract.example:443", trace)
				trace.finish(err)
				wantResult, wantEvidence, wantDuration := "timeout", "not_proved", 5*time.Second
				if mode == "local_wait" || mode == "owner_canceled" {
					wantEvidence = "local_contract_no_provider_write"
				}
				if mode == "unobserved" {
					wantEvidence = "unobserved"
				}
				if mode == "healthy_answer" {
					wantResult, wantDuration = "answer", time.Second
					if !errors.Is(err, stop) || dials != 1 {
						t.Fatal("healthy DNS changed")
					}
				} else if dials != 0 || err == nil {
					t.Fatal("failed DNS reached target")
				}
				if mode == "owner_canceled" {
					wantResult, wantDuration = "canceled", time.Second
					if !errors.Is(err, context.Canceled) {
						t.Fatal("owner cancellation changed")
					}
				}
				if time.Since(start) != wantDuration || queries != 1 || witnessCalls != map[bool]int{true: 0, false: 1}[mode == "unobserved"] {
					t.Fatal("diagnostics changed attempts/budget")
				}
				var total uint64
				for _, row := range counts.ContractSnapshot() {
					total += row.Count
					if row.Result == wantResult && row.Evidence == wantEvidence && (row.Count != 1 || row.WaveSeconds != wantDuration.Seconds()) {
						t.Fatalf("wrong wave observation: %+v", row)
					}
				}
				if total != 1 {
					t.Fatal("missing/duplicate wave")
				}
				for _, row := range (&DnsObservations{}).ContractSnapshot() {
					if row.Count != 0 {
						t.Fatal("another tunnel inherited evidence")
					}
				}
			})
		})
	}
}
