package model

import (
	"sort"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// The spacing the owner chose (task #57, option A): a tenth of RefreshAge.
// Spelled out here so these tests also run against the code before the change.
const testingSpreadSpacing = ProviderEgressProbeRefreshAge / ProviderUrlProbeRunTarget

// One accepted measured run through the real claim, accepted-history writer
// and completion, the way a prober turn reaches the API.
func testingSpreadMeasuredTurn(t testing.TB, client server.Id, claimAt time.Time, arrange bool) time.Time {
	t.Helper()
	ctx := t.Context()
	var due ProviderUrlProbeDue
	if arrange {
		due = testingClaimUrlCompletion(t, ctx, client, claimAt)
	} else {
		claimed := ClaimProviderUrlProbeDue(ctx, claimAt, 1, 0, 1)
		if len(claimed) != 1 || claimed[0].ClientId != client {
			t.Fatalf("no renewal turn at its stored deadline %s: %+v", claimAt, claimed)
		}
		due = claimed[0]
	}
	measuredAt := claimAt.Add(time.Second)
	testingSetUrlProbeHealth(ctx, &ProviderEgressHealth{RunId: server.NewId(), ClientId: client,
		CycleStartedAt: due.CycleStartedAt, MeasuredAt: measuredAt, OKCount: 1, Total: 1})
	testingCompleteUrlClaim(t, ctx, due, measuredAt.Add(time.Second), "")
	return measuredAt
}

// The latest ten runs of a sorted history.
func testingSpreadLatestTen(runs []time.Time) []time.Time {
	return runs[max(0, len(runs)-ProviderUrlProbeRunTarget):]
}

// A cohort measured together, as after an outage catch-up at the one-minute
// recovery pace, used to stay due together: every renewal was anchored to the
// oldest counted run, so the ten runs were replaced in a ten-minute burst every
// 3h54m. Through the real writer, claim and completion, the renewal after such
// a cluster is now due 24 minutes after the latest result, and each later
// renewal 24 minutes after its predecessor until the replacement horizon of
// the cluster's last run decides. One cycle later the ten runs are spread, and
// at every turn the provider still has ten current runs: no evidence ages out.
func TestUrlProbeRenewalSpreadDissolvesClusterWithoutAgeOut(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		client := testingUrlCompletionClients(t, now.Add(-3*time.Hour), 1)[0]
		horizon := ProviderEgressProbeRefreshAge - ProviderUrlProbeRenewalHeadroom
		runs := []time.Time{}
		for turn := range ProviderUrlProbeRunTarget {
			runs = append(runs, testingSpreadMeasuredTurn(t, client, now.Add(-30*time.Minute+time.Duration(turn)*time.Minute), true))
		}
		first, last := runs[0], runs[len(runs)-1]
		cycle := testingReadUrlCompletionCycle(t, ctx, client)
		// Before the change the deadline was first+3h54m, the cluster's own
		// replacement burst.
		if want := last.Add(testingSpreadSpacing); !cycle.next.Equal(want) || !want.Before(first.Add(horizon)) {
			t.Fatalf("quota-full deadline after a clustered cohort=%s, want latest+24m=%s (oldest+3h54m=%s)",
				cycle.next, want, first.Add(horizon))
		}
		if early := ClaimProviderUrlProbeDue(ctx, cycle.next.Add(-time.Microsecond), 1, 0, 1); len(early) != 0 {
			t.Fatalf("quota-full provider admitted before latest+24m: %+v", early)
		}
		// Renew at each stored deadline, as admission would.
		horizonRenewals := 0
		for len(runs) < 2*ProviderUrlProbeRunTarget {
			at := testingReadUrlCompletionCycle(t, ctx, client).next
			latestTen := testingSpreadLatestTen(runs)
			oldest, latest := latestTen[0], latestTen[len(latestTen)-1]
			want := latest.Add(testingSpreadSpacing)
			if oldest.Add(horizon).Before(want) {
				want = oldest.Add(horizon)
				horizonRenewals++
			}
			if !at.Equal(want) {
				t.Fatalf("renewal %d deadline=%s, want min(oldest+3h54m, latest+24m)=%s", len(runs), at, want)
			}
			// No gap: ten runs are current when the renewal is claimed, and
			// the run it replaces is still current when the renewal lands.
			if fleet := testingGetUrlProbeFleet(t, ctx, at); fleet.QuotaComplete != 1 || fleet.RunsNeeded != 0 {
				t.Fatalf("coverage lost before renewal %d at %s: %+v", len(runs), at, fleet)
			}
			measuredAt := testingSpreadMeasuredTurn(t, client, at, false)
			if !measuredAt.Before(oldest.Add(ProviderEgressProbeRefreshAge)) {
				t.Fatalf("renewal %d measured at %s after its oldest run expired at %s", len(runs), measuredAt, oldest.Add(ProviderEgressProbeRefreshAge))
			}
			runs = append(runs, measuredAt)
			if fleet := testingGetUrlProbeFleet(t, ctx, measuredAt); fleet.QuotaComplete != 1 || fleet.RunsNeeded != 0 {
				t.Fatalf("coverage lost after renewal %d at %s: %+v", len(runs)-1, measuredAt, fleet)
			}
		}
		// Nine renewals took the spacing; the cluster's last run reached its
		// replacement horizon first only once.
		if horizonRenewals != 1 {
			t.Fatalf("horizon decided %d of ten renewals, want 1", horizonRenewals)
		}
		latestTen := testingSpreadLatestTen(runs)
		sort.Slice(latestTen, func(i, j int) bool { return latestTen[i].Before(latestTen[j]) })
		for i := 1; i < len(latestTen); i++ {
			if gap := latestTen[i].Sub(latestTen[i-1]); gap < 10*time.Minute {
				t.Fatalf("latest ten still clustered after one cycle: gap %s between %s and %s", gap, latestTen[i-1], latestTen[i])
			}
		}
		// The quota stays ten: one cycle took ten renewals, as before.
		if cycle := testingReadUrlCompletionCycle(t, ctx, client); cycle.history != 2*ProviderUrlProbeRunTarget || cycle.ordinal != 2*ProviderUrlProbeRunTarget {
			t.Fatalf("one cycle of renewals emitted surplus work: %+v", cycle)
		}
	})
}

// The writer and admission's re-park store the same deadline at each boundary
// of the rule. History older than the latest run is inserted directly; the
// latest run goes through the accepted-history writer.
func TestUrlProbeRenewalSpreadBoundaries(t *testing.T) {
	horizon := ProviderEgressProbeRefreshAge - ProviderUrlProbeRenewalHeadroom
	for _, c := range []struct {
		name string
		// the span from the oldest to the latest of ten runs
		span       time.Duration
		wantLatest bool
	}{
		{"cluster", 9 * time.Minute, true},
		{"spacing one second before horizon", horizon - testingSpreadSpacing - time.Second, true},
		{"spacing equals horizon", horizon - testingSpreadSpacing, true},
		{"spacing one second after horizon", horizon - testingSpreadSpacing + time.Second, false},
		{"evenly spread", 9 * testingSpreadSpacing, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			server.DefaultTestEnv().Run(t, func(t testing.TB) {
				ctx := t.Context()
				now := server.NowUtc().Truncate(time.Microsecond)
				client := testingUrlCompletionClients(t, now.Add(-5*time.Hour), 1)[0]
				latest := now.Add(-time.Minute)
				oldest := latest.Add(-c.span)
				server.Tx(ctx, func(tx server.PgTx) {
					for i := range ProviderUrlProbeRunTarget - 1 {
						at := oldest.Add(time.Duration(i) * c.span / time.Duration(ProviderUrlProbeRunTarget-1))
						server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_health_history
 (run_id,client_id,measured_at,ok_count,total_count,class_results,tls_authentication_failure,url_probe,url_probe_policy_version)
 VALUES($1,$2,$3,1,1,'{}',false,true,$4)`, server.NewId(), client, at, SelectedProviderUrlProbePolicyVersion()))
					}
				})
				var cycleStartedAt time.Time
				server.Db(ctx, func(conn server.PgConn) {
					server.Raise(conn.QueryRow(ctx, `SELECT cycle_started_at FROM provider_egress_probe_cycle WHERE client_id=$1`, client).Scan(&cycleStartedAt))
				})
				testingSetUrlProbeHealth(ctx, &ProviderEgressHealth{RunId: server.NewId(), ClientId: client,
					CycleStartedAt: cycleStartedAt, MeasuredAt: latest, OKCount: 1, Total: 1})
				want := oldest.Add(horizon)
				if c.wantLatest {
					want = latest.Add(testingSpreadSpacing)
				}
				if c.wantLatest && oldest.Add(horizon).Before(want) || !c.wantLatest && latest.Add(testingSpreadSpacing).Before(want) {
					t.Fatal("fixture does not sit on the intended side of the boundary")
				}
				if cycle := testingReadUrlCompletionCycle(t, ctx, client); !cycle.next.Equal(want) {
					t.Fatalf("writer deadline=%s, want %s (oldest+3h54m=%s, latest+24m=%s)", cycle.next, want,
						oldest.Add(horizon), latest.Add(testingSpreadSpacing))
				}
				// Admission finds the row early and re-parks it at the same deadline.
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_probe_cycle SET next_attempt_at=$2 WHERE client_id=$1`, client, now))
				})
				if due := ClaimProviderUrlProbeDue(ctx, now, 1, 0, 1); len(due) != 0 {
					t.Fatalf("quota-full provider admitted before its renewal deadline: %+v", due)
				}
				if cycle := testingReadUrlCompletionCycle(t, ctx, client); !cycle.next.Equal(want) || cycle.ordinal != 0 {
					t.Fatalf("admission re-parked at %s ordinal %d, want %s", cycle.next, cycle.ordinal, want)
				}
				// Without a latest result admission keeps the replacement horizon.
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_probe_cycle SET next_attempt_at=$2, latest_result_at=NULL WHERE client_id=$1`, client, now))
				})
				if due := ClaimProviderUrlProbeDue(ctx, now, 1, 0, 1); len(due) != 0 {
					t.Fatalf("quota-full provider without a latest result admitted early: %+v", due)
				}
				if cycle := testingReadUrlCompletionCycle(t, ctx, client); !cycle.next.Equal(oldest.Add(horizon)) {
					t.Fatalf("NULL latest result re-parked at %s, want oldest+3h54m=%s", cycle.next, oldest.Add(horizon))
				}
			})
		})
	}
}
