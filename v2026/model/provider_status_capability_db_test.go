package model

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// The provider status's capability reason (FP2-PROTOCOL.md §5.5, D11.6)
// against the database: the url_checks number of a provider whose URL checks
// took a capability away says why, with each sentence's count, and the
// number itself stays the strict one. Needs the failed_later_legs and
// pq_outcome columns of the evidence migration.

// One accepted URL run of a provider's window: whether it passed, the later
// legs judged the provider's (failed_later_legs) and its post-quantum outcome
// (pq_outcome).
type capabilityStatusTestRun struct {
	ok              bool
	failedLaterLegs int16
	pqOutcome       int16
}

// count runs of one kind.
func capabilityStatusTestRepeat(count int, run capabilityStatusTestRun) []capabilityStatusTestRun {
	runs := make([]capabilityStatusTestRun, count)
	for i := range runs {
		runs[i] = run
	}
	return runs
}

// Records the runs as accepted URL outcomes, each at its own time over the
// forty minutes that end a minute ago, in order.
func capabilityStatusTestRuns(ctx context.Context, clientId server.Id, runs ...[]capabilityStatusTestRun) {
	allRuns := []capabilityStatusTestRun{}
	for _, someRuns := range runs {
		allRuns = append(allRuns, someRuns...)
	}
	measuredAt := server.NowUtc().Add(-time.Minute)
	server.Tx(ctx, func(tx server.PgTx) {
		for index, run := range allRuns {
			at := egressTestRunAt(measuredAt, index, len(allRuns))
			ok := 0
			if run.ok {
				ok = 1
			}
			evidence := fp2TestUrlProbeEvidence(at, run.ok)
			server.Raise(evidence.ValidateOutcome(ok, 1, false))
			encoded, err := json.Marshal(evidence)
			server.Raise(err)
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_health_history
				(run_id,client_id,measured_at,ok_count,total_count,class_results,tls_authentication_failure,url_probe_evidence,url_probe_policy_version,failed_later_legs,pq_outcome)
				VALUES($1,$2,$3,$4,1,'{}',false,$5::jsonb,$6,$7,$8)`,
				server.NewId(), clientId, at.UTC(), ok, string(encoded), evidence.PolicyVersion, run.failedLaterLegs, run.pqOutcome))
		}
	})
}

// Marks the provider's connections as proving IPv6 too, after the rollup
// wrote its row: a dual-stack provider, whose advertised IP label is
// dualstack.
func capabilityStatusTestDualstack(ctx context.Context, clientId server.Id) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client_location_reliability SET ipv4_proven = true, ipv6_proven = true WHERE client_id = $1`, clientId))
	})
}

// The url_checks number of a status.
func capabilityStatusTestUrlChecks(t testing.TB, status *ProviderStatus) *ProviderRankingNumber {
	t.Helper()
	if status == nil {
		t.Fatal("no status")
	}
	for _, number := range status.Ranking {
		if number.Name == ProviderStatusNumberUrlChecks {
			return number
		}
	}
	t.Fatal("no url_checks number")
	return nil
}

// Each downgrade gives its sentences and counts, tcp first, and appends them
// to the explanation; a provider that is not downgraded gives none. The
// number's value, count, total and passes are the strict ones in every case.
// A dual-stack provider classed ipv4 by IPv6 HTTP/3 runs alone (mask 4)
// counts them in its ipv4 sentence.
func TestProviderStatusTellsTheCapabilityReason(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		city := egressTestCity(ctx, "Capability City", "Capability Region", "United States", "us")
		pass := capabilityStatusTestRun{ok: true}
		ipv4Udp := capabilityStatusTestRun{failedLaterLegs: ProviderEgressFailedLaterLegIpv4Udp}
		ipv6Tcp := capabilityStatusTestRun{failedLaterLegs: ProviderEgressFailedLaterLegIpv6Tcp}
		ipv6Udp := capabilityStatusTestRun{failedLaterLegs: ProviderEgressFailedLaterLegIpv6Udp}
		type testCase struct {
			name            string
			dualstack       bool
			runs            [][]capabilityStatusTestRun
			ok              int
			wantTcpCount    int
			wantIpv4Count   int
			wantPasses      bool
			wantReasonParts []string
		}
		tcpSentence := func(count int) string {
			return fmt.Sprintf(providerCapabilityReasonTcp, count)
		}
		ipv4Sentence := func(count int) string {
			return fmt.Sprintf(providerCapabilityReasonIpv4, count)
		}
		cases := []testCase{
			// 10 of 20 fail only over HTTP/3: class D from v4-only
			{name: "tcp", runs: [][]capabilityStatusTestRun{capabilityStatusTestRepeat(10, pass), capabilityStatusTestRepeat(10, ipv4Udp)}, ok: 10, wantTcpCount: 10, wantReasonParts: []string{tcpSentence(10)}},
			// 5 of 20 fail only over IPv6 HTTPS: class B from dual-stack
			{name: "ipv4", dualstack: true, runs: [][]capabilityStatusTestRun{capabilityStatusTestRepeat(15, pass), capabilityStatusTestRepeat(5, ipv6Tcp)}, ok: 15, wantIpv4Count: 5, wantReasonParts: []string{ipv4Sentence(5)}},
			// 5 over HTTP/3 and 5 over IPv6: only D forgives both
			{name: "both", dualstack: true, runs: [][]capabilityStatusTestRun{capabilityStatusTestRepeat(10, pass), capabilityStatusTestRepeat(5, ipv4Udp), capabilityStatusTestRepeat(5, ipv6Tcp)}, ok: 10, wantTcpCount: 5, wantIpv4Count: 5, wantReasonParts: []string{tcpSentence(5), ipv4Sentence(5)}},
			// 5 of 20 fail only over IPv6 HTTP/3: B before C, counted as IPv6
			{name: "mask4", dualstack: true, runs: [][]capabilityStatusTestRun{capabilityStatusTestRepeat(15, pass), capabilityStatusTestRepeat(5, ipv6Udp)}, ok: 15, wantIpv4Count: 5, wantReasonParts: []string{ipv4Sentence(5)}},
			// every check passes: no reason
			{name: "neither", dualstack: true, runs: [][]capabilityStatusTestRun{capabilityStatusTestRepeat(20, pass)}, ok: 20, wantPasses: true},
		}
		nameProviders := map[string]*egressTestProvider{}
		for _, c := range cases {
			provider := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
			egressTestReliability(ctx, provider.clientId, 0, 1, 1)
			egressTestReliability(ctx, provider.clientId, 1, 1, 1)
			egressTestReliability(ctx, provider.clientId, 2, 1, 1)
			capabilityStatusTestRuns(ctx, provider.clientId, c.runs...)
			nameProviders[c.name] = provider
		}
		egressTestPasses(ctx, t)
		for _, c := range cases {
			if c.dualstack {
				capabilityStatusTestDualstack(ctx, nameProviders[c.name].clientId)
			}
		}

		for _, c := range cases {
			provider := nameProviders[c.name]
			status := GetClientProviderStatus(ctx, provider.networkId, provider.clientId)
			urlChecks := capabilityStatusTestUrlChecks(t, status)
			// the strict number, unchanged
			if !urlChecks.HasValue || urlChecks.Count != c.ok || urlChecks.Total != 20 || urlChecks.Value != float64(c.ok)/20 || urlChecks.Passes != c.wantPasses {
				t.Errorf("%s: url_checks %d of %d (%v) passes %t, want %d of 20 passes %t", c.name, urlChecks.Count, urlChecks.Total, urlChecks.Value, urlChecks.Passes, c.ok, c.wantPasses)
			}
			wantReason := ""
			for i, part := range c.wantReasonParts {
				if 0 < i {
					wantReason += " "
				}
				wantReason += part
			}
			if urlChecks.Reason != wantReason || urlChecks.ReasonTcpCount != c.wantTcpCount || urlChecks.ReasonIpv4Count != c.wantIpv4Count {
				t.Errorf("%s: reason %q tcp %d ipv4 %d, want %q tcp %d ipv4 %d", c.name, urlChecks.Reason, urlChecks.ReasonTcpCount, urlChecks.ReasonIpv4Count, wantReason, c.wantTcpCount, c.wantIpv4Count)
			}
			wantExplanation := fmt.Sprintf("Test sites that loaded through this connection in the last %d hours: %d of %d. At least %d in %d must load; filtering by your internet provider lowers it.", 8, c.ok, 20, 4, 5)
			if wantReason != "" {
				wantExplanation += " " + wantReason
			}
			if urlChecks.Explanation != wantExplanation {
				t.Errorf("%s: explanation %q, want %q", c.name, urlChecks.Explanation, wantExplanation)
			}
			// the compact form carries the counts without the text
			compact := NewProviderStatusCompact(status)
			for _, number := range compact.Ranking {
				if number.Name != ProviderStatusNumberUrlChecks {
					if number.ReasonTcpCount != 0 || number.ReasonIpv4Count != 0 {
						t.Errorf("%s: compact %s carries reason counts", c.name, number.Name)
					}
					continue
				}
				if number.ReasonTcpCount != c.wantTcpCount || number.ReasonIpv4Count != c.wantIpv4Count {
					t.Errorf("%s: compact url_checks tcp %d ipv4 %d", c.name, number.ReasonTcpCount, number.ReasonIpv4Count)
				}
			}
			// absent from the wire without a reason
			encoded, err := json.Marshal(urlChecks)
			server.Raise(err)
			var wire map[string]any
			server.Raise(json.Unmarshal(encoded, &wire))
			_, hasReason := wire["reason"]
			_, hasTcpCount := wire["reason_tcp_count"]
			_, hasIpv4Count := wire["reason_ipv4_count"]
			if hasReason != (wantReason != "") || hasTcpCount != (0 < c.wantTcpCount) || hasIpv4Count != (0 < c.wantIpv4Count) {
				t.Errorf("%s: wire fields reason %t tcp %t ipv4 %t", c.name, hasReason, hasTcpCount, hasIpv4Count)
			}
		}
	})
}
