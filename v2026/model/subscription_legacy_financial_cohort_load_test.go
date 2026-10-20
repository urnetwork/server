// Matched finite loads use the public shard owner and drain its durable outputs.
package model

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

func TestLegacyFinancialCohortLoadedSamePayerSerial(t *testing.T) {
	legacyFinancialCohortLoaded(t, 1, false)
}
func TestLegacyFinancialCohortLoadedSamePayerConcurrent(t *testing.T) {
	legacyFinancialCohortLoaded(t, 4, false)
}
func TestLegacyFinancialCohortLoadedSamePayerCold(t *testing.T) {
	legacyFinancialCohortLoaded(t, 4, true)
}

// Sixteen shards share one payer network and two grants 15:1; each shard has
// four providers. Independent queued mirror/provider owners drain through the
// actual worker, so reducing inline work cannot hide a growing output backlog.
func legacyFinancialCohortLoaded(t *testing.T, workers int, cold bool) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
		defer cancel()
		if os.Getenv("URN_LEGACY_REQUIRE_PGSS") == "1" {
			server.Db(ctx, func(conn server.PgConn) {
				server.RaisePgResult(conn.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS pg_stat_statements`))
			})
		}
		const count = 1024
		fixture := legacyFinancialCohortSeed(t, ctx, count, 16)
		if cold {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_balance_net_escrow_snapshot WHERE balance_id=ANY($1)`, fixture.balances))
			})
		}
		hook := &legacyHotpathRedisCountHook{}
		server.Redis(ctx, func(client server.RedisClient) { client.AddHook(hook) })
		server.RedisDoOnce(ctx, func(client server.RedisClient) { client.AddHook(hook) })
		server.Raise(server.RedisWithDeadline(ctx, func(client server.RedisClient) error { client.AddHook(hook); return nil }))
		measured := hook.context(ctx)
		cursors := make([]*LegacySettlementCursor, 16)
		payerCursors := make([]*LegacySettlementPayerCursor, 16)
		type pageResult struct {
			shard int
			page  LegacySettlementShardResult
			err   error
		}
		var pages []LegacySettlementShardResult
		completed, visits, busy, rounds := 0, 0, 0, 0
		beforeSql := legacyTargetSqlSnapshot(t, ctx)
		started := time.Now()
		for completed < count {
			previous := completed
			rounds++
			if rounds > count {
				t.Fatal("finite loaded cohort did not drain")
			}
			for offset := 0; offset < 16; offset += workers {
				results := make(chan pageResult, workers)
				var joined sync.WaitGroup
				for slot := 0; slot < workers && offset+slot < 16; slot++ {
					shard := offset + slot
					joined.Add(1)
					go func() {
						defer joined.Done()
						value := pageResult{shard: shard}
						server.HandleError(func() {
							value.page, value.err = FlushLegacySettlementShard(measured, shard, cursors[shard], payerCursors[shard], 256)
						}, func(err error) { value.err = err })
						results <- value
					}()
				}
				joined.Wait()
				close(results)
				for value := range results {
					if value.err != nil || value.page.Failed != 0 {
						t.Fatal("loaded public owner failed", value.shard, value.page, value.err)
					}
					pages = append(pages, value.page)
					completed += value.page.Completed
					visits += value.page.Visited
					busy += value.page.BusyOrGone
					wire, err := json.Marshal(value.page.Cursor)
					server.Raise(err)
					server.Raise(json.Unmarshal(wire, &cursors[value.shard]))
					wire, err = json.Marshal(value.page.PayerCursor)
					server.Raise(err)
					server.Raise(json.Unmarshal(wire, &payerCursors[value.shard]))
				}
			}
			if completed <= previous {
				t.Fatal("joined wave had no committed progress", rounds, completed)
			}
		}
		financialElapsed := time.Since(started)
		if completed != count {
			t.Fatal("loaded completion count escaped fixed cohort", completed)
		}
		drain, drainErr := legacyFinancialDrainOwners(t, measured, count+2, nil)
		if drainErr != nil {
			t.Fatal("durable output failed finite eligible worker drain", drain, drainErr)
		}
		pendingBefore, finished := drain.Initial, drain.Finished
		includingOwners := time.Since(started)
		afterSql := legacyTargetSqlSnapshot(t, ctx)
		commands, dispatches := hook.snapshot()
		if finished != count+2 || pendingBefore != count+2 {
			t.Fatal("coalesced mirror or immutable provider owner cardinality differs", pendingBefore, finished)
		}
		legacyFinancialCohortRequire(t, ctx, fixture, legacyFinancialCohortCompleted(fixture.ids))
		for _, balanceId := range fixture.balances {
			if Testing_NetEscrowByteCount(ctx, balanceId) != 0 {
				t.Fatal("drained owner left stale legacy admission capacity")
			}
		}
		for shard := range 16 {
			page, err := FlushLegacySettlementShard(ctx, shard, nil, nil, 256)
			if err != nil || page.Completed != 0 || page.Visited != 0 {
				t.Fatal("loaded replay repeated financial work", page, err)
			}
		}
		legacyFinancialCohortRequire(t, ctx, fixture, legacyFinancialCohortCompleted(fixture.ids))
		out := map[string]any{
			"profile": os.Getenv("URN_LEGACY_FINANCIAL_COHORT_PROFILE"), "contracts": count, "workers": workers, "cold_cache": cold,
			"payer_networks": 1, "shared_grants": 2, "grant_distribution": "15:1 within each shard", "providers": 4, "shards": 16,
			"rounds": rounds, "visits": visits, "completed": completed, "busy": busy, "pages": pages,
			"financial_and_joined_page_wall_ns": financialElapsed.Nanoseconds(), "including_owner_drain_wall_ns": includingOwners.Nanoseconds(),
			"closes_per_second": float64(completed) / financialElapsed.Seconds(), "closes_per_second_including_owners": float64(completed) / includingOwners.Seconds(),
			"pending_owners_before_drain": pendingBefore, "finished_owners": finished, "pending_owners_after_drain": 0, "owner_drain": drain,
			"redis_commands": commands, "redis_client_dispatches": dispatches, "sql": legacyTargetSqlDelta(t, beforeSql, afterSql),
			"qualifiers": []string{"Matched source/module/fixture runs are required; a profile label is not source identity.",
				"This local finite same-payer load is not Main overall close throughput, workload mix or a sustained5x claim.",
				"The full public shard owner includes payer fairness, cohort rollbacks/fallbacks, individual heads and joined page posts.",
				"Durable provider/mirror owners drain through their actual registered worker target and finalization; observed future-availability waiting and all eligibility reads remain inside total cost.",
				"Phase families overlap; counts are owner invocations, and a cohort can serve several contract outcomes.",
				"SQL statement totals include explicitly separate nested work; normalized query text volume is not protocol bytes.",
				"Redis hook dispatches are not cluster network round trips. The separate protocol test counts real PostgreSQL Sync/ReadyForQuery and bytes.",
				"Pages are invoked immediately on four bounded fixture slots; this does not model production scheduler gaps or external open traffic."},
		}
		raw, err := json.Marshal(out)
		server.Raise(err)
		t.Logf("legacy_financial_cohort_load=%s", raw)
	})
}
