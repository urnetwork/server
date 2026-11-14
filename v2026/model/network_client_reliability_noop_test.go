package model

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

func TestReliabilityCommittedBoundsDoNotRepeatRunningMaintenance(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		server.Tx(ctx, func(tx server.PgTx) {
			const minBlock, maxBlock int64 = 29850000, 29850060
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO client_reliability_running(client_id,lookback_index,network_id,independent_sum,reliability_sum,observed_row_count)
    SELECT md5(i::text)::uuid,(i%4)::int,md5((i/10)::text)::uuid,10,10,10 FROM generate_series(1,100000)i`))
			writeReliabilityRunningWindow(ctx, tx, 0, minBlock, maxBlock, maxBlock)
			var plan []byte
			server.Raise(tx.QueryRow(ctx, `EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) DELETE FROM client_reliability_running WHERE lookback_index=0 AND observed_row_count=0`).Scan(&plan))
			var parsed []map[string]any
			server.Raise(json.Unmarshal(plan, &parsed))
			t.Logf("unchanged-window prune over100000 stored rows local execution_ms=%v planning_ms=%v", parsed[0]["Execution Time"], parsed[0]["Planning Time"])
			server.RaisePgResult(tx.Exec(ctx, `CREATE TABLE reliability_noop_writes(n int NOT NULL);INSERT INTO reliability_noop_writes VALUES(0);
    CREATE FUNCTION reliability_noop_record_write() RETURNS trigger LANGUAGE plpgsql AS $f$ BEGIN UPDATE reliability_noop_writes SET n=n+1; RETURN NULL; END $f$;
    CREATE TRIGGER reliability_noop_write AFTER INSERT OR UPDATE OR DELETE ON client_reliability_running FOR EACH STATEMENT EXECUTE FUNCTION reliability_noop_record_write()`))
			var beforeToken, afterToken string
			server.Raise(tx.QueryRow(ctx, `SELECT degraded_classification_write_token::text FROM client_reliability_running_window WHERE lookback_index=0`).Scan(&beforeToken))
			updateClientReliabilityRunningLookbackAtBoundsInTx(tx, ctx, reliabilityRunningLookback{lookbackIndex: 0, lookback: time.Hour}, minBlock, maxBlock, true)
			var writes int
			server.Raise(tx.QueryRow(ctx, `SELECT n FROM reliability_noop_writes`).Scan(&writes))
			server.Raise(tx.QueryRow(ctx, `SELECT degraded_classification_write_token::text FROM client_reliability_running_window WHERE lookback_index=0`).Scan(&afterToken))
			if writes != 0 || beforeToken != afterToken {
				t.Fatalf("committed unchanged bounds repeated running maintenance: statement_trigger_events=%d token_changed=%t", writes, beforeToken != afterToken)
			}

			// Equal bounds do not excuse an old writer's invalidated generation.
			for _, column := range []string{"observation_version", "degraded_classification_version"} {
				server.RaisePgResult(tx.Exec(ctx, "UPDATE client_reliability_running_window SET "+column+"=0, degraded_classification_write_token=gen_random_uuid(), observation_write_token=gen_random_uuid() WHERE lookback_index=0"))
				server.RaisePgResult(tx.Exec(ctx, "UPDATE reliability_noop_writes SET n=0"))
				updateClientReliabilityRunningLookbackAtBoundsInTx(tx, ctx, reliabilityRunningLookback{lookbackIndex: 0, lookback: time.Hour}, minBlock, maxBlock, false)
				server.Raise(tx.QueryRow(ctx, "SELECT n FROM reliability_noop_writes").Scan(&writes))
				window := readReliabilityRunningWindow(ctx, tx, 0)
				if writes == 0 || !reliabilityRunningObservationCurrent(window) || window.degradedClassificationVersion != reliabilityDegradedClassificationVersion {
					t.Fatal("equal bounds bypassed required generation repair")
				}
			}

			// A quiet periodic re-anchor and an advancing window keep their work.
			server.RaisePgResult(tx.Exec(ctx, "UPDATE client_reliability_running_window SET last_recompute_block=$1, degraded_classification_write_token=gen_random_uuid(), observation_write_token=gen_random_uuid() WHERE lookback_index=0", maxBlock-ReliabilityRunningRecomputeBlocks))
			server.RaisePgResult(tx.Exec(ctx, "UPDATE reliability_noop_writes SET n=0"))
			updateClientReliabilityRunningLookbackAtBoundsInTx(tx, ctx, reliabilityRunningLookback{lookbackIndex: 0, lookback: time.Hour}, minBlock, maxBlock, true)
			server.Raise(tx.QueryRow(ctx, "SELECT n FROM reliability_noop_writes").Scan(&writes))
			if writes == 0 || readReliabilityRunningWindow(ctx, tx, 0).lastRecomputeBlock != maxBlock {
				t.Fatal("equal bounds bypassed scheduled quiet re-anchor")
			}
			server.RaisePgResult(tx.Exec(ctx, "UPDATE reliability_noop_writes SET n=0"))
			updateClientReliabilityRunningLookbackAtBoundsInTx(tx, ctx, reliabilityRunningLookback{lookbackIndex: 0, lookback: time.Hour}, minBlock+1, maxBlock+1, true)
			server.Raise(tx.QueryRow(ctx, "SELECT n FROM reliability_noop_writes").Scan(&writes))
			if writes == 0 || readReliabilityRunningWindow(ctx, tx, 0).maxBlockNumber != maxBlock+1 {
				t.Fatal("advancing window lost delta maintenance")
			}
		})
	})
}

// Out-of-band/backfill writes can violate the normal drained-block finality
// contract. Equal bounds already exclude those rows from the entering/leaving
// delta. Preserve that baseline boundary, and prove mandatory invalidation
// still repairs their observed evidence before the no-op decision.
func TestReliabilitySameBoundsLateCommitStillRepairsInvalidatedEvidence(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		const minBlock, maxBlock int64 = 29850000, 29850060
		lb := reliabilityRunningLookback{lookbackIndex: 0, lookback: time.Hour}
		client, network := server.NewId(), server.NewId()
		server.Tx(ctx, func(tx server.PgTx) { writeReliabilityRunningWindow(ctx, tx, 0, minBlock, maxBlock, maxBlock) })
		// This row commits after the window checkpoint, at an already covered block.
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO client_reliability(block_number,client_address_hash,network_id,client_id,valid) VALUES($1,decode(repeat('01',32),'hex'),$2,$3,false)`, minBlock+1, network, client))
		})
		server.Tx(ctx, func(tx server.PgTx) {
			updateClientReliabilityRunningLookbackAtBoundsInTx(tx, ctx, lb, minBlock, maxBlock, false)
			var n int
			server.Raise(tx.QueryRow(ctx, `SELECT count(*) FROM client_reliability_running WHERE client_id=$1 AND lookback_index=0`, client).Scan(&n))
			if n != 0 {
				t.Fatal("control no longer exercises unchanged trusted delta bounds")
			}
		})
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE client_reliability_running_window SET observation_version=0,degraded_classification_write_token=gen_random_uuid(),observation_write_token=gen_random_uuid() WHERE lookback_index=0`))
			updateClientReliabilityRunningLookbackAtBoundsInTx(tx, ctx, lb, minBlock, maxBlock, false)
			var observed int64
			var independent float64
			server.Raise(tx.QueryRow(ctx, `SELECT observed_row_count,independent_sum FROM client_reliability_running WHERE client_id=$1 AND lookback_index=0`, client).Scan(&observed, &independent))
			if observed != 1 || independent != 0 || !reliabilityRunningObservationCurrent(readReliabilityRunningWindow(ctx, tx, 0)) {
				t.Fatal("late committed invalid evidence bypassed mandatory same-bounds repair")
			}
		})
	})
}

func TestReliabilityCommittedBoundsCost(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		server.Tx(ctx, func(tx server.PgTx) {
			const minBlock, maxBlock int64 = 29850000, 29850060
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO client_reliability_running(client_id,lookback_index,network_id,independent_sum,reliability_sum,observed_row_count)
    SELECT md5(i::text)::uuid,(i%4)::int,md5((i/10)::text)::uuid,10,10,10 FROM generate_series(1,100000)i`))
			writeReliabilityRunningWindow(ctx, tx, 0, minBlock, maxBlock, maxBlock)
			lb := reliabilityRunningLookback{lookbackIndex: 0, lookback: time.Hour}
			result := testing.Benchmark(func(b *testing.B) {
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					updateClientReliabilityRunningLookbackAtBoundsInTx(tx, ctx, lb, minBlock, maxBlock, true)
				}
				b.StopTimer()
			})
			t.Logf("unchanged bounds over100000 stored rows: %s %s", result.String(), result.MemString())
		})
	})
}
