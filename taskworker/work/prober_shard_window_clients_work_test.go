// The prober retention's window step through real queue admission and
// completion: a pass whose windows fail to delete hands the cursor before it
// to the retry, the retry deletes them, and a caught up run rearms its
// successor with its cursor after the idle interval.
package work

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

func TestRemoveProberShardWindowsCheckpointsAndRearms(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		controlCtx, cancelControl := context.WithTimeout(ctx, 5*time.Second)
		defer cancelControl()
		server.Raise(model.SetProberShardRetentionControl(controlCtx, map[string]string{
			model.ProberShardRetentionControlFieldMode:                     "on",
			model.ProberShardRetentionControlFieldMaxWindowShardsPerMinute: "120",
		}))
		defer func() {
			removeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			server.Raise(model.RemoveProberShardRetentionControl(removeCtx))
		}()

		// a settled pass with two windows, whose deadline is two hours past
		settledPass := func(index int) (model.ProberShardKey, model.ProberShardWindowCursor, []server.Id) {
			key := model.ProberShardKey{TaskId: server.NewId(), Epoch: server.NewId(), ShardIndex: index, ShardCount: 4}
			pass, err := model.BeginProberShard(ctx, key, 64*1024, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			windows := []server.Id{server.NewId(), server.NewId()}
			server.Tx(ctx, func(tx server.PgTx) {
				for _, windowId := range windows {
					server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client
						(client_id,network_id,device_id,source_client_id,description,active,create_time,auth_time)
						VALUES ($1,$2,$3,$4,$5,true,$6,$6)`,
						windowId, pass.NetworkId, pass.DeviceId, pass.ClientId, model.ProberClientDescription, server.NowUtc()))
				}
			}, server.TxReadCommitted, server.OptNoRetry())
			server.Raise(model.DrainProberShard(ctx, key))
			server.Raise(model.SettleProberShard(ctx, key))
			cursor := model.ProberShardWindowCursor{TaskId: key.TaskId, Epoch: key.Epoch}
			server.Tx(ctx, func(tx server.PgTx) {
				server.Raise(tx.QueryRow(ctx, `UPDATE prober_shard_run
					SET create_time=create_time-interval '3 hours', deadline=deadline-interval '3 hours'
					WHERE task_id=$1 AND epoch=$2 RETURNING create_time`, key.TaskId, key.Epoch).Scan(&cursor.CreateTime))
			}, server.TxReadCommitted, server.OptNoRetry())
			return key, cursor, windows
		}
		firstKey, firstCursor, firstWindows := settledPass(0)
		secondKey, secondCursor, secondWindows := settledPass(1)
		windowsLeft := func(windows []server.Id) (count int) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM network_client WHERE client_id=ANY($1)`, windows).Scan(&count))
			})
			return
		}
		sameCursor := func(a model.ProberShardWindowCursor, b model.ProberShardWindowCursor) bool {
			return a.CreateTime.Equal(b.CreateTime) && a.TaskId == b.TaskId && a.Epoch == b.Epoch
		}
		pendingArgs := func() (args RemoveProberShardWindowsArgs, runAt time.Time, rescheduleError string) {
			var argsJson string
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT args_json,run_at,COALESCE(reschedule_error,'') FROM pending_task
					WHERE run_once_key=$1`, task.RunOnce("remove_prober_shard_windows").String()).Scan(&argsJson, &runAt, &rescheduleError))
			})
			server.Raise(json.Unmarshal([]byte(argsJson), &args))
			return
		}

		// a writer holds the second pass's window's location fold past the
		// lock bound
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client_location_reliability (client_id, update_block_number, network_id)
				VALUES ($1, 1, $2)`, secondWindows[0], proberShardTestNetworkId(t, ctx, secondKey)))
		}, server.TxReadCommitted, server.OptNoRetry())
		holder, err := server.AcquireMaintenanceDbConn(ctx)
		server.Raise(err)
		defer holder.Release()
		held, err := holder.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		server.Raise(err)
		defer held.Rollback(context.WithoutCancel(ctx))
		server.RaisePgResult(held.Exec(ctx, `SELECT 1 FROM network_client_location_reliability WHERE client_id=$1 FOR UPDATE`, secondWindows[0]))

		settings := model.DefaultProberShardRetentionSettings()
		target := task.NewTaskTargetWithPost(RemoveProberShardWindows, RemoveProberShardWindowsPost)
		var id server.Id
		server.Tx(ctx, func(tx server.PgTx) {
			scheduleRemoveProberShardWindows(owner, tx, model.ProberShardWindowCursor{}, time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), settings)
		}, server.TxReadCommitted, server.OptNoRetry())
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT task_id FROM pending_task WHERE run_once_key=$1`,
				task.RunOnce("remove_prober_shard_windows").String()).Scan(&id))
		})

		worker := startupClosureWorker(ctx, target)
		finished, retried, _, err := worker.EvalTasks(1)
		worker.Close()
		if err != nil || len(finished) != 0 || len(retried) != 1 || retried[0] != id {
			t.Fatal("the blocked run did not retry", finished, retried, err)
		}
		args, _, rescheduleError := pendingArgs()
		if windowsLeft(firstWindows) != 0 || windowsLeft(secondWindows) != 2 || !sameCursor(args.Cursor, firstCursor) || rescheduleError == "" {
			t.Fatalf("the retry does not resume at the failed pass: first %d, second %d windows left, %+v, error %q",
				windowsLeft(firstWindows), windowsLeft(secondWindows), args.Cursor, rescheduleError)
		}
		if state := proberShardTestState(t, ctx, firstKey); state != "draining" {
			t.Fatalf("the first pass is %q after its windows went, want draining until its generation drops", state)
		}

		server.Raise(held.Rollback(ctx))
		makeCloseRetryTaskDue(ctx, id)
		worker = startupClosureWorker(ctx, target)
		finished, retried, _, err = worker.EvalTasks(1)
		worker.Close()
		if err != nil || len(finished) != 1 || finished[0] != id || len(retried) != 0 || windowsLeft(secondWindows) != 0 {
			t.Fatal("the retry did not delete the second pass's windows", finished, retried, err, windowsLeft(secondWindows))
		}
		successor, runAt, _ := pendingArgs()
		if !sameCursor(successor.Cursor, secondCursor) ||
			runAt.Before(server.NowUtc().Add(settings.IdleInterval-time.Minute)) || server.NowUtc().Add(settings.IdleInterval).Before(runAt) {
			t.Fatalf("the caught up run did not rearm at the idle interval with its cursor: %+v at %s", successor.Cursor, runAt)
		}

		// a run with passes left comes back after the resume interval
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM pending_task WHERE run_once_key=$1`, task.RunOnce("remove_prober_shard_windows").String()))
			server.Raise(RemoveProberShardWindowsPost(
				&RemoveProberShardWindowsArgs{},
				&RemoveProberShardWindowsResult{Cursor: firstCursor},
				owner,
				tx,
			))
		}, server.TxReadCommitted, server.OptNoRetry())
		resumed, runAt, _ := pendingArgs()
		if !sameCursor(resumed.Cursor, firstCursor) || server.NowUtc().Add(settings.ResumeInterval).Before(runAt) {
			t.Fatalf("a run with passes left did not resume soon with its cursor: %+v at %s", resumed.Cursor, runAt)
		}
	})
}
