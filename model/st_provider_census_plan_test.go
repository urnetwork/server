// Native controls retain the original query as a causal comparison while
// asserting unchanged immutable usage, complete windows and financial totals.
package model

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urfoundation/sn/payoutartifact"
	"github.com/urnetwork/server"
)

// Exact pre-fix optional-window query from d7b6810df; the shared immutable
// usage selection is unchanged. This control is never used by production.
const stOriginalUsageUnboundedWindowTestSql = `
 WITH usage_rows AS MATERIALIZED (` + stEpochProviderUsageSql + `),
 window_rows AS MATERIALIZED (
  SELECT * FROM (
   SELECT contract_id, 'credited'::text AS disposition, close_time, provider_usage FROM usage_rows
   UNION ALL
   SELECT contract_id, CASE WHEN outcome IS NULL THEN 'open' WHEN close_time IS NULL THEN 'unassigned_canceled' ELSE 'canceled' END,
     close_time, NULL::jsonb FROM transfer_contract
   WHERE (outcome IS NULL AND create_time < $2)
      OR (outcome='canceled' AND (close_time IS NULL OR ($1<=close_time AND close_time<$2)))
  ) AS all_rows ORDER BY contract_id LIMIT 32769
 ), window_body AS MATERIALIZED (
  SELECT CASE WHEN count(*)<=32768 AND COALESCE(sum(512+octet_length(COALESCE(provider_usage::text,''))),0)<=524288
   THEN jsonb_build_object('schema','urnetwork-closed-work-window-inventory-v1',
    'start',to_char($1::timestamp,'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
    'end',to_char($2::timestamp,'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
    'records',COALESCE(jsonb_agg(jsonb_build_object('contract_id',contract_id::text,'disposition',disposition,
       'closed_at',to_char(close_time,'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
       'original_snapshot',CASE WHEN provider_usage IS NULL THEN NULL ELSE encode(convert_to(provider_usage::text,'UTF8'),'base64') END)
      ORDER BY contract_id),'[]'::jsonb)) ELSE NULL END AS body FROM window_rows
 )
 SELECT usage.*, CASE WHEN octet_length(combined.body)<=1048576 THEN combined.body ELSE originals.body END, false AS window_only
 FROM usage_rows AS usage CROSS JOIN window_body
 LEFT JOIN LATERAL (
  WITH selected AS MATERIALIZED (
   SELECT client_id,report_id,party,acked_byte_count,unacked_byte_count,checkpoint,original_report,original_key_registration,original_key_issue,original_inventory
   FROM contract_close_report_evidence WHERE contract_id=usage.contract_id
   ORDER BY client_id,report_id LIMIT 1025
  )
  SELECT CASE WHEN (SELECT count(*) FROM selected) BETWEEN 1 AND 1024
    AND (SELECT COALESCE(sum(512+COALESCE(octet_length(original_report),0)+COALESCE(octet_length(original_key_registration),0)+COALESCE(octet_length(original_inventory),0)),0) FROM selected) <= 524288
   THEN (SELECT convert_to(jsonb_build_object('schema',CASE WHEN bool_or(original_inventory IS NOT NULL) THEN 'urnetwork-original-close-report-census-v2' ELSE 'urnetwork-original-close-report-census-v1' END,'count',count(*),
    'reports',jsonb_agg(jsonb_build_object(
     'client_id',client_id::text,'report_id',report_id::text,'party',party,
     'acked_bytes',acked_byte_count,'unacked_bytes',unacked_byte_count,'checkpoint',checkpoint,
     'original',encode(original_report,'base64'),'key_registration',encode(original_key_registration,'base64'),'key_issue',original_key_issue
    ) || CASE WHEN original_inventory IS NULL THEN '{}'::jsonb ELSE jsonb_build_object('inventory',encode(original_inventory,'base64')) END ORDER BY client_id,report_id))::text,'UTF8') FROM selected)
   ELSE NULL END AS body
 ) AS originals ON true
 LEFT JOIN LATERAL (
  SELECT CASE WHEN usage.contract_id=(SELECT min(contract_id::text)::uuid FROM usage_rows) AND window_body.body IS NOT NULL
   THEN convert_to((COALESCE(convert_from(originals.body,'UTF8')::jsonb,jsonb_build_object('count',0,'reports','[]'::jsonb))
      || jsonb_build_object('schema','urnetwork-original-close-report-census-v2','window',window_body.body))::text,'UTF8')
   ELSE originals.body END AS body
 ) AS combined ON true
 UNION ALL
 SELECT '00000000-0000-0000-0000-000000000000'::uuid,NULL::jsonb,NULL::timestamp,false,
   convert_to(window_body.body::text,'UTF8'),true FROM window_body
`

type stCensusPlanTestRow struct {
	Usage, Reports []byte
	ClosedAt       *time.Time
	Duplicate      bool
	WindowOnly     bool
}

// Preserve the exact jsonb and bytea outputs, including the independent empty
// window sentinel. PostgreSQL may stream the same owners in a different order.
func stCensusPlanTestRows(t testing.TB, ctx context.Context, conn server.PgConn, query string, start, end time.Time) map[server.Id]stCensusPlanTestRow {
	t.Helper()
	result := map[server.Id]stCensusPlanTestRow{}
	rows, err := conn.Query(ctx, query, start, end)
	server.Raise(err)
	defer rows.Close()
	for rows.Next() {
		var id server.Id
		var row stCensusPlanTestRow
		server.Raise(rows.Scan(&id, &row.Usage, &row.ClosedAt, &row.Duplicate, &row.Reports, &row.WindowOnly))
		if _, exists := result[id]; exists {
			t.Fatal("census control has duplicate output identity", id)
		}
		row.Usage, row.Reports = bytes.Clone(row.Usage), bytes.Clone(row.Reports)
		result[id] = row
	}
	server.Raise(rows.Err())
	return result
}

type stCensusPlanTestWork struct {
	ContractRows, WindowRows, JsonAggregations float64
}

// Count visible executor work, including rejected rows and repeated scans.
// Worker detail is already represented by Actual Rows times Actual Loops.
func stCensusPlanTestCounters(t testing.TB, value any) (work stCensusPlanTestWork) {
	t.Helper()
	node, ok := value.(map[string]any)
	if !ok {
		t.Fatal("census plan lacks an executor node")
	}
	actual, _ := node["Actual Rows"].(float64)
	loops, _ := node["Actual Loops"].(float64)
	filtered, _ := node["Rows Removed by Filter"].(float64)
	rechecked, _ := node["Rows Removed by Index Recheck"].(float64)
	if node["Relation Name"] == "transfer_contract" {
		work.ContractRows += (actual + filtered + rechecked) * loops
	}
	if node["CTE Name"] == "window_rows" {
		work.WindowRows += (actual + filtered) * loops
	}
	if node["Node Type"] == "Aggregate" {
		if outputs, ok := node["Output"].([]any); ok {
			for _, output := range outputs {
				if text, ok := output.(string); ok && strings.Contains(text, "jsonb_agg") {
					work.JsonAggregations += loops
					break
				}
			}
		}
	}
	if children, ok := node["Plans"].([]any); ok {
		for _, child := range children {
			childWork := stCensusPlanTestCounters(t, child)
			work.ContractRows += childWork.ContractRows
			work.WindowRows += childWork.WindowRows
			work.JsonAggregations += childWork.JsonAggregations
		}
	}
	return
}

// The native plan itself supplies work counters; elapsed time is diagnostic.
func stCensusPlanTestExplain(t testing.TB, ctx context.Context, conn server.PgConn, query string, start, end time.Time) stCensusPlanTestWork {
	t.Helper()
	var raw []byte
	server.Raise(conn.QueryRow(ctx, `EXPLAIN (ANALYZE, BUFFERS, VERBOSE, TIMING OFF, FORMAT JSON) `+query, start, end).Scan(&raw))
	var documents []map[string]any
	server.Raise(json.Unmarshal(raw, &documents))
	if len(documents) != 1 {
		t.Fatal("census control did not return one plan")
	}
	work := stCensusPlanTestCounters(t, documents[0]["Plan"])
	t.Logf("current=%t work=%+v plan=%s", query == stEpochProviderOriginalUsageSql, work, raw)
	return work
}

// Large optional history must not be scanned and sorted after its byte
// allowance is already disproved. Credited usage remains complete and exact.
func TestStClosedWorkWindowBoundedOverflowPlan(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
		defer cancel()
		start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
		end := start.Add(time.Hour)
		const credited, open, canceled = 128, 32768, 262144
		provider, network := server.NewId(), server.NewId()
		snapshot := &contractUsageSnapshot{Version: 1, ByteCount: 11, Providers: []contractProviderUsage{{ClientId: provider, NetworkId: network, ByteCount: 11}}}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
				(contract_id,source_id,source_network_id,destination_id,destination_network_id,
				 transfer_byte_count,create_time,outcome,close_time,provider_usage,usage_origin_is_source)
				SELECT md5('synthetic-census-work-'||n)::uuid,$5,$5,$6,$6,1000000,$1::timestamp-interval '1 hour',
				 CASE WHEN n<=$2 THEN 'settled' WHEN n<=$2+$3 THEN NULL ELSE 'canceled' END,
				 CASE WHEN n<=$2 THEN $1::timestamp WHEN n<=$2+$3 OR n%2=0 THEN NULL ELSE $1::timestamp-interval '1 day' END,
				 CASE WHEN n<=$2 THEN $7::jsonb ELSE NULL END,true
				FROM generate_series(1,$2::int+$3::int+$4::int) n`, start, credited, open, canceled, provider, network, snapshot))
			server.RaisePgResult(tx.Exec(ctx, `ANALYZE transfer_contract; ANALYZE st_provider_usage_archive; ANALYZE contract_close_report_evidence`))
		})
		server.Db(ctx, func(conn server.PgConn) {
			before := stCensusPlanTestRows(t, ctx, conn, stOriginalUsageUnboundedWindowTestSql, start, end)
			after := stCensusPlanTestRows(t, ctx, conn, stEpochProviderOriginalUsageSql, start, end)
			if len(before) != credited+1 || !reflect.DeepEqual(before, after) || len(after[server.Id{}].Reports) != 0 {
				t.Fatal("overflow changed original usage or published a partial window")
			}
			oldWork := stCensusPlanTestExplain(t, ctx, conn, stOriginalUsageUnboundedWindowTestSql, start, end)
			newWork := stCensusPlanTestExplain(t, ctx, conn, stEpochProviderOriginalUsageSql, start, end)
			if oldWork.WindowRows < 32769 || oldWork.ContractRows < canceled || oldWork.JsonAggregations < 1 {
				t.Fatal("control did not exercise the original history scan and discarded JSON construction", oldWork)
			}
			if newWork.WindowRows != 1025 || newWork.ContractRows > 8192 || newWork.JsonAggregations != 0 {
				t.Fatal("overflow still scanned history or encoded an unavailable window", newWork)
			}
		})
		usages, census, window, err := GetStEpochProviderUsageWholeCensus(ctx, 17, start, end)
		if err != nil || len(usages) != 1 || usages[0].ClientId != provider || usages[0].NetworkId != network || usages[0].PayoutByteCount != credited*11 || census == nil || census.Count != credited || len(census.Records) != credited || window != nil {
			t.Fatal("optional overflow changed complete immutable provider earnings", usages, census, window, err)
		}
	})
}

// At exactly 512 KiB every zero-credit original fits. One more row must omit
// the whole window, while start/end filters and canonical ordering stay exact.
func TestStClosedWorkWindowExactCapacityAndOrder(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
		end := start.Add(time.Hour)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
				(contract_id,source_id,source_network_id,destination_id,destination_network_id,transfer_byte_count,create_time,outcome,close_time)
				SELECT md5('synthetic-census-capacity-'||n)::uuid,$2,$2,$3,$3,0,$1::timestamp-interval '1 hour',
				 CASE WHEN n%3=0 THEN NULL ELSE 'canceled' END,
				 CASE WHEN n%3=1 THEN $1::timestamp ELSE NULL END FROM generate_series(1,1024) n`, start, server.NewId(), server.NewId()))
			// Neither an open identity created at end nor canceled rows outside
			// the half-open interval belong to the complete window.
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
				(contract_id,source_id,source_network_id,destination_id,destination_network_id,transfer_byte_count,create_time,outcome,close_time)
				VALUES ($1,$1,$1,$1,$1,0,$4,NULL,NULL),
				($2,$2,$2,$2,$2,0,$4,'canceled',$4),
				($3,$3,$3,$3,$3,0,$4,'canceled',$5::timestamp-interval '1 microsecond')`, server.NewId(), server.NewId(), server.NewId(), end, start))
		})
		check := func(want int) {
			server.Db(ctx, func(conn server.PgConn) {
				before := stCensusPlanTestRows(t, ctx, conn, stOriginalUsageUnboundedWindowTestSql, start, end)
				after := stCensusPlanTestRows(t, ctx, conn, stEpochProviderOriginalUsageSql, start, end)
				if !reflect.DeepEqual(before, after) {
					t.Fatal("capacity or canonical original bytes differ")
				}
				var window *payoutartifact.ClosedWorkWindow
				raw := after[server.Id{}].Reports
				if want == 0 {
					if len(raw) != 0 {
						t.Fatal("overflow published a complete prefix")
					}
					return
				}
				server.Raise(json.Unmarshal(raw, &window))
				if window == nil || len(window.Records) != want {
					t.Fatal("exact capacity dropped complete originals", window)
				}
			})
		}
		check(1024)
		server.Tx(ctx, func(tx server.PgTx) {
			id := server.NewId()
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
				(contract_id,source_id,source_network_id,destination_id,destination_network_id,transfer_byte_count,create_time)
				VALUES($1,$1,$1,$1,$1,0,$2)`, id, start))
		})
		check(0)
	})
}

// One large valid usage snapshot exceeds the byte budget without exceeding
// the row budget. It must retain its raw usage and skip discarded JSON work.
func TestStClosedWorkWindowByteOverflowSkipsEncoding(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
		snapshot := &contractUsageSnapshot{Version: 1, ByteCount: 8192}
		for range 8192 {
			snapshot.Providers = append(snapshot.Providers, contractProviderUsage{ClientId: server.NewId(), NetworkId: server.NewId(), ByteCount: 1})
		}
		slices.SortFunc(snapshot.Providers, func(a, b contractProviderUsage) int { return a.ClientId.Cmp(b.ClientId) })
		id := addStContractUsageSnapshotTestRow(t, ctx, start, snapshot)
		server.Db(ctx, func(conn server.PgConn) {
			before := stCensusPlanTestRows(t, ctx, conn, stOriginalUsageUnboundedWindowTestSql, start, start.Add(time.Hour))
			after := stCensusPlanTestRows(t, ctx, conn, stEpochProviderOriginalUsageSql, start, start.Add(time.Hour))
			if len(after[id].Usage) <= 524288 || len(after[server.Id{}].Reports) != 0 || !reflect.DeepEqual(before, after) {
				t.Fatal("single-original byte overflow changed retained usage")
			}
			oldWork := stCensusPlanTestExplain(t, ctx, conn, stOriginalUsageUnboundedWindowTestSql, start, start.Add(time.Hour))
			newWork := stCensusPlanTestExplain(t, ctx, conn, stEpochProviderOriginalUsageSql, start, start.Add(time.Hour))
			if oldWork.JsonAggregations < 1 || newWork.JsonAggregations != 0 {
				t.Fatal("byte overflow still encoded the discarded window", oldWork, newWork)
			}
		})
		usages, err := GetStEpochProviderUsage(ctx, start, start.Add(time.Hour))
		if err != nil || len(usages) != 8192 {
			t.Fatal("byte capacity suppressed provider earnings", len(usages), err)
		}
		for _, usage := range usages {
			if usage.PayoutByteCount != 1 {
				t.Fatal("large original changed a provider's exact earning", usage)
			}
		}
	})
}
