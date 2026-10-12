package work

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// Contracts seeded in the checkpointed window: two full chunks and a partial.
const transferCloseCheckpointContractCount = 4337

// Seeds closed contracts with close times spread over [start, end): every
// seventh outcome is NULL, every eleventh canceled, the rest settled with an
// empty synthetic usage snapshot, which the settlement guard requires.
func seedTransferCloseCheckpointWindow(ctx context.Context, start time.Time, end time.Time) {
	step := end.Sub(start) / (transferCloseCheckpointContractCount + 1)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO transfer_contract (
				contract_id, source_network_id, source_id, destination_network_id, destination_id,
				transfer_byte_count, create_time, close_time, outcome, provider_usage
			)
			SELECT
				lpad(to_hex(5000000 + g), 32, '0')::uuid,
				lpad('d1', 32, '0')::uuid, lpad('d2', 32, '0')::uuid,
				lpad('d3', 32, '0')::uuid, lpad('d4', 32, '0')::uuid,
				1000, $1::timestamp - interval '1 hour',
				$1::timestamp + g * $3::bigint * interval '1 microsecond',
				CASE WHEN g % 7 = 0 THEN NULL WHEN g % 11 = 0 THEN 'canceled' ELSE 'settled' END,
				'{"version":1,"byte_count":0,"providers":[]}'::jsonb
			FROM generate_series(1, $2::int) g
			`,
			start,
			transferCloseCheckpointContractCount,
			step.Microseconds(),
		))
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO contract_close (contract_id, close_time, party, used_transfer_byte_count)
			SELECT lpad(to_hex(5000000 + g), 32, '0')::uuid, $1::timestamp, 'destination', g % 1000 + 1
			FROM generate_series(1, $2::int) g
			UNION ALL
			SELECT lpad(to_hex(5000000 + g), 32, '0')::uuid, $1::timestamp, 'source', 999
			FROM generate_series(1, $2::int) g
			`,
			start,
			transferCloseCheckpointContractCount,
		))
	})
}

// The single-statement sum a checkpointed walk must equal.
func transferCloseCheckpointExpectedSum(ctx context.Context, start time.Time, end time.Time, terminalOnly bool) model.ByteCount {
	var byteCount model.ByteCount
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`
			SELECT COALESCE(SUM(contract_close.used_transfer_byte_count), 0)::bigint
			FROM transfer_contract
			INNER JOIN contract_close ON
				contract_close.contract_id = transfer_contract.contract_id AND
				contract_close.party = 'destination'
			WHERE
				$1 <= transfer_contract.close_time AND
				transfer_contract.close_time < $2 AND
				(NOT $3 OR transfer_contract.outcome IS NOT NULL)
			`,
			start,
			end,
			terminalOnly,
		).Scan(&byteCount))
	})
	return byteCount
}

// A day split over several task runs, with its checkpoint stored as task args
// between runs, keeps the day's old row until the last run and then replaces
// it with exactly the single-statement sum.
func TestTransferAuditRollupTaskSplitsDayAcrossRuns(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc()
		dayEnd := now.UTC().Truncate(24 * time.Hour)
		dayStart := dayEnd.Add(-24 * time.Hour)
		seedTransferCloseCheckpointWindow(ctx, dayStart, dayEnd)
		want := transferCloseCheckpointExpectedSum(ctx, dayStart, dayEnd, false)

		dayRows := func() []model.ByteCount {
			byteCounts := []model.ByteCount{}
			server.Db(ctx, func(conn server.PgConn) {
				result, err := conn.Query(
					ctx,
					`
					SELECT transfer_byte_count FROM audit_contract_event
					WHERE event_details = $1 AND $2 <= event_time AND event_time < $3
					`,
					model.AuditEventDetailsTransferRollup,
					dayStart,
					dayEnd,
				)
				server.WithPgResult(result, err, func() {
					for result.Next() {
						var byteCount model.ByteCount
						server.Raise(result.Scan(&byteCount))
						byteCounts = append(byteCounts, byteCount)
					}
				})
			})
			return byteCounts
		}
		// an earlier rollup of the day stays until the day's walk completes
		model.AdvanceTransferAuditRollupDay(ctx, &model.TransferCloseByteSum{
			Start:    dayStart,
			End:      dayEnd,
			Complete: true,
		}, 1)
		if rows := dayRows(); len(rows) != 1 || rows[0] != 0 {
			t.Fatalf("seeded earlier day row=%v", rows)
		}

		// one chunk per run, so the 4337 contracts of the day need three runs
		advanceOneChunk := func(ctx context.Context, day *model.TransferCloseByteSum, _ int) bool {
			return model.AdvanceTransferAuditRollupDay(ctx, day, 1)
		}
		idle := func(context.Context) bool { return false }
		minTime := dayStart
		args := &RollupTransferAuditEventsArgs{MinTime: &minTime}
		runCount := 0
		for {
			result := rollupTransferAuditEvents(ctx, args, now, idle, advanceOneChunk)
			runCount += 1
			if result.DayCount == 1 {
				if result.Day != nil || result.MinTime != nil {
					t.Fatalf("completed last day kept a continuation: %+v", result)
				}
				break
			}
			if result.Day == nil || runCount == 3 {
				t.Fatalf("run %d did not checkpoint the unfinished day: %+v", runCount, result)
			}
			if rows := dayRows(); len(rows) != 1 || rows[0] != 0 {
				t.Fatalf("run %d replaced the day row before the walk completed: %v", runCount, rows)
			}
			next, _ := nextTransferAuditRollup(now, result)
			argsJson, err := json.Marshal(next)
			server.Raise(err)
			args = &RollupTransferAuditEventsArgs{}
			server.Raise(json.Unmarshal(argsJson, args))
		}
		if runCount != 3 {
			t.Fatalf("day took %d runs, want 3", runCount)
		}
		if rows := dayRows(); len(rows) != 1 || rows[0] != want {
			t.Fatalf("day rows=%v, want one row of the single-statement sum %d", rows, want)
		}
	})
}

// A clock walk split over several task runs carries its checkpoint through
// the real post hook and pending task args, reconciles the clock to exactly
// the single-statement sum once the walk completes, and schedules nothing
// after that.
func TestBackfillClockTaskCheckpointsTailAcrossRuns(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()
		end := server.NowUtc().Add(-time.Minute)
		seedTransferCloseCheckpointWindow(ctx, end.Add(-24*time.Hour), end)
		// no rollup rows exist, so the raw tail starts at the block-9 start
		want := transferCloseCheckpointExpectedSum(ctx, model.ClockSinceTime, server.NowUtc(), true)

		// the evaluator deletes its completed RunOnce row before the post hook
		post := func(args *BackfillClockArgs, result *BackfillClockResult) *BackfillClockArgs {
			var next *BackfillClockArgs
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `DELETE FROM pending_task WHERE run_once_key = '["backfill_clock",9]'`))
				server.Raise(BackfillClockPost(args, result, clientSession, tx))
				rows, err := tx.Query(ctx, `SELECT args_json FROM pending_task WHERE run_once_key = '["backfill_clock",9]'`)
				server.WithPgResult(rows, err, func() {
					if rows.Next() {
						var argsJson string
						server.Raise(rows.Scan(&argsJson))
						next = &BackfillClockArgs{}
						server.Raise(json.Unmarshal([]byte(argsJson), next))
					}
				})
			})
			return next
		}

		args := &BackfillClockArgs{}
		runCount := 0
		for {
			result := backfillClockRun(ctx, args, 1)
			runCount += 1
			next := post(args, result)
			if result.Progress == nil {
				if next != nil {
					t.Fatal("completed walk scheduled another run")
				}
				if result.TotalTransferByteCount != strconv.FormatInt(want, 10) {
					t.Fatalf("reconciled clock=%s, want the single-statement sum %d", result.TotalTransferByteCount, want)
				}
				break
			}
			if result.TotalTransferByteCount != "" || next == nil || next.Progress == nil || runCount == 2 {
				t.Fatalf("run %d did not hand its checkpoint to a continuation: %+v", runCount, result)
			}
			args = next
		}
		// 3718 terminal contracts (every seventh is NULL-outcome) are two chunks
		if runCount != 2 {
			t.Fatalf("walk took %d runs, want 2", runCount)
		}
		clock, ok := model.GetClock(ctx)
		if !ok || clock.TotalTransferByteCount != strconv.FormatInt(want, 10) {
			t.Fatalf("public clock=%+v, want %d", clock, want)
		}
	})
}
