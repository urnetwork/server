package model

import (
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
)

// The old aggregate contract permits status-only and bodyless successes. It
// remains an audit record, never evidence for the real-content URL contract.
func TestFp2LegacyEvidenceDoesNotQualify(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		clientId := server.NewId()
		legacy := &ProviderEgressHealth{RunId: server.NewId(), ClientId: clientId, MeasuredAt: server.NowUtc(), OKCount: 10, Total: 10}
		SetProviderEgressHealth(ctx, legacy)
		// Reusing the run id cannot upgrade the meaning of an immutable receipt.
		SetProviderEgressHealth(ctx, &ProviderEgressHealth{RunId: legacy.RunId, ClientId: clientId, MeasuredAt: legacy.MeasuredAt,
			OKCount: 1, Total: 1, UrlProbeEvidence: fp2TestUrlProbeEvidence(legacy.MeasuredAt, true)})
		if counts, exists := GetAllProviderEgressHealthCounts(ctx)[clientId]; exists {
			t.Fatalf("legacy status-only evidence qualified: %+v", counts)
		}
		if health := GetProviderEgressHealth(ctx, clientId); health == nil || health.OKCount != 10 {
			t.Fatal("legacy diagnostic history was discarded")
		}
	})
}

// Readers choose exactly one known version, and old/future history cannot
// improve the ratio. Quarantine is deliberately outside that history selector.
func TestFp2UrlPolicyVersionSelectorAndIndependentSecurity(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		clientId := server.NewId()
		now := server.NowUtc().Add(-time.Minute).Truncate(time.Microsecond)
		cycleStartedAt := now.Add(-time.Hour)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_probe_cycle(client_id,cycle_started_at,next_attempt_at) VALUES($1,$2,$2)`, clientId, cycleStartedAt))
		})
		for index, ok := range []int{1, 0} {
			at := now.Add(time.Duration(index) * time.Second)
			SetProviderEgressHealth(ctx, &ProviderEgressHealth{RunId: server.NewId(), ClientId: clientId, CycleStartedAt: cycleStartedAt, MeasuredAt: at,
				OKCount: ok, Total: 1, UrlProbeEvidence: fp2TestUrlProbeEvidence(at, ok == 1)})
		}
		server.Tx(ctx, func(tx server.PgTx) {
			for _, version := range []int{0, 2} {
				// Simulates immutable rows imported from an older/newer writer.
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_health_history
					(run_id,client_id,measured_at,ok_count,total_count,class_results,tls_authentication_failure,url_probe,url_probe_policy_version)
					VALUES($1,$2,$3,1,1,'{}',false,true,$4)`, server.NewId(), clientId, now, version))
			}
		})
		counts := GetAllProviderEgressHealthCounts(ctx)[clientId]
		if counts.OKCount != 1 || counts.Total != 2 {
			t.Fatalf("unselected evidence affected the ratio: %+v", counts)
		}
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := conn.Query(ctx, providerUrlProbeSuccessWindowSql("$1", "$2"), clientId, server.NowUtc())
			server.WithPgResult(rows, err, func() {
				if !rows.Next() {
					t.Fatal("missing rolling aggregate")
				}
				var successes int
				var oldest *time.Time
				server.Raise(rows.Scan(&successes, &oldest))
				if successes != 1 {
					t.Fatalf("unselected evidence affected rolling quota: successes=%d", successes)
				}
			})
		})
		failure := &ProviderEgressHealth{RunId: server.NewId(), ClientId: clientId, MeasuredAt: now.Add(-9 * time.Hour), Total: 1,
			TLSAuthenticationFailure: true, UrlProbeEvidence: fp2TestUrlEvidence(now.Add(-9*time.Hour), "https://affected.example/", true)}
		SetProviderEgressHealth(ctx, failure)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_health_history SET url_probe_policy_version=0 WHERE run_id=$1`, failure.RunId))
		})
		if !GetAllProviderEgressTLSAuthenticationFailedClientIds(ctx)[clientId] {
			t.Fatal("old-version/aged security evidence lost quarantine")
		}
		SetProviderEgressHealth(ctx, &ProviderEgressHealth{RunId: server.NewId(), ClientId: clientId, MeasuredAt: now, Total: 1,
			UrlProbeEvidence: fp2TestUrlEvidence(now, "https://affected.example/", false)})
		if GetAllProviderEgressTLSAuthenticationFailedClientIds(ctx)[clientId] {
			t.Fatal("validated same-URL recovery did not clear the old finding")
		}
	})
}

// A bad selector is a configuration error, never a request for future formats.
func TestFp2UrlPolicySelectorValidation(t *testing.T) {
	for _, version := range []int{0, 1, 2} {
		rules := DefaultProviderEgressRules()
		rules.UrlProbeResultVersion = version
		if err := rules.Validate(); (err == nil) != (version == egresshealth.UrlProbePolicyVersion) {
			t.Fatalf("selector=%d error=%v", version, err)
		}
	}
}

// Security can recover independently of a measured quality outcome. A clean
// security-only receipt must release the claim reservation at the quota
// replacement deadline, without inventing another successful or failed URL outcome. The
// preceding measured TLS failure counts toward the ten-run quota, so expiry
// of the earliest success alone does not reopen it.
func TestFp2UrlSecurityOnlyRecoveryRecomputesQuotaDeadline(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		clientId := server.NewId()
		now := server.NowUtc().Truncate(time.Microsecond)
		cycleStartedAt := now.Add(-4 * time.Hour)
		oldestSuccessAt := now.Add(-4*time.Hour + 2*time.Minute)
		oldestQuotaRunAt := now.Add(-3*time.Hour + time.Minute)
		quotaExpiresAt := oldestQuotaRunAt.Add(ProviderEgressProbeRefreshAge)
		renewalAt := quotaExpiresAt.Add(-ProviderUrlProbeRenewalHeadroom)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_probe_cycle(client_id,cycle_started_at,next_attempt_at) VALUES($1,$2,$2)`, clientId, cycleStartedAt))
		})
		for index := range 10 {
			at := now.Add(-3*time.Hour + time.Duration(index)*time.Minute)
			if index == 0 {
				at = oldestSuccessAt
			}
			SetProviderEgressHealth(ctx, &ProviderEgressHealth{RunId: server.NewId(), ClientId: clientId, CycleStartedAt: cycleStartedAt,
				MeasuredAt: at, OKCount: 1, Total: 1, UrlProbeEvidence: fp2TestUrlProbeEvidence(at, true)})
		}
		failureAt := now.Add(-time.Minute)
		SetProviderEgressHealth(ctx, &ProviderEgressHealth{RunId: server.NewId(), ClientId: clientId, CycleStartedAt: cycleStartedAt,
			MeasuredAt: failureAt, Total: 1, TLSAuthenticationFailure: true,
			UrlProbeEvidence: fp2TestUrlEvidence(failureAt, "https://affected.example/", true)})
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_probe_cycle SET next_attempt_at=$2 WHERE client_id=$1`, clientId, now.Add(15*time.Minute)))
		})
		security := fp2TestUrlEvidence(now, "https://affected.example/", false)
		security.FailureStage, security.PerformanceClassification = "response_encoding", ""
		SetProviderEgressHealth(ctx, &ProviderEgressHealth{RunId: server.NewId(), ClientId: clientId, CycleStartedAt: cycleStartedAt,
			MeasuredAt: now, NotMeasuredCount: 1, UrlProbeEvidence: security})
		if GetAllProviderEgressTLSAuthenticationFailedClientIds(ctx)[clientId] {
			t.Fatal("authenticated security-only receipt did not clear its exact URL")
		}
		if counts := GetAllProviderEgressHealthCounts(ctx)[clientId]; counts.OKCount != 10 || counts.Total != 11 {
			t.Fatalf("unmeasured security receipt changed the URL ratio: %+v", counts)
		}
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := conn.Query(ctx, `SELECT success_count,error_count,outcome_count,next_attempt_at FROM provider_egress_probe_cycle WHERE client_id=$1`, clientId)
			server.WithPgResult(rows, err, func() {
				if !rows.Next() {
					t.Fatal("missing rolling progress")
				}
				var successes, failures, outcomes int
				var next time.Time
				server.Raise(rows.Scan(&successes, &failures, &outcomes, &next))
				if successes != 10 || failures != 1 || outcomes != 11 || !next.Equal(renewalAt) {
					t.Fatalf("security-only receipt changed outcomes or measured quota deadline: successes=%d failures=%d outcomes=%d next=%s want=%s", successes, failures, outcomes, next, renewalAt)
				}
			})
			for _, boundary := range []struct {
				at   time.Time
				want int
			}{
				{oldestSuccessAt.Add(ProviderEgressProbeRefreshAge), 10},
				{quotaExpiresAt.Add(-time.Microsecond), 10},
				{quotaExpiresAt, 9},
			} {
				var measured int
				server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM provider_egress_health_history
					WHERE client_id=$1 AND url_probe AND url_probe_policy_version=$2 AND total_count=1
					AND measured_at>$3 AND measured_at<=$4`, clientId, SelectedProviderUrlProbePolicyVersion(),
					boundary.at.Add(-ProviderEgressProbeRefreshAge), boundary.at).Scan(&measured))
				if measured != boundary.want {
					t.Fatalf("wrong accepted measured count at %s: got=%d want=%d", boundary.at, measured, boundary.want)
				}
			}
		})
	})
}
