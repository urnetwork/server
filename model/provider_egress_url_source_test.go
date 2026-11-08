package model

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// During a rollout an older release keeps storing URL outcomes with its own
// insert, which names neither url_source nor url_probation. Such an outcome
// reads as the scored general outcome it was counted as: the eight-hour
// window counts it, exactly as before the columns existed.
func TestProviderEgressUrlOutcomeFromAnOlderReleaseCountsAsScoredGeneral(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		// a current store first, which creates the day's partition
		SetProviderEgressHealth(ctx, &ProviderEgressHealth{
			RunId: server.NewId(), ClientId: server.NewId(), MeasuredAt: now.Add(-2 * time.Minute), Total: 1, OKCount: 1,
			ClassResults:     map[string]ProviderEgressHealthClassResult{"site": {OK: 1, Total: 1}},
			UrlProbeEvidence: fp2TestUrlProbeEvidence(now.Add(-2*time.Minute), true),
		})

		clientId := server.NewId()
		evidence := fp2TestUrlProbeEvidence(now.Add(-time.Minute), true)
		evidenceJson, err := json.Marshal(evidence)
		if err != nil {
			t.Fatal(err)
		}
		// the release before the columns, its statement verbatim
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `
			INSERT INTO provider_egress_health_history
				(run_id, client_id, measured_at, ok_count, total_count, class_results, tls_authentication_failure, cycle_started_at, url_probe, url_probe_evidence, url_probe_policy_version)
			VALUES ($1,$2,$3,$4,$5,$6::jsonb,$7,$8,$9,$10::jsonb,$11)
			ON CONFLICT DO NOTHING`,
				server.NewId(), clientId, evidence.MeasuredAt.UTC(), 1, 1,
				`{"site":{"ok":1,"total":1}}`, false, nil, false, string(evidenceJson), evidence.PolicyVersion))
		})

		counts := GetAllProviderEgressHealthCounts(ctx)[clientId]
		if counts.OKCount != 1 || counts.Total != 1 {
			t.Fatalf("an older release's outcome counted %d/%d, want 1/1", counts.OKCount, counts.Total)
		}
		var source int16
		var probation bool
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT url_source, url_probation FROM provider_egress_health_history WHERE client_id=$1`,
				clientId).Scan(&source, &probation))
		})
		if ProviderEgressUrlSource(source) != ProviderEgressUrlSourceGeneral || probation {
			t.Fatalf("an older release's outcome reads as source %d probation %t, want general and scored", source, probation)
		}
	})
}

// The store reads the pool as it stands: an outcome on a general destination
// the pool holds on probation is measured and unscored even when the caller
// did not mark it, and a destination the pool does not hold is scored, as a
// name the pool does not know always was (ProviderEgressHealthScoring.Scores).
func TestProviderEgressUrlOutcomeOnAPoolSiteOnProbationIsUnscored(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		promotedTime := server.NowUtc().Add(-time.Hour)
		SetProviderEgressDestination(ctx, &ProviderEgressDestination{
			Name: "synthetic-document", Class: "site", Url: "https://document.example/", Expect: "body",
			Category: "reference", Region: "global", Source: ProviderEgressDestinationSourceCandidates,
			Active: true, Probation: true, PromotedTime: &promotedTime,
		})
		now := server.NowUtc().Truncate(time.Microsecond)
		probationClientId := server.NewId()
		stored := StoreProviderEgressHealth(ctx, &ProviderEgressHealth{
			RunId: server.NewId(), ClientId: probationClientId, MeasuredAt: now.Add(-time.Minute), Total: 1, OKCount: 1,
			ClassResults:     map[string]ProviderEgressHealthClassResult{"site": {OK: 1, Total: 1}},
			UrlProbeEvidence: fp2TestUrlProbeEvidence(now.Add(-time.Minute), true),
		})
		if !stored.Stored || !stored.Probation || stored.Recheck {
			t.Fatalf("stored %+v, want an outcome on probation", stored)
		}
		if counts, ok := GetAllProviderEgressHealthCounts(ctx)[probationClientId]; ok {
			t.Fatalf("an outcome on probation counted %d/%d", counts.OKCount, counts.Total)
		}
		health := GetProviderEgressHealth(ctx, probationClientId)
		if health == nil || health.OKCount != 0 || health.Total != 0 || len(health.ClassResults) != 0 {
			t.Fatalf("latest run %+v, want no scored load", health)
		}

		unknownClientId := server.NewId()
		evidence := fp2TestUrlEvidence(now.Add(-time.Minute), "https://unknown.example/", false)
		evidence.Destination.Name = "unknown-site"
		evidence.Security[0].Destination.Name = "unknown-site"
		stored = StoreProviderEgressHealth(ctx, &ProviderEgressHealth{
			RunId: server.NewId(), ClientId: unknownClientId, MeasuredAt: now.Add(-time.Minute), Total: 1, OKCount: 1,
			ClassResults:     map[string]ProviderEgressHealthClassResult{"site": {OK: 1, Total: 1}},
			UrlProbeEvidence: evidence,
		})
		if !stored.Stored || stored.Probation {
			t.Fatalf("stored %+v, want a scored outcome", stored)
		}
		if counts := GetAllProviderEgressHealthCounts(ctx)[unknownClientId]; counts.OKCount != 1 || counts.Total != 1 {
			t.Fatalf("an outcome on a site the pool does not hold counted %d/%d, want 1/1", counts.OKCount, counts.Total)
		}
	})
}
