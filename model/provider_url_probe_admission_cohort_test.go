// First-admission coverage gives new providers one warm-up without hiding old deficits.
package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// The three age partitions must retain every admitted provider and measured credit.
func testingAdmissionCohort(t testing.TB, ctx context.Context, at time.Time) ProviderUrlProbeFleet {
	t.Helper()
	fleet := GetProviderUrlProbeFleet(ctx, at)
	if fleet.MatureEligible+fleet.WarmingEligible+fleet.EligibilityAgeUnknown != fleet.Eligible ||
		fleet.MatureQuotaComplete+fleet.WarmingQuotaComplete+fleet.AgeUnknownQuotaComplete != fleet.QuotaComplete ||
		fleet.MatureRunsNeeded+fleet.WarmingRunsNeeded+fleet.AgeUnknownRunsNeeded != fleet.RunsNeeded {
		t.Fatalf("admission-age partition lost providers or credit: %+v", fleet)
	}
	return fleet
}

func testingAdmissionHealth(ctx context.Context, clientId server.Id, cycle, at time.Time, count int) {
	for index := range count {
		testingSetUrlProbeHealth(ctx, &ProviderEgressHealth{RunId: server.NewId(), ClientId: clientId,
			CycleStartedAt: cycle, MeasuredAt: at.Add(-time.Minute + time.Duration(index)*time.Second),
			Total: 1, OKCount: index % 2})
	}
}

func TestUrlProbeAdmissionCohortNewJoinAndExactBoundary(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		at := server.NowUtc().Truncate(time.Microsecond)
		started := at.Add(-time.Hour)
		testingUrlCompletionClients(t, started, 1)
		for _, tc := range []struct {
			at     time.Time
			mature int
			warm   int
		}{
			{at, 0, 1},
			{started.Add(4*time.Hour - time.Microsecond), 0, 1},
			{started.Add(4 * time.Hour), 1, 0},
			{started.Add(4*time.Hour + time.Microsecond), 1, 0},
		} {
			fleet := testingAdmissionCohort(t, ctx, tc.at)
			if fleet.MatureEligible != tc.mature || fleet.WarmingEligible != tc.warm ||
				fleet.MatureRunsNeeded != 10*tc.mature || fleet.WarmingRunsNeeded != 10*tc.warm || fleet.EligibilityAgeUnknown != 0 {
				t.Fatalf("first-admission boundary at %s: %+v", tc.at, fleet)
			}
		}
	})
}

func TestUrlProbeAdmissionCohortCountsFailuresAndSeparatesSecurity(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		at := server.NowUtc().Truncate(time.Microsecond)
		started := at.Add(-8 * time.Hour)
		clients := testingUrlCompletionClients(t, started, 3)
		testingAdmissionHealth(ctx, clients[0], started, at, 9)
		testingAdmissionHealth(ctx, clients[1], started, at, 10)
		testingAdmissionHealth(ctx, clients[2], started, at, 3)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_probe_cycle SET cycle_started_at=$2 WHERE client_id=$1`, clients[2], at.Add(-time.Hour)))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_health SET legacy_tls_authentication_failure=true WHERE client_id=$1`, clients[1]))
		})
		fleet := testingAdmissionCohort(t, ctx, at)
		if fleet.MatureEligible != 2 || fleet.MatureQuotaComplete != 1 || fleet.MatureRunsNeeded != 1 ||
			fleet.WarmingEligible != 1 || fleet.WarmingQuotaComplete != 0 || fleet.WarmingRunsNeeded != 7 ||
			fleet.SecurityExceptions != 1 || fleet.Complete != 0 || fleet.QuotaComplete != 1 {
			t.Fatalf("measurement failures or TLS state changed quota partitions: %+v", fleet)
		}
		testingSetUrlProbeHealth(ctx, &ProviderEgressHealth{RunId: server.NewId(), ClientId: clients[0],
			CycleStartedAt: started, MeasuredAt: at, Total: 1, OKCount: 0})
		fleet = testingAdmissionCohort(t, ctx, at)
		if fleet.MatureEligible != 2 || fleet.MatureQuotaComplete != 2 || fleet.MatureRunsNeeded != 0 ||
			fleet.QuotaComplete != 2 || fleet.Eligible != 3 || fleet.RunsNeeded != 7 || fleet.SecurityExceptions != 1 {
			t.Fatalf("newcomer changed complete mature quota or final measured failure was lost: %+v", fleet)
		}
	})
}

func TestUrlProbeAdmissionCohortReentryCannotRenewWarmup(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		at := server.NowUtc().Truncate(time.Microsecond)
		started := at.Add(-8 * time.Hour)
		clientId := testingUrlCompletionClients(t, started, 1)[0]
		SetProvide(ctx, clientId, map[ProvideMode][]byte{})
		if fleet := testingAdmissionCohort(t, ctx, at); fleet.Eligible != 0 {
			t.Fatalf("withdrawn provider remained in denominator: %+v", fleet)
		}
		SetProvide(ctx, clientId, publicAdmissionKeys())
		server.Tx(ctx, func(tx server.PgTx) { updateProviderUrlProbeEligibility(ctx, tx) })
		assertMature := func() {
			var actual time.Time
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT cycle_started_at FROM provider_egress_probe_cycle WHERE client_id=$1`, clientId).Scan(&actual))
			})
			fleet := testingAdmissionCohort(t, ctx, at)
			if !actual.Equal(started) || fleet.MatureEligible != 1 || fleet.MatureRunsNeeded != 10 || fleet.WarmingEligible != 0 {
				t.Fatalf("re-entry granted an old provider another warm-up: started=%s fleet=%+v", actual, fleet)
			}
		}
		assertMature()
		for _, risk := range []bool{true, false} {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client_location_reliability SET arin_risk=$2 WHERE client_id=$1`, clientId, risk))
				updateProviderUrlProbeEligibility(ctx, tx)
			})
			if risk {
				if fleet := testingAdmissionCohort(t, ctx, at); fleet.Eligible != 0 {
					t.Fatalf("current risk exclusion ignored: %+v", fleet)
				}
			} else {
				assertMature()
			}
		}
	})
}

func TestUrlProbeAdmissionCohortMissingAndFutureAgeUnknown(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		at := server.NowUtc().Truncate(time.Microsecond)
		started := at.Add(-8 * time.Hour)
		clients := testingUrlCompletionClients(t, started, 3)
		testingAdmissionHealth(ctx, clients[0], started, at, 10)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM provider_egress_probe_cycle WHERE client_id=$1`, clients[0]))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_probe_cycle SET cycle_started_at=$2 WHERE client_id=$1`, clients[1], at.Add(time.Microsecond)))
		})
		fleet := testingAdmissionCohort(t, ctx, at)
		if fleet.EligibilityAgeUnknown != 2 || fleet.AgeUnknownQuotaComplete != 1 || fleet.AgeUnknownRunsNeeded != 10 ||
			fleet.MatureEligible != 1 || fleet.MatureRunsNeeded != 10 || fleet.WarmingEligible != 0 || fleet.MissingCycles != 1 {
			t.Fatalf("missing/future admission was assigned an invented age: %+v", fleet)
		}
	})
}

func TestUrlProbeAdmissionCohortUsesCurrentAdmissionGates(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		at := server.NowUtc().Truncate(time.Microsecond)
		clients := testingUrlCompletionClients(t, at.Add(-8*time.Hour), 5)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client_location_reliability SET arin_risk=true WHERE client_id=$1`, clients[0]))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET active=false WHERE client_id=$1`, clients[1]))
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM provide_key WHERE client_id=$1`, clients[2]))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client_location_reliability SET ipv4_proven=false,ipv6_proven=true WHERE client_id=$1`, clients[3]))
		})
		fleet := testingAdmissionCohort(t, ctx, at)
		if fleet.Eligible != 1 || fleet.MatureEligible != 1 || fleet.MatureRunsNeeded != 10 || fleet.RunsNeeded != 10 {
			t.Fatalf("old cycle age bypassed current admission gates: %+v", fleet)
		}
	})
}
