// URL probe admission waits for reliability history; serving keeps it neutral.
package model

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Runs only the location pass. Fresh handler heartbeats keep a slow run's
// fixture connections live, so the pass always sees them connected.
func testingHistoryAdmissionLocationPass(t testing.TB, ctx context.Context, clientIds ...server.Id) {
	t.Helper()
	now := server.NowUtc()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client_handler SET heartbeat_time=$2
			WHERE handler_id IN (SELECT handler_id FROM network_client_connection WHERE client_id=ANY($1::uuid[]))`,
			clientIds, now))
	})
	UpdateClientLocationReliabilities(ctx, now.Add(-time.Hour), now)
}

// Replaces the client's score generation and publishes the scheduling hints
// in the same transaction, as the score pass does.
func testingHistoryAdmissionScores(t testing.TB, ctx context.Context, clientId server.Id, lookbackIndexWeights map[int]float64) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `DELETE FROM client_connection_reliability_score WHERE client_id=$1`, clientId))
		for lookbackIndex, weight := range lookbackIndexWeights {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO client_connection_reliability_score
				(client_id,lookback_index,independent_reliability_score,independent_reliability_weight,reliability_score,reliability_weight)
				VALUES ($1,$2,$3,$3,$3,$3)`, clientId, lookbackIndex, weight))
		}
		updateProviderUrlProbeEligibility(ctx, tx)
	})
}

// A client's scheduling row, if any: its hint and its first-admission time.
type testingHistoryAdmissionCycle struct {
	exists         bool
	eligible       bool
	cycleStartedAt time.Time
}

// Reads the scheduling row without creating or changing one.
func testingReadHistoryAdmissionCycle(t testing.TB, ctx context.Context, clientId server.Id) testingHistoryAdmissionCycle {
	t.Helper()
	cycle := testingHistoryAdmissionCycle{}
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `SELECT eligible,cycle_started_at FROM provider_egress_probe_cycle WHERE client_id=$1`, clientId)
		server.WithPgResult(rows, err, func() {
			if rows.Next() {
				cycle.exists = true
				server.Raise(rows.Scan(&cycle.eligible, &cycle.cycleStartedAt))
			}
		})
	})
	return cycle
}

// The location pass commits before the score pass. A new connection without
// history must not be counted, claimed or given a cycle there: on Main its
// first failing scores followed 20 to 130 seconds later, after the scheduler
// had claimed it, and the cycle left behind made its real admission about
// eight hours later count as mature with no runs instead of warming.
func TestUrlProbeAdmissionWaitsForReliabilityHistory(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		start := server.NowUtc()
		// Censuses and claims up to the failing scores read this one instant,
		// which follows every pass below.
		at := start.Add(time.Hour)

		_, matureClientId, city := locationProbeConnectedFixture(t, ctx, 141)
		testingInsertProviderConnectionScore(ctx, matureClientId, city)
		testingHistoryAdmissionLocationPass(t, ctx, matureClientId)
		matureStartedAt := start.Add(-8 * time.Hour)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_probe_cycle SET cycle_started_at=$2 WHERE client_id=$1`, matureClientId, matureStartedAt))
		})
		testingAdmissionHealth(ctx, matureClientId, matureStartedAt, start, ProviderUrlProbeRunTarget)
		// The quota-complete provider stays out of every claim below.
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_probe_cycle SET next_attempt_at=$2 WHERE client_id=$1`, matureClientId, at.Add(time.Hour)))
		})
		_, newClientId, _ := locationProbeConnectedFixture(t, ctx, 142)
		if cycle := testingReadHistoryAdmissionCycle(t, ctx, newClientId); cycle.exists {
			t.Fatalf("newcomer fixture already has a cycle: %+v", cycle)
		}
		before := testingAdmissionCohort(t, ctx, at)
		if before.Eligible != 1 || before.MatureEligible != 1 || before.QuotaComplete != 1 || before.Due != 0 ||
			before.Warming != 0 || before.RunsNeeded != 0 || before.MissingCycles != 0 {
			t.Fatalf("mature quota-complete fixture: %+v", before)
		}

		testingHistoryAdmissionLocationPass(t, ctx, matureClientId, newClientId)
		if after := testingAdmissionCohort(t, ctx, at); !reflect.DeepEqual(after, before) {
			t.Errorf("location pass counted a newcomer without history: before=%+v after=%+v", before, after)
		}
		if cycle := testingReadHistoryAdmissionCycle(t, ctx, newClientId); cycle.exists {
			t.Errorf("location pass opened a cycle without history: %+v", cycle)
		}
		for _, due := range ClaimProviderUrlProbeDue(ctx, at, 10, 0, 1) {
			if due.ClientId == newClientId {
				t.Errorf("newcomer without history was claimed: %+v", due)
			}
		}

		// The score pass writes the newcomer's first scores, which fail.
		testingHistoryAdmissionScores(t, ctx, newClientId, map[int]float64{0: 0.5, 1: 0.02, 2: 0.001})
		if after := testingAdmissionCohort(t, ctx, at); !reflect.DeepEqual(after, before) {
			t.Errorf("failing first scores changed the census: before=%+v after=%+v", before, after)
		}
		if cycle := testingReadHistoryAdmissionCycle(t, ctx, newClientId); cycle.eligible {
			t.Errorf("failing first scores left the newcomer eligible: %+v", cycle)
		}

		// About eight hours later its history first passes. Age any cycle the
		// location pass opened by those hours, as the clock would have.
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_probe_cycle SET cycle_started_at=cycle_started_at-interval '8 hours',
				next_attempt_at=next_attempt_at-interval '8 hours' WHERE client_id=$1`, newClientId))
		})
		admitStart := server.NowUtc()
		testingHistoryAdmissionScores(t, ctx, newClientId, map[int]float64{0: 1, 1: 1, 2: 0.75})
		admitEnd := server.NowUtc()
		cycle := testingReadHistoryAdmissionCycle(t, ctx, newClientId)
		if !cycle.exists || !cycle.eligible || cycle.cycleStartedAt.Before(admitStart) || cycle.cycleStartedAt.After(admitEnd) {
			t.Errorf("admission did not start the newcomer's cycle within [%s, %s]: %+v", admitStart, admitEnd, cycle)
		}
		admitted := testingAdmissionCohort(t, ctx, admitEnd)
		if admitted.Eligible != 2 || admitted.WarmingEligible != 1 || admitted.WarmingRunsNeeded != ProviderUrlProbeRunTarget ||
			admitted.MatureEligible != 1 || admitted.MatureQuotaComplete != 1 || admitted.MatureRunsNeeded != 0 ||
			admitted.EligibilityAgeUnknown != 0 {
			t.Errorf("admitted newcomer skipped its warm-up: %+v", admitted)
		}
		claimed := ClaimProviderUrlProbeDue(ctx, admitEnd, 10, 0, 1)
		if len(claimed) != 1 || claimed[0].ClientId != newClientId || !claimed[0].CycleStartedAt.Equal(cycle.cycleStartedAt) {
			t.Errorf("admitted newcomer was not claimed from its admission: %+v", claimed)
		}
	})
}

// Waiting for history leaves the other admissions alone: an intent provider in
// its qualification grace has none yet, and a scored provider may lack a
// lookback. Serving still treats missing history as neutral.
func TestUrlProbeAdmissionWithoutHistoryKeepsIntentPartialHistoryAndServing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		_, intentClientId, _ := locationProbeConnectedFixture(t, ctx, 151)
		_, partialClientId, _ := locationProbeConnectedFixture(t, ctx, 152)
		_, unscoredClientId, _ := locationProbeConnectedFixture(t, ctx, 153)
		prioritySince := server.NowUtc().Add(-time.Hour)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_intent_probe_priority(client_id,priority_since,update_time)
				VALUES($1,$2,$2)`, intentClientId, prioritySince))
			// The 12 hour lookback has no row yet.
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO client_connection_reliability_score
				(client_id,lookback_index,independent_reliability_score,independent_reliability_weight,reliability_score,reliability_weight)
				VALUES ($1,0,1,1,1,1),($1,1,1,1,1,1)`, partialClientId))
		})
		testingHistoryAdmissionLocationPass(t, ctx, intentClientId, partialClientId, unscoredClientId)
		at := server.NowUtc()

		for _, c := range []struct {
			name     string
			clientId server.Id
			admitted bool
		}{
			{name: "intent", clientId: intentClientId, admitted: true},
			{name: "partial", clientId: partialClientId, admitted: true},
			{name: "unscored", clientId: unscoredClientId, admitted: false},
		} {
			if cycle := testingReadHistoryAdmissionCycle(t, ctx, c.clientId); cycle.exists != c.admitted || cycle.eligible != c.admitted {
				t.Errorf("%s location admission: want admitted=%t, cycle=%+v", c.name, c.admitted, cycle)
			}
		}
		fleet := testingAdmissionCohort(t, ctx, at)
		if fleet.Eligible != 2 || fleet.WarmingEligible != 2 || fleet.WarmingRunsNeeded != 2*ProviderUrlProbeRunTarget || fleet.MissingCycles != 0 {
			t.Errorf("census lost the intent or partially scored provider: %+v", fleet)
		}
		claimedClientIds := map[server.Id]bool{}
		for _, due := range ClaimProviderUrlProbeDue(ctx, at, 10, 0, 1) {
			claimedClientIds[due.ClientId] = true
		}
		if len(claimedClientIds) != 2 || !claimedClientIds[intentClientId] || !claimedClientIds[partialClientId] {
			t.Errorf("claims differ from the intent and partially scored providers: %v", claimedClientIds)
		}

		var urlAdmitted, reliabilityEligible, probeEligible, egressEligible bool
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT `+providerUrlProbeAdmissionSql("provider")+`,`+
				providerReliabilityEligibilitySql("provider.client_id")+`,`+providerProbeEligibilitySql("provider")+`,`+
				providerEgressEligibilitySql("provider")+`
				FROM network_client_location_reliability AS provider WHERE provider.client_id=$1`, unscoredClientId).Scan(
				&urlAdmitted, &reliabilityEligible, &probeEligible, &egressEligible))
		})
		if urlAdmitted || !reliabilityEligible || !probeEligible || !egressEligible ||
			!providerReliabilityPasses(map[int]float64{}, providerReliabilityMinimums()) {
			t.Errorf("serving changed for missing history: url=%t reliability=%t probe=%t egress=%t",
				urlAdmitted, reliabilityEligible, probeEligible, egressEligible)
		}
	})
}

// Public-mode changes read the same admission as the location pass: without
// history the change opens no cycle, and once scored a re-entry seeds one.
func TestUrlProbePublicAdmissionWaitsForReliabilityHistory(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		networkId := server.NewId()
		clientId := testingCreateProviderClient(ctx, networkId, nil, true)
		city := egressTestCity(ctx, "Public History City", "Public History Region", "Public History Country", "zz")
		testingInsertProviderLocationReliability(ctx, clientId, networkId, city)
		SetProvide(ctx, clientId, publicAdmissionKeys())
		if exists, eligible := publicAdmissionCycle(t, ctx, clientId); exists || eligible {
			t.Errorf("public mode opened a cycle without history: exists=%t eligible=%t", exists, eligible)
		}
		testingInsertProviderConnectionScore(ctx, clientId, city)
		SetProvide(ctx, clientId, publicAdmissionKeys())
		if exists, eligible := publicAdmissionCycle(t, ctx, clientId); !exists || !eligible {
			t.Errorf("scored public re-entry did not seed its cycle: exists=%t eligible=%t", exists, eligible)
		}
	})
}
