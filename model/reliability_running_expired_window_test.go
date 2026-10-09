package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

type reliabilityExpiredRange struct {
	min, max int64
	degraded []int64
}

// Forward every statement to the real transaction. Record only the history
// ranges consumed by the owning maintenance implementation, so a correct final
// row count cannot conceal an unnecessarily large entering/leaving scan.
type reliabilityExpiredWindowTx struct {
	server.PgTx
	ranges          []reliabilityExpiredRange
	beforeAggregate func()
}

func (self *reliabilityExpiredWindowTx) Exec(ctx context.Context, query string, args ...any) (server.PgTag, error) {
	if strings.Contains(query, "COUNT(*) OVER (PARTITION BY block_number, client_address_hash)") {
		if len(args) != 4 {
			return server.PgTag{}, errors.New("unexpected reliability aggregate argument shape")
		}
		minBlock, minOK := args[0].(int64)
		maxBlock, maxOK := args[1].(int64)
		degraded, degradedOK := args[2].([]int64)
		if !minOK || !maxOK || !degradedOK {
			return server.PgTag{}, errors.New("unexpected reliability aggregate argument types")
		}
		self.ranges = append(self.ranges, reliabilityExpiredRange{
			min: minBlock, max: maxBlock, degraded: append([]int64{}, degraded...),
		})
		if self.beforeAggregate != nil {
			self.beforeAggregate()
		}
	}
	return self.PgTx.Exec(ctx, query, args...)
}

func TestReliabilityRunningExpiredWindowDecision(t *testing.T) {
	previous := reliabilityRunningWindow{
		exists: true, minBlockNumber: 100, maxBlockNumber: 106, lastRecomputeBlock: 106,
		degradedClassificationVersion:           reliabilityDegradedClassificationVersion,
		degradedClassificationWriteTokenPresent: true,
	}
	for _, allowed := range []bool{false, true} {
		for _, test := range []struct {
			name     string
			min, max int64
			rebuild  bool
		}{
			{name: "unchanged", min: 100, max: 106},
			{name: "one_block_overlap", min: 105, max: 111},
			{name: "touching_half_open_boundary", min: 106, max: 112, rebuild: true},
			{name: "gap", min: 107, max: 113, rebuild: true},
			{name: "normal_thirty_minute_cadence", min: 130, max: 136, rebuild: true},
		} {
			recompute, deferred := reliabilityRunningNeedsRecompute(previous, test.min, test.max, allowed)
			if recompute != test.rebuild || deferred {
				t.Errorf("%s maintenance_allowed=%t: rebuild=%t deferred=%t, want rebuild=%t",
					test.name, allowed, recompute, deferred, test.rebuild)
			}
		}
	}
}

func reliabilityExpiredAssertRows(t testing.TB, ctx context.Context, tx server.PgTx, aggregate string, minBlock, maxBlock int64, lookback int, wantRows, wantZero int) {
	t.Helper()
	degraded := reliabilityDegradedBlocks(ctx, tx, minBlock, maxBlock)
	var count, missing, mismatched, zero int
	server.Raise(tx.QueryRow(ctx, `WITH expected AS MATERIALIZED (`+aggregate+`), actual AS MATERIALIZED (
		SELECT network_id,client_id,independent_sum,reliability_sum,observed_row_count
		FROM client_reliability_running WHERE lookback_index=$4
	) SELECT count(*),
		count(*) FILTER(WHERE e.client_id IS NULL OR a.client_id IS NULL),
		count(*) FILTER(WHERE e.network_id IS DISTINCT FROM a.network_id
			OR abs(e.ind-a.independent_sum)>1e-9 OR abs(e.rel-a.reliability_sum)>1e-9
			OR e.observed_row_count IS DISTINCT FROM a.observed_row_count),
		count(*) FILTER(WHERE a.independent_sum=0 AND a.observed_row_count>0)
	FROM expected e FULL JOIN actual a USING(client_id)`,
		minBlock, maxBlock, degraded, lookback).Scan(&count, &missing, &mismatched, &zero))
	if count != wantRows || missing != 0 || mismatched != 0 || zero != wantZero {
		t.Fatalf("current-window result: rows=%d/%d missing=%d mismatched=%d observed_zero=%d/%d",
			count, wantRows, missing, mismatched, zero, wantZero)
	}
}

// Exercise the actual checkpoint's chosen ranges on a loaded cohort. The old
// code produces the same final providers by scanning the intervening gap twice;
// it must fail the work oracle. No planner method is disabled. This is local
// workload evidence, not a Main plan or CPU attribution.
func TestReliabilityRunningExpiredWindowLoadedBound(t *testing.T) {
	if testing.Short() {
		t.Skip("3.06-million-row expired reliability window control")
	}
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
		defer cancel()
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `INSERT INTO client_reliability
				(block_number,client_address_hash,network_id,client_id,
				 connection_established_count,provide_enabled_count,receive_message_count,valid)
				SELECT block,decode(md5('synthetic-address-'||((provider-1)/8)::text),'hex'),
					md5('synthetic-network-'||(provider%101)::text)::uuid,
					md5('synthetic-provider-'||provider::text)::uuid,
					1,1,CASE WHEN provider%17=0 THEN 0 ELSE 1 END,
					client_reliability_valid(0,1,1,0,CASE WHEN provider%17=0 THEN 0 ELSE 1 END,$1)
				FROM generate_series(1,36) block CROSS JOIN generate_series(1,85000) provider`,
				ReliabilityAllowDisconnectCountPerBlock))
			// Independent old-only and gap-only providers must be absent from the
			// final window. Invalid observations in the current window must remain.
			server.RaisePgResult(conn.Exec(ctx, `INSERT INTO client_reliability
				(block_number,client_address_hash,network_id,client_id,
				 connection_established_count,provide_enabled_count,receive_message_count,valid)
				SELECT block,decode(md5('synthetic-expired-address-'||block::text),'hex'),
					md5('synthetic-expired-network')::uuid,md5('synthetic-expired-client-'||block::text)::uuid,
					1,1,1,client_reliability_valid(0,1,1,0,1,$1)
				FROM unnest(ARRAY[1,20]) block`, ReliabilityAllowDisconnectCountPerBlock))
			server.RaisePgResult(conn.Exec(ctx, `INSERT INTO client_reliability_block
				(block_number,client_count,valid_client_count)
				SELECT block,1000,CASE WHEN block=33 THEN 100 ELSE 1000 END
				FROM generate_series(-60,36) block`))
			server.RaisePgResult(conn.Exec(ctx, `CREATE INDEX reliability_expired_covering
				ON client_reliability `+clientReliabilitySecondaryIndexShape))
			server.RaisePgResult(conn.Exec(ctx, `VACUUM (ANALYZE) client_reliability`))
			for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
				for _, form := range []struct {
					name, aggregate      string
					lookback, rows, zero int
				}{
					{name: "client_observed", aggregate: reliabilityRunningObservedAggSql, lookback: 0, rows: 85000, zero: 5000},
					{name: "network_valid", aggregate: `SELECT agg.*,0::bigint AS observed_row_count FROM (` + reliabilityRunningAggSql + `) agg`, lookback: networkWindowLookbackIndex, rows: 80000},
				} {
					func() {
						tx, err := conn.Begin(ctx)
						server.Raise(err)
						defer tx.Rollback(ctx)
						server.RaisePgResult(tx.Exec(ctx, "SET LOCAL plan_cache_mode="+mode))
						server.RaisePgResult(tx.Exec(ctx, "SET LOCAL statement_timeout='45s'"))
						lb := reliabilityRunningLookback{lookbackIndex: form.lookback}
						updateClientReliabilityRunningLookbackAtBoundsInTx(tx, ctx, lb, 1, 7, false)
						recorder := &reliabilityExpiredWindowTx{PgTx: tx}
						started := time.Now()
						updateClientReliabilityRunningLookbackAtBoundsInTx(recorder, ctx, lb, 31, 37, false)
						checkpointMillis := time.Since(started).Seconds() * 1000
						reliabilityExpiredAssertRows(t, ctx, tx, form.aggregate, 31, 37, form.lookback, form.rows, form.zero)
						degraded := reliabilityDegradedBlocks(ctx, tx, 31, 37)
						if len(degraded) != 1 || degraded[0] != 33 {
							t.Fatal("synthetic degraded block was not classified")
						}
						statement := `"expired_range_` + server.NewId().String() + `"`
						server.RaisePgResult(tx.Exec(ctx, `PREPARE `+statement+` (bigint,bigint,bigint[]) AS `+form.aggregate))
						defer tx.Exec(ctx, "DEALLOCATE "+statement)
						examined, buffers, elapsed := 0.0, 0.0, 0.0
						currentOnly := len(recorder.ranges) == 1
						for _, bounds := range recorder.ranges {
							currentOnly = currentOnly && bounds.min == 31 && bounds.max == 37
							blocks := []string{}
							for _, block := range bounds.degraded {
								blocks = append(blocks, fmt.Sprintf("%d", block))
							}
							var raw []byte
							server.Raise(tx.QueryRow(ctx, fmt.Sprintf(
								"EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) EXECUTE %s(%d,%d,'{%s}')",
								statement, bounds.min, bounds.max, strings.Join(blocks, ","))).Scan(&raw))
							var plans []struct {
								Plan          reliabilityObservationPlanNode
								ExecutionTime float64 `json:"Execution Time"`
							}
							server.Raise(json.Unmarshal(raw, &plans))
							if len(plans) != 1 {
								t.Fatal("expired-window control lacks one execution plan")
							}
							examined += reliabilityObservationExamined(plans[0].Plan)
							buffers += plans[0].Plan.Hits + plans[0].Plan.Reads
							elapsed += plans[0].ExecutionTime
						}
						var generic, custom int
						server.Raise(tx.QueryRow(ctx, `SELECT generic_plans,custom_plans FROM pg_prepared_statements WHERE name=$1`,
							strings.Trim(statement, `"`)).Scan(&generic, &custom))
						if (mode == "force_custom_plan" && (custom != len(recorder.ranges) || generic != 0)) ||
							(mode == "force_generic_plan" && (generic != len(recorder.ranges) || custom != 0)) {
							t.Fatal("prepared plan regime was not observed")
						}
						t.Logf("expired_running mode=%s form=%s parity=true rows=%d ranges=%d current_only=%t examined=%.0f buffers=%.0f aggregate_ms=%.3f checkpoint_ms=%.3f",
							mode, form.name, form.rows, len(recorder.ranges), currentOnly, examined, buffers, elapsed, checkpointMillis)
						if !currentOnly || examined > 510004 {
							t.Fatal("expired-window checkpoint scanned history outside its current six-block window")
						}
						window := readReliabilityRunningWindow(ctx, tx, form.lookback)
						if window.minBlockNumber != 31 || window.maxBlockNumber != 37 || window.lastRecomputeBlock != 37 ||
							!reliabilityRunningObservationCurrent(window) || !window.degradedClassificationWriteTokenPresent {
							t.Fatal("expired-window rebuild did not publish a complete guarded checkpoint")
						}
						updateClientReliabilityRunningLookbackAtBoundsInTx(tx, ctx, lb, 60, 66, false)
						reliabilityExpiredAssertRows(t, ctx, tx, form.aggregate, 60, 66, form.lookback, 0, 0)
					}()
				}
			}
		}, server.OptNoRetry())
	})
}

func TestReliabilityRunningExpiredWindowCancellationKeepsCheckpoint(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		lb := reliabilityRunningLookback{lookbackIndex: 0}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO client_reliability
				(block_number,client_address_hash,network_id,client_id,
				 connection_established_count,provide_enabled_count,receive_message_count,valid)
				SELECT block,decode(md5('synthetic-cancel-address'),'hex'),
					md5('synthetic-cancel-network')::uuid,md5('synthetic-cancel-client')::uuid,
					1,1,1,client_reliability_valid(0,1,1,0,1,$1)
				FROM generate_series(1,36) block`, ReliabilityAllowDisconnectCountPerBlock))
			updateClientReliabilityRunningLookbackAtBoundsInTx(tx, ctx, lb, 1, 7, false)
		})
		previousToken := testingReadRunningWindowWriteToken(ctx, 0)
		cancelCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		canceledAtAggregate, failed := false, false
		func() {
			defer func() { failed = recover() != nil }()
			server.MaintenanceTx(cancelCtx, func(tx server.PgTx) {
				recorder := &reliabilityExpiredWindowTx{PgTx: tx, beforeAggregate: func() {
					// DELETE has executed; cancellation of the following aggregate
					// must roll the whole checkpoint back before the safe retry.
					canceledAtAggregate = true
					cancel()
				}}
				updateClientReliabilityRunningLookbackAtBoundsInTx(recorder, cancelCtx, lb, 31, 37, false)
			}, server.OptNoRetry(), server.TxReadCommitted)
		}()
		if !failed || !canceledAtAggregate || cancelCtx.Err() != context.Canceled {
			t.Fatal("cancellation did not cross the actual checkpoint aggregate boundary")
		}
		previous := testingReadRunningWindow(ctx, 0)
		if previous.minBlockNumber != 1 || previous.maxBlockNumber != 7 || testingReadRunningWindowWriteToken(ctx, 0) != previousToken {
			t.Fatal("canceled rebuild replaced the previous guarded checkpoint")
		}
		server.Tx(ctx, func(tx server.PgTx) {
			reliabilityExpiredAssertRows(t, ctx, tx, reliabilityRunningObservedAggSql, 1, 7, 0, 1, 0)
			updateClientReliabilityRunningLookbackAtBoundsInTx(tx, ctx, lb, 31, 37, false)
			reliabilityExpiredAssertRows(t, ctx, tx, reliabilityRunningObservedAggSql, 31, 37, 0, 1, 0)
		})
	})
}
