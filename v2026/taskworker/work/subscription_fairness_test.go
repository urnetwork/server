// Fair expiry checkpoints cross the same durable task boundaries as settlement.
package work

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

// Successful zero-close pages persist both lanes, including the case where
// history has finished and only a recent raw continuation remains.
func TestCloseExpiryFairSuccessPersistsBothLaneCursors(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		defer clientSession.Cancel()
		stamp := server.NowUtc().Truncate(time.Microsecond)
		historical := &model.ContractExpiryCursor{ScanBefore: stamp.Add(-72 * time.Hour),
			Open: &model.ContractExpiryPosition{CreateTime: stamp.Add(-96 * time.Hour), ContractId: server.NewId()}, DisputeDone: true}
		recent := &model.ContractExpiryCursor{ScanBefore: stamp.Add(-time.Hour),
			Dispute: &model.ContractExpiryPosition{CreateTime: stamp.Add(-2 * time.Hour), ContractId: server.NewId()}, OpenDone: true}
		for _, historicalDone := range []bool{false, true} {
			sweep := &model.ContractExpirySweepCursor{Historical: historical, Recent: recent, RecentAfter: historical.ScanBefore,
				HistoricalDone: historicalDone, HistoricalNext: true,
				Fresh: &model.ContractExpiryCursor{ScanBefore: stamp.Add(-12 * time.Minute),
					Open: &model.ContractExpiryPosition{CreateTime: stamp.Add(-20 * time.Minute), ContractId: server.NewId()}},
				FreshBefore: stamp.Add(-30 * time.Minute), FreshNext: true}
			if historicalDone {
				sweep.Historical = nil
			}
			args := &CloseExpiredContractsArgs{BlockSize: 1, BlockIndex: 0}
			result, err := closeExpiredContractsSweepPageResult(ctx, args, 0, sweep, nil)
			if err != nil || !result.Full || result.Sweep != sweep || result.Cursor != sweep.Historical {
				t.Fatal("successful fair continuation lost a lane or immediate cadence")
			}
			server.Tx(ctx, func(tx server.PgTx) {
				server.Raise(CloseExpiredContractsPost(args, result, clientSession, tx))
			})
			var raw string
			var id server.Id
			var runAt time.Time
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT task_id,args_json,run_at FROM pending_task WHERE function_name=$1`,
					"github.com/urnetwork/server/v2026/taskworker/work.CloseExpiredContracts").Scan(&id, &raw, &runAt))
			})
			var stored CloseExpiredContractsArgs
			if err := json.Unmarshal([]byte(raw), &stored); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(stored.Sweep, sweep) || !reflect.DeepEqual(stored.Cursor, sweep.Historical) || runAt.After(stamp.Add(10*time.Second)) {
				t.Fatal("task post lost durable lane state or parked raw progress")
			}
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `DELETE FROM pending_task WHERE task_id=$1`, id))
			})
		}
	})
}

// The real target first visits a post-epoch rejected dispute, persists its
// historical handoff on the same failed task, and then completes that history.
// Both protected reservations and the verified sibling payouts retain owners.
func TestCloseExpiryFairAccountingRetryPersistsLaneHandoff(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		historical, recent := newCloseRetryFixture(t, ctx), newCloseRetryFixture(t, ctx)
		epoch := time.Date(2021, time.January, 1, 0, 0, 0, 0, time.UTC)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=$3 WHERE contract_id IN ($1,$2)`,
				recent.originId, recent.companionId, epoch.Add(time.Hour)))
		})
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		defer clientSession.Cancel()
		legacy := &model.ContractExpiryCursor{ScanBefore: epoch}
		task.ScheduleTask(CloseExpiredContracts, &CloseExpiredContractsArgs{BlockSize: 1, BlockIndex: 0, Cursor: legacy},
			clientSession, task.RunOnce("synthetic-expiry-fair-retry"))
		var id server.Id
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT task_id FROM pending_task WHERE run_once_key=$1`, `["synthetic-expiry-fair-retry"]`).Scan(&id))
		})
		worker := task.NewTaskWorkerWithDefaults(ctx)
		worker.AddTargets(task.NewTaskTargetWithPost(CloseExpiredContracts, CloseExpiredContractsPost))
		_, _, _, metadata, _ := readCloseRetryTask(t, ctx, id)
		for attempt := range 2 {
			makeCloseRetryTaskDue(ctx, id)
			finished, retried, posts, err := worker.EvalTasks(1)
			if err != nil || len(finished) != 0 || len(retried) != 1 || retried[0] != id || len(posts) != 0 {
				t.Fatal("fair accounting page lost its failing durable task")
			}
			stored, storedError, errorCount, currentMetadata, delay := readCloseRetryTask(t, ctx, id)
			if storedError == "" || errorCount != attempt+1 || currentMetadata != metadata || delay < time.Minute || 5*time.Minute <= delay {
				t.Fatal("fair retry changed failure evidence, task identity or accounting cadence")
			}
			if attempt == 0 {
				if stored.Sweep == nil || !stored.Sweep.HistoricalNext || stored.Sweep.HistoricalDone ||
					stored.Sweep.Recent != nil || !stored.Sweep.RecentAfter.Equal(epoch) ||
					!reflect.DeepEqual(stored.Sweep.Historical, legacy) || !reflect.DeepEqual(stored.Cursor, legacy) {
					t.Fatal("accounting retry lost the post-epoch to historical lane handoff")
				}
				recent.requireAccounting(t, ctx)
				if _, closed := model.GetContractClose(ctx, historical.originId); closed {
					t.Fatal("post-epoch page unexpectedly consumed historical work")
				}
			} else if stored.Sweep != nil || stored.Cursor != nil {
				t.Fatal("both completed lanes failed to reset the same rejected task")
			}
		}
		historical.requireAccounting(t, ctx)
		recent.requireAccounting(t, ctx)
	})
}
