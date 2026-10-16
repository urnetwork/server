// Deterministic mixed rates expose amplification hidden by a healthy-only load.
package model

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

func TestLegacyFinancialCohortMixedBusyAndInvalidWork(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	for _, percentage := range []int{0, 1, 10, 50} {
		for _, kind := range []string{"busy", "invalid"} {
			if percentage == 0 && kind == "invalid" {
				continue
			}
			// Each fixed case owns a fresh database: retained refusal rows must
			// never become eligible in a later case.
			env.Run(t, func(t testing.TB) {
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
				defer cancel()
				if os.Getenv("URN_LEGACY_REQUIRE_PGSS") == "1" {
					server.Db(ctx, func(conn server.PgConn) {
						server.RaisePgResult(conn.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS pg_stat_statements`))
					})
				}
				f := legacyFinancialCohortSeed(t, ctx, 100)
				var selected []server.Id
				unavailable := map[server.Id]bool{}
				for index, id := range f.ids {
					if (index*37)%100 < percentage {
						selected = append(selected, id)
						unavailable[id] = true
						if kind == "invalid" {
							f.reservationAmounts[index] = 2
						}
					}
				}
				if kind == "invalid" {
					server.Tx(ctx, func(tx server.PgTx) {
						server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow SET balance_byte_count=2 WHERE contract_id=ANY($1)`, selected))
					})
					refreshNetEscrow(ctx, f.balances)
				}
				conn := acquireContractLifecycleTestConnection(t, ctx)
				defer conn.Release()
				held, err := conn.Begin(ctx)
				server.Raise(err)
				defer held.Rollback(context.Background())
				if kind == "busy" {
					server.RaisePgResult(held.Exec(ctx, `SELECT contract_id FROM transfer_contract WHERE contract_id=ANY($1) ORDER BY contract_id FOR UPDATE`, selected))
				}
				var cursor *LegacySettlementCursor
				var pages []LegacySettlementFlushResult
				visited, completed, failed, busy := 0, 0, 0, 0
				before := legacyTargetSqlSnapshot(t, ctx)
				started := time.Now()
				for pageIndex := 0; pageIndex < 201; pageIndex++ {
					page, err := FlushLegacySettlements(ctx, 1, cursor, 100)
					if err != nil {
						t.Fatal("mixed financial page failed", percentage, kind, err)
					}
					pages = append(pages, page)
					visited += page.Visited
					completed += page.Completed
					failed += page.Failed
					busy += page.BusyOrGone
					cursor = page.Cursor
					if cursor == nil {
						break
					}
					if pageIndex == 200 {
						t.Fatal("fixed mixed pass did not end")
					}
				}
				elapsed := time.Since(started)
				after := legacyTargetSqlSnapshot(t, ctx)
				server.Raise(held.Rollback(ctx))
				wantFailures := 0
				if kind == "invalid" {
					wantFailures = percentage
				}
				if completed != 100-percentage || failed != wantFailures {
					t.Fatal("mixed cohort changed complete/refusal counts", percentage, kind, completed, failed)
				}
				expected := legacyFinancialCohortCompleted(f.ids)
				for id := range unavailable {
					delete(expected, id)
				}
				legacyFinancialCohortRequire(t, ctx, f, expected)
				if kind == "busy" {
					page, err := FlushLegacySettlements(ctx, 1, nil, 100)
					if err != nil || page.Completed != percentage || page.Failed != 0 || page.BusyOrGone != 0 {
						t.Fatal("lock-only release did not drain fixed unavailable subset", percentage, page, err)
					}
					legacyFinancialCohortRequire(t, ctx, f, legacyFinancialCohortCompleted(f.ids))
				}
				record := map[string]any{"profile": os.Getenv("URN_LEGACY_FINANCIAL_COHORT_PROFILE"), "contracts": 100, "percentage": percentage, "kind": kind,
					"wall_ns": elapsed.Nanoseconds(), "visited": visited, "completed": completed, "failed": failed, "busy": busy, "pages": pages, "sql": legacyTargetSqlDelta(t, before, after),
					"qualifiers": []string{"Unavailable row membership is fixed by an actual contract lock or insufficient own escrow, not random concurrency.",
						"Every known cohort rollback/fallback remains inside the measured interval; no wall-clock speed threshold is a correctness oracle.",
						"Invalid rows remain reserved and deferred; no credit, report or escrow repair runs after setup.",
						"This mixed control retains durable provider/mirror work but does not drain it; the separate loaded public-owner tests include that full drain.",
						"Compare identical baseline/candidate source graphs before inferring workload cost."}}
				wire, err := json.Marshal(record)
				server.Raise(err)
				t.Logf("legacy_financial_cohort_mixed=%s", wire)
			})
		}
	}
}
