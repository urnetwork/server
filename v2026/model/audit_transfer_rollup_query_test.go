package model

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server/v2026"
)

const transferAuditDailyBytesBeforeSQL = `
SELECT COALESCE(SUM(contract_close.used_transfer_byte_count), 0)
FROM transfer_contract
INNER JOIN contract_close ON
    contract_close.contract_id = transfer_contract.contract_id AND
    contract_close.party = 'destination'
WHERE
    $1 <= transfer_contract.close_time AND
    transfer_contract.close_time < $2
`

//go:embed testdata/audit_transfer_rollup_work.sql
var transferAuditWorkFixtureSQL string

// Exercise the actual daily replacement, including closed/NULL outcomes, late
// reports, canceled outcomes, both day edges and an independent report clock.
func TestRollupTransferAuditDailyReportBoundaries(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		dayEnd := startOfUtcDay(server.NowUtc())
		dayStart := dayEnd.Add(-24 * time.Hour)
		nullOutcomeID, lateReportID := server.NewId(), server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			for _, row := range []struct {
				id      server.Id
				closed  any
				outcome any
				report  bool
				bytes   int64
			}{
				{server.NewId(), dayStart.Add(-time.Microsecond), "success", true, 19},
				{nullOutcomeID, dayStart, nil, true, 11},
				{server.NewId(), dayStart.Add(time.Hour), "canceled", true, 13},
				{server.NewId(), dayEnd.Add(-time.Microsecond), "success", true, 17},
				{server.NewId(), dayEnd, "success", true, 23},
				{server.NewId(), nil, nil, true, 29},
				{lateReportID, dayStart.Add(2 * time.Hour), "success", false, 7},
				{server.NewId(), dayStart.Add(3 * time.Hour), "success", true, 0},
			} {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
					(contract_id,source_network_id,source_id,destination_network_id,destination_id,
					transfer_byte_count,create_time,close_time,outcome)
					VALUES($1,$2,$3,$4,$5,1000,$6,$7,$8)`, row.id,
					server.NewId(), server.NewId(), server.NewId(), server.NewId(),
					dayStart.Add(-24*time.Hour), row.closed, row.outcome))
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close
					(contract_id,close_time,party,used_transfer_byte_count)
					VALUES($1,$2,'source',999)`, row.id, dayStart.Add(-7*24*time.Hour)))
				if row.report {
					server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close
						(contract_id,close_time,party,used_transfer_byte_count)
						VALUES($1,$2,'destination',$3)`, row.id, dayStart.Add(-7*24*time.Hour), row.bytes))
				}
			}
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close
				(contract_id,close_time,party,used_transfer_byte_count)
				VALUES($1,$2,'destination',77)`, server.NewId(), dayStart))
		})
		assertDays := func(wantYesterday int64) {
			if days := RollupTransferAuditEvents(ctx, dayStart.Add(-24*time.Hour), dayEnd); days != 2 {
				t.Fatalf("rollup days=%d, want 2", days)
			}
			server.Db(ctx, func(conn server.PgConn) {
				amounts := map[string]int64{}
				rows, err := conn.Query(ctx, `SELECT event_time,transfer_byte_count,transfer_packets
					FROM audit_contract_event WHERE event_details=$1`, AuditEventDetailsTransferRollup)
				server.WithPgResult(rows, err, func() {
					for rows.Next() {
						var at time.Time
						var amount, packets int64
						server.Raise(rows.Scan(&at, &amount, &packets))
						key := at.UTC().Format("2006-01-02")
						if _, exists := amounts[key]; exists || packets != 0 || !at.Equal(startOfUtcDay(at).Add(12*time.Hour)) {
							t.Fatal("daily replacement duplicated a day or changed its noon/packet contract")
						}
						amounts[key] = amount
					}
				})
				if len(amounts) != 2 || amounts[dayStart.Add(-24*time.Hour).Format("2006-01-02")] != 19 || amounts[dayStart.Format("2006-01-02")] != wantYesterday {
					t.Fatalf("daily report totals changed: %v", amounts)
				}
			})
		}
		assertDays(41)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE contract_close SET used_transfer_byte_count=31
				WHERE contract_id=$1 AND party='destination'`, nullOutcomeID))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close
				(contract_id,close_time,party,used_transfer_byte_count)
				VALUES($1,$2,'destination',7)`, lateReportID, dayStart.Add(-7*24*time.Hour)))
		})
		assertDays(68)
		assertDays(68)
	})
}

type transferAuditWorkNode struct {
	NodeType  string                  `json:"Node Type"`
	Relation  string                  `json:"Relation Name"`
	Index     string                  `json:"Index Name"`
	Rows      float64                 `json:"Actual Rows"`
	Loops     float64                 `json:"Actual Loops"`
	Removed   float64                 `json:"Rows Removed by Filter"`
	Recheck   float64                 `json:"Rows Removed by Index Recheck"`
	Hits      float64                 `json:"Shared Hit Blocks"`
	Reads     float64                 `json:"Shared Read Blocks"`
	LocalHit  float64                 `json:"Local Hit Blocks"`
	LocalRead float64                 `json:"Local Read Blocks"`
	TempRead  float64                 `json:"Temp Read Blocks"`
	TempWrite float64                 `json:"Temp Written Blocks"`
	Plans     []transferAuditWorkNode `json:"Plans"`
}

type transferAuditWork struct {
	contractUpper, reportUpper, bitmapEntries, tempBlocks float64
	contractRepeatedLoops                                 bool
	contracts, reports, buffers                           float64
	bitmapOr                                              bool
	indexes                                               map[string]bool
}

func explainTransferAuditWork(t testing.TB, ctx context.Context, conn server.PgConn, query, mode, start, end string, want int64) transferAuditWork {
	t.Helper()
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly})
	server.Raise(err)
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(cleanup)
		_, _ = conn.Exec(cleanup, `DEALLOCATE audit_rollup_work_plan`)
	}()
	server.RaisePgResult(tx.Exec(ctx, `SET LOCAL statement_timeout='30s'; SET LOCAL lock_timeout='2s';
		SET LOCAL max_parallel_workers_per_gather=0; SET LOCAL work_mem='16MB'; SET LOCAL jit=off`))
	server.RaisePgResult(tx.Exec(ctx, `SET LOCAL plan_cache_mode=`+mode))
	server.RaisePgResult(tx.Exec(ctx, `PREPARE audit_rollup_work_plan(timestamp,timestamp) AS `+query))
	call := fmt.Sprintf(`EXECUTE audit_rollup_work_plan(timestamp '%s',timestamp '%s')`, start, end)
	var raw []byte
	server.Raise(tx.QueryRow(ctx, `EXPLAIN(ANALYZE,BUFFERS,TIMING OFF,FORMAT JSON) `+call, pgx.QueryExecModeExec).Scan(&raw))
	var plans []struct{ Plan transferAuditWorkNode }
	server.Raise(json.Unmarshal(raw, &plans))
	if len(plans) != 1 {
		t.Fatal("unexpected daily rollup plan count")
	}
	root := plans[0].Plan
	work := transferAuditWork{buffers: root.Hits + root.Reads + root.LocalHit + root.LocalRead,
		tempBlocks: root.TempRead + root.TempWrite, indexes: map[string]bool{}}
	var inspect func(transferAuditWorkNode)
	inspect = func(node transferAuditWorkNode) {
		visited := node.Loops * (node.Rows + node.Removed + node.Recheck)
		upper := visited
		if node.Loops > 1 {
			// EXPLAIN rounds each per-loop row/filter/recheck average.
			upper += 1.5 * node.Loops
		}
		if node.Relation == "transfer_contract" {
			work.contracts += visited
			work.contractUpper += upper
			work.contractRepeatedLoops = work.contractRepeatedLoops || node.Loops > 1
		}
		if node.Relation == "contract_close" {
			work.reports += visited
			work.reportUpper += upper
		}
		if node.NodeType == "Bitmap Index Scan" && strings.HasPrefix(node.Index, "transfer_contract") {
			work.bitmapEntries += node.Rows * node.Loops
			if node.Loops > 1 {
				work.bitmapEntries += 0.5 * node.Loops
			}
		}
		work.bitmapOr = work.bitmapOr || node.NodeType == "BitmapOr"
		if node.Index != "" && node.Loops > 0 {
			work.indexes[node.Index] = true
		}
		for _, child := range node.Plans {
			inspect(child)
		}
	}
	inspect(root)
	var amount int64
	server.Raise(tx.QueryRow(ctx, call, pgx.QueryExecModeExec).Scan(&amount))
	if amount != want {
		t.Fatalf("%s daily total=%d, want %d", mode, amount, want)
	}
	return work
}

// Assert work, not elapsed time. The native companion experiment also covers
// JIT on/off and a concurrent bounded writer; those timings are not a Main SLA.
// Temporary tables model the exact relevant partial indexes. The API test
// above independently exercises the real migrated tables and daily writes.
func TestRollupTransferAuditPartialIndexWork(t *testing.T) {
	if testing.Short() {
		t.Skip("daily rollup query-work population")
	}
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
		defer cancel()
		server.Db(ctx, func(conn server.PgConn) {
			defer func() {
				cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				defer cancel()
				_, _ = conn.Exec(cleanup, `DROP TABLE IF EXISTS pg_temp.contract_close,pg_temp.transfer_contract;
					RESET search_path; RESET statement_timeout`)
			}()
			server.RaisePgResult(conn.Exec(ctx, transferAuditWorkFixtureSQL))
			var contracts, reports, nullOutcomes int64
			server.Raise(conn.QueryRow(ctx, `SELECT (SELECT count(*) FROM transfer_contract),
				(SELECT count(*) FROM contract_close),(SELECT count(*) FROM transfer_contract WHERE outcome IS NULL)`).Scan(&contracts, &reports, &nullOutcomes))
			if contracts != 660008 || reports != 1520014 || nullOutcomes != 40002 {
				t.Fatalf("work population changed: contracts=%d reports=%d null_outcomes=%d", contracts, reports, nullOutcomes)
			}
			for _, window := range []struct {
				name, start, end string
				want             int64
			}{
				{"narrow", "2026-10-06", "2026-10-07", 280716},
				{"dense", "2026-09-01", "2026-09-02", 4059300},
			} {
				for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
					before := explainTransferAuditWork(t, ctx, conn, transferAuditDailyBytesBeforeSQL, mode, window.start, window.end, window.want)
					after := explainTransferAuditWork(t, ctx, conn, transferAuditDailyBytesSQL, mode, window.start, window.end, window.want)
					if window.name == "narrow" && mode == "force_custom_plan" {
						if before.contracts < 600000 || !after.bitmapOr || !after.indexes["transfer_contract_closed_usage"] || !after.indexes["transfer_contract_outcome_null"] || after.contracts > before.contracts/4 || after.buffers > before.buffers*0.6 {
							t.Fatalf("selective day lost partial-index work reduction: before=%+v after=%+v", before, after)
						}
					}
					if after.contracts > before.contracts+1 || after.reports > before.reports+1 || after.buffers > before.buffers*1.05+16 {
						t.Fatalf("%s/%s amplified contract/report work: before=%+v after=%+v", window.name, mode, before, after)
					}
					t.Logf("window=%s mode=%s contract_work=%.0f->%.0f report_work=%.0f->%.0f buffers=%.0f->%.0f", window.name, mode, before.contracts, after.contracts, before.reports, after.reports, before.buffers, after.buffers)
				}
			}
		})
	})
}
