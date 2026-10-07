// Local lock graphs expose the synchronous tally's shared-place critical
// section without treating elapsed time, buffer hits, or CPU as lock evidence.
package model

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Resolves one fixture-owned blocker edge. Polls are bounded and paced; the
// observed PostgreSQL dependency, not the number of polls, is the assertion.
func testingTallyBlockedBackend(ctx context.Context, holderPid int, statement string) (blockedPid int) {
	server.Db(ctx, func(conn server.PgConn) {
		for {
			server.Raise(conn.QueryRow(ctx, `SELECT COALESCE((
				SELECT pid FROM pg_stat_activity WHERE datname=current_database()
				AND $1=ANY(pg_blocking_pids(pid)) AND query LIKE $2 LIMIT 1),0)`, holderPid, statement).Scan(&blockedPid))
			if blockedPid != 0 {
				return
			}
			select {
			case <-ctx.Done():
				server.Raise(ctx.Err())
			case <-time.After(5 * time.Millisecond):
			}
		}
	})
	return
}

// A site wait retains the earlier place lock. A second run that touches a
// different site is consequently blocked on the place, even at read committed.
// This positive mechanism control accompanies the publication regression; it
// does not claim that the corresponding lock graph was observed on Main.
func TestProviderEgressTallySiteWaitRetainsSharedPlaceLock(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		var workers sync.WaitGroup
		defer func() {
			cancel()
			workers.Wait()
		}()
		day := time.Date(2026, time.October, 6, 0, 0, 0, 0, time.UTC)
		run := ProviderEgressRunTally{Place: ProviderEgressPlace{CountryCode: "zz", Region: "Synthetic lock chain"}, Healthy: true}
		first := ProviderEgressSiteLoad{Name: "a-synthetic.example", Ok: true, Healthy: true}
		second := ProviderEgressSiteLoad{Name: "b-synthetic.example", Ok: true, Healthy: true}
		AddProviderEgressRunTally(ctx, day, run, []ProviderEgressSiteLoad{first, second})
		finished := make(chan any, 2)
		start := func(load ProviderEgressSiteLoad) {
			workers.Add(1)
			go func() {
				defer workers.Done()
				finished <- server.HandleError(func() {
					AddProviderEgressRunTally(ctx, day, run, []ProviderEgressSiteLoad{load})
				})
			}()
		}
		server.Tx(ctx, func(tx server.PgTx) {
			var siteHolderPid int
			server.Raise(tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&siteHolderPid))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_site_tally
				SET update_time=update_time+interval '1 microsecond'
				WHERE tally_day=$1 AND name=$2 AND country_code=$3 AND region=$4`,
				day, first.Name, run.Place.CountryCode, run.Place.Region))
			start(first)
			placeHolderPid := testingTallyBlockedBackend(ctx, siteHolderPid, "%INSERT INTO provider_egress_site_tally%")
			start(second)
			independentPid := testingTallyBlockedBackend(ctx, placeHolderPid, "%INSERT INTO provider_egress_place_tally%")
			if placeHolderPid == siteHolderPid || independentPid == placeHolderPid || independentPid == siteHolderPid {
				t.Fatal("the three fixture owners did not form a distinct site-to-place lock chain")
			}
			select {
			case err := <-finished:
				t.Fatalf("a tally completed while its observed blocker still owned the lock: %v", err)
			default:
			}
		}, server.TxReadCommitted, server.OptNoRetry())
		for range 2 {
			if err := <-finished; err != nil {
				t.Fatal(err)
			}
		}
		places := GetProviderEgressPlaceTallies(ctx, day)
		sites := GetProviderEgressSiteTallies(ctx, day)
		if len(places) != 1 || places[0].RunCount != 3 || places[0].HealthyRunCount != 3 || len(sites) != 2 {
			t.Fatalf("joined whole-run counters changed: places=%+v sites=%+v", places, sites)
		}
		for _, site := range sites {
			if site.LoadCount != 2 || site.HealthyLoadCount != 2 || site.FailureCount != 0 {
				t.Fatalf("an independent site's counters changed: %+v", site)
			}
		}
	})
}
