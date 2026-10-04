package model

import (
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// An origin-provenance rebuild changes the actual lookup generation while
// preserving classification. It must not become a new URL measurement policy
// or discard the provider's accepted receipts and scheduling progress.
func TestUrlProbeArinEpochTransitionPreservesProgress(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		const oldEpoch int64 = 1791109251
		const candidateEpoch int64 = 1791138081
		oldLookup := server.NowUtc().Add(-time.Minute).Truncate(time.Microsecond)
		city := egressTestCity(ctx, "Epoch City", "Epoch Region", "Epoch Country", "zz")
		provider := egressTestConnect(ctx, t, city, egressTestUnsampled, nil, &ConnectionLocationScores{
			ArinLookupAt: &oldLookup, ArinDatabaseBuildEpoch: oldEpoch, ArinQualityVerified: true,
		})
		rollup := func() {
			now := server.NowUtc()
			UpdateClientLocationReliabilities(ctx, now.Add(-time.Hour), now)
		}
		rollup()
		now := server.NowUtc().Add(time.Second).Truncate(time.Microsecond)
		due := ClaimProviderUrlProbeDue(ctx, now, 1, 0, 1)
		if len(due) != 1 {
			t.Fatal("old-generation provider was not admitted")
		}
		for index := range ProviderUrlProbeRunTarget {
			testingSetUrlProbeHealth(ctx, &ProviderEgressHealth{
				RunId: server.NewId(), ClientId: provider.clientId, CycleStartedAt: due[0].CycleStartedAt,
				MeasuredAt: now.Add(time.Duration(index) * time.Microsecond), OKCount: index % 2, Total: 1,
			})
		}
		// Capture every persisted scheduling field and complete receipts, including
		// run identity and selected URL policy version, before the real write path.
		snapshot := func() (cycle, history string) {
			server.Db(ctx, func(conn server.PgConn) {
				rows, err := conn.Query(ctx, `SELECT to_jsonb(cycle)::text,
					(SELECT jsonb_agg(to_jsonb(receipt) ORDER BY receipt.run_id)::text
					FROM provider_egress_health_history AS receipt WHERE receipt.client_id=$1)
					FROM provider_egress_probe_cycle AS cycle WHERE cycle.client_id=$1`, provider.clientId)
				server.WithPgResult(rows, err, func() {
					if !rows.Next() {
						t.Fatal("missing durable URL cycle")
					}
					server.Raise(rows.Scan(&cycle, &history))
				})
			})
			return
		}
		beforeCycle, beforeHistory := snapshot()
		readWriteToken := func() (token server.Id) {
			server.Db(ctx, func(conn server.PgConn) {
				rows, err := conn.Query(ctx, `SELECT arin_quality_write_token FROM network_client_location WHERE connection_id=$1`, provider.connectionId)
				server.WithPgResult(rows, err, func() {
					if !rows.Next() {
						t.Fatal("missing actual connection lookup")
					}
					server.Raise(rows.Scan(&token))
				})
			})
			return
		}
		beforeToken := readWriteToken()
		cutover := server.NowUtc().Truncate(time.Microsecond)
		coverage := GetProviderArinClassificationCoverage(ctx, candidateEpoch, cutover)
		if coverage.OutdatedConnections != 1 || coverage.ClassifiedConnections != 0 {
			t.Fatalf("old lookup incorrectly attested candidate generation: %+v", coverage)
		}
		newLookup := server.NowUtc().Truncate(time.Microsecond)
		if err := SetConnectionLocation(ctx, provider.connectionId, city.LocationId, &ConnectionLocationScores{
			ArinLookupAt: &newLookup, ArinDatabaseBuildEpoch: candidateEpoch, ArinQualityVerified: true,
		}); err != nil {
			t.Fatal(err)
		}
		if readWriteToken() == beforeToken {
			t.Fatal("actual connection lookup reused its previous quality write token")
		}
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := conn.Query(ctx, `SELECT arin_database_build_epoch,arin_lookup_at,arin_risk,arin_non_quality,arin_quality_verified
				FROM network_client_location WHERE connection_id=$1`, provider.connectionId)
			server.WithPgResult(rows, err, func() {
				if !rows.Next() {
					t.Fatal("missing candidate lookup facts")
				}
				var epoch int64
				var lookup time.Time
				var risk, nonQuality, verified bool
				server.Raise(rows.Scan(&epoch, &lookup, &risk, &nonQuality, &verified))
				if epoch != candidateEpoch || !lookup.Equal(newLookup) || risk || nonQuality || !verified {
					t.Fatalf("candidate write lost actual provenance or changed classification: epoch=%d lookup=%s risk=%v non_quality=%v verified=%v", epoch, lookup, risk, nonQuality, verified)
				}
			})
		})
		// Repeated rollup exercises both conflict-safe seeding and eligibility hints.
		rollup()
		rollup()
		afterCycle, afterHistory := snapshot()
		if afterCycle != beforeCycle || afterHistory != beforeHistory {
			t.Fatalf("ARIN epoch transition changed URL cycle or receipts: cycle_changed=%v history_changed=%v",
				afterCycle != beforeCycle, afterHistory != beforeHistory)
		}
		coverage = GetProviderArinClassificationCoverage(ctx, candidateEpoch, cutover)
		if coverage.ClassifiedConnections != 1 || coverage.OutdatedConnections != 0 || coverage.FullyClassifiedProviders != 1 {
			t.Fatalf("actual candidate lookup did not advance generation attestation: %+v", coverage)
		}
		fleet := testingGetUrlProbeFleet(t, ctx, now.Add(23*time.Minute))
		if fleet.Eligible != 1 || fleet.QuotaComplete != 1 || fleet.Complete != 1 || fleet.RunsNeeded != 0 {
			t.Fatalf("generation transition lost earned measured-run quota: %+v", fleet)
		}
		if repeat := ClaimProviderUrlProbeDue(ctx, now.Add(23*time.Minute), 1, 0, 1); len(repeat) != 0 {
			t.Fatalf("generation transition reprobed a quota-complete provider: %+v", repeat)
		}
		// Normal rolling expiry still creates work without renewing the token or
		// deleting historical credit. All ten measurements are now outside four hours.
		retry := ClaimProviderUrlProbeDue(ctx, now.Add(ProviderEgressProbeRefreshAge+time.Minute), 1, 0, 1)
		if len(retry) != 1 || retry[0].RunsNeeded != ProviderUrlProbeRunTarget || retry[0].OutcomeCount != ProviderUrlProbeRunTarget || !retry[0].CycleStartedAt.Equal(due[0].CycleStartedAt) {
			t.Fatalf("generation transition reset admission or measured-run quota: %+v", retry)
		}
	})
}
