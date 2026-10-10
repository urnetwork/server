package model

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server/v2026"
)

// Retain the observed query as a result and query-work control.
const providerStatsTransferBytesBeforeSQL = `
SELECT tc.destination_id, COALESCE(SUM(cc.used_transfer_byte_count), 0)
FROM transfer_contract tc
INNER JOIN contract_close cc ON
    cc.contract_id = tc.contract_id AND cc.party = 'destination'
WHERE tc.destination_id = ANY($1::uuid[]) AND tc.close_time >= $2
GROUP BY tc.destination_id`

func providerTransferTestAmounts(t testing.TB, ctx context.Context, conn server.PgConn, sql string, ids []string, start time.Time) map[server.Id]int64 {
	t.Helper()
	amounts := map[server.Id]int64{}
	rows, err := conn.Query(ctx, sql, ids, start)
	server.WithPgResult(rows, err, func() {
		for rows.Next() {
			var id server.Id
			var amount int64
			server.Raise(rows.Scan(&id, &amount))
			if _, exists := amounts[id]; exists {
				t.Fatal("provider returned twice")
			}
			amounts[id] = amount
		}
	})
	return amounts
}

// Exercise the production tables and actual API, including the original lower
// bound only: future closes and canceled outcomes with reports still count.
func TestProviderStatsTransferQueryPreservesReportSemantics(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		start := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
		network, sourceNetwork, source := server.NewId(), server.NewId(), server.NewId()
		a, b, zero, idle, foreign := server.NewId(), server.NewId(), server.NewId(), server.NewId(), server.NewId()
		clientSession := providerPayoutTestSession(ctx, network)
		for _, id := range []server.Id{a, b, zero, idle} {
			statsInsertNetworkClient(ctx, network, id)
			statsInsertProvideKey(ctx, id, ProvideModePublic)
		}
		server.Db(ctx, func(conn server.PgConn) {
			for _, row := range []struct {
				provider server.Id
				closed   any
				outcome  any
				report   bool
				bytes    int64
			}{
				{a, start.Add(-time.Microsecond), "success", true, 999},
				{a, start, "success", true, 11},
				{a, start.Add(48 * time.Hour), "success", true, 13},
				{a, start.Add(time.Hour), "canceled", true, 17},
				{a, start.Add(2 * time.Hour), "success", false, 19},
				{a, nil, nil, true, 23},
				{b, start.Add(time.Hour), "success", true, 29},
				{zero, start, "success", true, 0},
				{foreign, start, "success", true, 997},
			} {
				id := server.NewId()
				server.RaisePgResult(conn.Exec(ctx, `INSERT INTO transfer_contract
					(contract_id,source_network_id,source_id,destination_network_id,destination_id,
					 transfer_byte_count,create_time,close_time,outcome)
					VALUES($1,$2,$3,$4,$5,1000,$6,$7,$8)`, id, sourceNetwork, source, network, row.provider,
					start.Add(-time.Hour), row.closed, row.outcome))
				server.RaisePgResult(conn.Exec(ctx, `INSERT INTO contract_close
					(contract_id,close_time,party,used_transfer_byte_count) VALUES($1,$2,'source',1001)`, id, start))
				if row.report {
					server.RaisePgResult(conn.Exec(ctx, `INSERT INTO contract_close
						(contract_id,close_time,party,used_transfer_byte_count) VALUES($1,$2,'destination',$3)`, id, start, row.bytes))
				}
			}
			server.RaisePgResult(conn.Exec(ctx, `INSERT INTO contract_close
				(contract_id,close_time,party,used_transfer_byte_count) VALUES($1,$2,'destination',1003)`, server.NewId(), start))
			for _, test := range []struct {
				name string
				ids  []server.Id
				want map[server.Id]int64
			}{
				{"empty", []server.Id{}, map[server.Id]int64{}},
				{"one", []server.Id{a}, map[server.Id]int64{a: 41}},
				{"duplicate", []server.Id{a, a, b}, map[server.Id]int64{a: 41, b: 29}},
				{"all", []server.Id{a, b, zero, idle}, map[server.Id]int64{a: 41, b: 29, zero: 0}},
				{"missing", []server.Id{server.NewId()}, map[server.Id]int64{}},
			} {
				for _, sql := range []string{providerStatsTransferBytesBeforeSQL, providerStatsTransferBytesSQL} {
					if got := providerTransferTestAmounts(t, ctx, conn, sql, idStrings(test.ids), start); !reflect.DeepEqual(got, test.want) {
						t.Fatalf("%s changed destination report totals: got=%v want=%v", test.name, got, test.want)
					}
				}
			}
		})
		result, err := statsProviders(clientSession, start, start.Add(24*time.Hour))
		if err != nil || result == nil || len(result.Providers) != 4 {
			t.Fatalf("actual provider API: result=%v err=%v", result, err)
		}
		want := map[server.Id]int64{a: 41, b: 29, zero: 0, idle: 0}
		for _, provider := range result.Providers {
			amount, ok := want[provider.ClientId]
			if !ok || provider.TransferDataLast24h != bytesToGib(amount) {
				t.Fatal("actual provider API changed report membership or byte conversion")
			}
			delete(want, provider.ClientId)
		}
		if len(want) != 0 {
			t.Fatal("actual provider API omitted a provider")
		}
	})
}

type providerTransferTestPlanNode struct {
	NodeType string                         `json:"Node Type"`
	Relation string                         `json:"Relation Name"`
	Index    string                         `json:"Index Name"`
	Cond     string                         `json:"Index Cond"`
	Rows     float64                        `json:"Actual Rows"`
	Loops    float64                        `json:"Actual Loops"`
	Removed  float64                        `json:"Rows Removed by Filter"`
	Recheck  float64                        `json:"Rows Removed by Index Recheck"`
	Hits     float64                        `json:"Shared Hit Blocks"`
	Reads    float64                        `json:"Shared Read Blocks"`
	Plans    []providerTransferTestPlanNode `json:"Plans"`
}

// Keep real migrated indexes and triggers. The separate native experiment uses
// 200k providers/660k contracts/1.52M reports; this smaller model fixture tests
// the same history-to-current skew without changing the application schema.
func TestProviderStatsTransferQueryWorkFollowsEligibleContracts(t *testing.T) {
	if testing.Short() {
		t.Skip("provider statistics query-work population")
	}
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		server.Db(ctx, func(conn server.PgConn) {
			for _, sql := range []string{
				`CREATE FUNCTION pg_temp.provider_transfer_ids(n integer) RETURNS uuid[] LANGUAGE sql STABLE AS $$
				 SELECT coalesce(array_agg(lpad(to_hex(g),32,'0')::uuid),'{}'::uuid[]) FROM generate_series(1,n) g $$`,
				`INSERT INTO transfer_contract
				 (contract_id,source_network_id,source_id,destination_network_id,destination_id,
				 transfer_byte_count,create_time,close_time,outcome)
				 SELECT lpad(to_hex(g),32,'0')::uuid,gen_random_uuid(),gen_random_uuid(),gen_random_uuid(),
				 lpad(to_hex(CASE WHEN g<=4000 THEN g WHEN g<=24000 THEN g-4000
				 WHEN g<=26000 THEN g-24000 ELSE 30000+(g-26001)%20000 END),32,'0')::uuid,
				 7,timestamp '2026-09-01',CASE WHEN g<=4000 THEN timestamp '2026-10-06'
				 WHEN g<=24000 THEN timestamp '2026-09-01' WHEN g<=26000 THEN NULL
				 WHEN g%2=0 THEN timestamp '2026-10-06' ELSE timestamp '2026-09-01' END,
				 CASE WHEN g>24000 AND g<=26000 THEN NULL ELSE 'success' END
				 FROM generate_series(1,66000) g`,
				`INSERT INTO contract_close(contract_id,close_time,party,used_transfer_byte_count)
				 SELECT contract_id,coalesce(close_time,timestamp '2026-10-06'),party,
				 CASE WHEN party='destination' THEN 7 ELSE 999 END
				 FROM transfer_contract CROSS JOIN (VALUES('source'),('destination')) p(party)`,
				`INSERT INTO contract_close(contract_id,close_time,party,used_transfer_byte_count)
				 SELECT lpad(to_hex(90000+g),32,'0')::uuid,timestamp '2026-10-06','destination',555
				 FROM generate_series(1,20000) g`,
				`ANALYZE transfer_contract,contract_close`,
			} {
				server.RaisePgResult(conn.Exec(ctx, sql))
			}
			defer func() {
				_, _ = conn.Exec(context.WithoutCancel(ctx), `DROP FUNCTION pg_temp.provider_transfer_ids(integer)`)
			}()
			var contracts, reports, eligible int64
			server.Raise(conn.QueryRow(ctx, `SELECT (SELECT count(*) FROM transfer_contract),
			 (SELECT count(*) FROM contract_close),(SELECT count(*) FROM transfer_contract
			 WHERE destination_id=ANY(pg_temp.provider_transfer_ids(20000)) AND close_time>=timestamp '2026-10-06')`).Scan(&contracts, &reports, &eligible))
			if contracts != 66000 || reports != 152000 || eligible != 4000 {
				t.Fatalf("population changed: contracts=%d reports=%d eligible=%d", contracts, reports, eligible)
			}
			for _, count := range []int{1, 5, 20000} {
				for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
					for variant, sql := range []string{providerStatsTransferBytesBeforeSQL, providerStatsTransferBytesSQL} {
						func() {
							tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly})
							server.Raise(err)
							defer func() {
								_ = tx.Rollback(context.WithoutCancel(ctx))
								_, _ = conn.Exec(context.WithoutCancel(ctx), `DEALLOCATE provider_transfer_plan`)
							}()
							server.RaisePgResult(tx.Exec(ctx, `SET LOCAL statement_timeout='30s'; SET LOCAL lock_timeout='2s'`))
							server.RaisePgResult(tx.Exec(ctx, `SET LOCAL plan_cache_mode=`+mode))
							server.RaisePgResult(tx.Exec(ctx, `PREPARE provider_transfer_plan(uuid[],timestamp) AS `+sql))
							call := fmt.Sprintf(`EXECUTE provider_transfer_plan(pg_temp.provider_transfer_ids(%d),timestamp '2026-10-06')`, count)
							var raw []byte
							server.Raise(tx.QueryRow(ctx, `EXPLAIN(ANALYZE,BUFFERS,TIMING OFF,FORMAT JSON) `+call, pgx.QueryExecModeExec).Scan(&raw))
							var plans []struct {
								Plan providerTransferTestPlanNode
							}
							server.Raise(json.Unmarshal(raw, &plans))
							if len(plans) != 1 {
								t.Fatal("unexpected provider transfer plan")
							}
							var contractWork, reportWork, reportLoops float64
							point := false
							var inspect func(providerTransferTestPlanNode)
							inspect = func(node providerTransferTestPlanNode) {
								work := node.Loops * (node.Rows + node.Removed + node.Recheck)
								if node.Relation == "transfer_contract" {
									contractWork += work
								}
								if node.Relation == "contract_close" {
									reportWork += work
									reportLoops += node.Loops
									// PostgreSQL deparses varchar equality as (party)::text.
									// Ignore only these type decorations, retaining both
									// equality keys and the exact primary-index identity.
									condition := strings.NewReplacer("(", "", ")", "", "::text", "", "::uuid", "").Replace(node.Cond)
									fullKey := node.Index == "contract_close_pkey" && strings.Contains(condition, "contract_id =") && strings.Contains(condition, "party =")
									if variant == 1 && (!fullKey || node.Rows+node.Removed+node.Recheck > 1) {
										t.Fatalf("%d/%s lost complete report PK lookup: %s", count, mode, raw)
									}
									point = point || fullKey
								}
								for _, child := range node.Plans {
									inspect(child)
								}
							}
							inspect(plans[0].Plan)
							want := min(count, int(eligible))
							if variant == 1 && (!point || contractWork > float64(want)*1.01+1 || reportWork > float64(want)*1.01+1 || reportLoops != float64(want)) {
								t.Fatalf("%d/%s exceeded eligible contracts: contract=%.0f report=%.0f probes=%.0f want=%d plan=%s", count, mode, contractWork, reportWork, reportLoops, want, raw)
							}
							rows, err := tx.Query(ctx, call, pgx.QueryExecModeExec)
							seen, total := 0, int64(0)
							server.WithPgResult(rows, err, func() {
								for rows.Next() {
									var id server.Id
									var amount int64
									server.Raise(rows.Scan(&id, &amount))
									if amount != 7 {
										t.Fatal("dense report total changed")
									}
									seen++
									total += amount
								}
							})
							if seen != want || total != int64(7*want) {
								t.Fatal("dense provider grouping changed")
							}
							t.Logf("providers=%d mode=%s candidate=%t contract_work=%.0f report_work=%.0f report_probes=%.0f shared_hits=%.0f shared_reads=%.0f", count, mode, variant == 1, contractWork, reportWork, reportLoops, plans[0].Plan.Hits, plans[0].Plan.Reads)
						}()
					}
				}
			}
		})
	})
}
