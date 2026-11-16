// The prober shard retention through real queue admission and completion: a
// failed walk batch checkpoints the last committed walk into the retry, and a
// completed run rearms its successor with its walk.
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

func TestRemoveProberShardGenerationsCheckpointsAndRearms(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		controlCtx, cancelControl := context.WithTimeout(ctx, 5*time.Second)
		defer cancelControl()
		server.Raise(model.SetProberShardRetentionControl(controlCtx, map[string]string{
			model.ProberShardRetentionControlFieldMode:                  "on",
			model.ProberShardRetentionControlFieldMaxWalkRowsPerSecond:  "20000",
			model.ProberShardRetentionControlFieldMaxContractsPerSecond: "2000",
		}))
		defer func() {
			removeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			server.Raise(model.RemoveProberShardRetentionControl(removeCtx))
		}()

		// a pass of a dropped generation: created, and past its deadline, nine
		// days ago
		key := model.ProberShardKey{TaskId: server.NewId(), Epoch: server.NewId(), ShardIndex: 0, ShardCount: 4}
		pass, err := model.BeginProberShard(ctx, key, 64*1024, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		server.Raise(model.DrainProberShard(ctx, key))
		server.Raise(model.SettleProberShard(ctx, key))
		var createTime time.Time
		server.Tx(ctx, func(tx server.PgTx) {
			server.Raise(tx.QueryRow(ctx, `UPDATE prober_shard_run
				SET create_time=create_time-interval '9 days', deadline=deadline-interval '9 days'
				WHERE task_id=$1 AND epoch=$2 RETURNING create_time`, key.TaskId, key.Epoch).Scan(&createTime))
		}, server.TxReadCommitted, server.OptNoRetry())
		generation := createTime.UTC().Truncate(model.DefaultProberShardRetentionSettings().GenerationDuration)

		// 600 resolved zero-byte contracts the pass's network pays, minted then:
		// the first walk batch keeps 500
		minted := server.NowUtc().Add(-9 * 24 * time.Hour)
		contractIds := make([]server.Id, 600)
		server.Tx(ctx, func(tx server.PgTx) {
			for i := range contractIds {
				at := minted.Add(time.Duration(i) * time.Millisecond)
				contractIds[i] = server.ContractIdFloor(at)
				tail := server.NewId()
				copy(contractIds[i][6:], tail[6:])
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
					(contract_id,source_network_id,source_id,destination_network_id,destination_id,payer_network_id,
					 transfer_byte_count,create_time,close_time,outcome)
					VALUES($1,$2,$3,$4,$5,$2,0,$6,$6,'canceled')`,
					contractIds[i], pass.NetworkId, pass.ClientId, server.NewId(), server.NewId(), at))
			}
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_extender(contract_id,extender_id,party,client_id,network_id)
				VALUES($1,$2,'source',$3,$4)`, contractIds[599], server.NewId(), server.NewId(), server.NewId()))
		}, server.TxReadCommitted, server.OptNoRetry())
		remaining := func() (count int) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($1)`, contractIds).Scan(&count))
			})
			return
		}
		pendingArgs := func() (args RemoveProberShardGenerationsArgs, runAt time.Time, rescheduleError string) {
			var argsJson string
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT args_json,run_at,COALESCE(reschedule_error,'') FROM pending_task
					WHERE run_once_key=$1`, task.RunOnce("remove_prober_shard_generations").String()).Scan(&argsJson, &runAt, &rescheduleError))
			})
			server.Raise(json.Unmarshal([]byte(argsJson), &args))
			return
		}

		// a writer holds the second batch's last extender row past the lock bound
		holder, err := server.AcquireMaintenanceDbConn(ctx)
		server.Raise(err)
		defer holder.Release()
		held, err := holder.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		server.Raise(err)
		defer held.Rollback(context.WithoutCancel(ctx))
		server.RaisePgResult(held.Exec(ctx, `SELECT 1 FROM contract_extender WHERE contract_id=$1 FOR UPDATE`, contractIds[599]))

		settings := model.DefaultProberShardRetentionSettings()
		target := task.NewTaskTargetWithPost(RemoveProberShardGenerations, RemoveProberShardGenerationsPost)
		var id server.Id
		server.Tx(ctx, func(tx server.PgTx) {
			scheduleRemoveProberShardGenerations(owner, tx, model.ProberShardContractWalk{}, time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), settings)
		}, server.TxReadCommitted, server.OptNoRetry())
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT task_id FROM pending_task WHERE run_once_key=$1`,
				task.RunOnce("remove_prober_shard_generations").String()).Scan(&id))
		})

		worker := startupClosureWorker(ctx, target)
		finished, retried, _, err := worker.EvalTasks(1)
		worker.Close()
		if err != nil || len(finished) != 0 || len(retried) != 1 || retried[0] != id {
			t.Fatal("the blocked run did not retry", finished, retried, err)
		}
		args, _, rescheduleError := pendingArgs()
		if remaining() != 100 || !args.Walk.Generation.Equal(generation) || args.Walk.Cursor != contractIds[499] || rescheduleError == "" {
			t.Fatalf("the retry does not resume after the committed batch: %d remaining, %+v, error %q", remaining(), args.Walk, rescheduleError)
		}
		if state := proberShardTestState(t, ctx, key); state != "draining" {
			t.Fatalf("the pass is %q before its generation's walk completed, want draining", state)
		}

		server.Raise(held.Rollback(ctx))
		makeCloseRetryTaskDue(ctx, id)
		worker = startupClosureWorker(ctx, target)
		finished, retried, _, err = worker.EvalTasks(1)
		worker.Close()
		if err != nil || len(finished) != 1 || finished[0] != id || len(retried) != 0 || remaining() != 0 {
			t.Fatal("the resumed run did not finish the walk", finished, retried, err, remaining())
		}
		if state := proberShardTestState(t, ctx, key); state != "" {
			t.Fatalf("the pass is %q after its generation's walk completed, want dropped", state)
		}
		successor, runAt, _ := pendingArgs()
		if !successor.Walk.CompleteThrough.Equal(generation) || !successor.Walk.Generation.IsZero() ||
			runAt.Before(server.NowUtc().Add(settings.IdleInterval-time.Minute)) || server.NowUtc().Add(settings.IdleInterval).Before(runAt) {
			t.Fatalf("the caught up run did not rearm at the idle interval with its walk: %+v at %s", successor.Walk, runAt)
		}

		// a run with a walk left comes back after the resume interval, with it
		inProgress := model.ProberShardContractWalk{
			CompleteThrough: generation,
			Generation:      generation.Add(settings.GenerationDuration),
			Lower:           contractIds[0],
			Upper:           contractIds[599],
			Cursor:          contractIds[300],
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM pending_task WHERE run_once_key=$1`, task.RunOnce("remove_prober_shard_generations").String()))
			server.Raise(RemoveProberShardGenerationsPost(
				&RemoveProberShardGenerationsArgs{},
				&RemoveProberShardGenerationsResult{Walk: inProgress},
				owner,
				tx,
			))
		}, server.TxReadCommitted, server.OptNoRetry())
		resumed, runAt, _ := pendingArgs()
		if resumed.Walk.Cursor != inProgress.Cursor || !resumed.Walk.Generation.Equal(inProgress.Generation) ||
			server.NowUtc().Add(settings.ResumeInterval).Before(runAt) {
			t.Fatalf("a run with a walk left did not resume soon with it: %+v at %s", resumed.Walk, runAt)
		}
	})
}

// Pure: a run that caught up, found the drop off or its control unreadable, or
// only dry ran comes back after the idle interval; a run with passes or a walk
// left comes back at once.
func TestProberShardRetentionRunIdle(t *testing.T) {
	for _, sample := range []struct {
		report *model.ProberShardRetentionReport
		idle   bool
	}{
		{report: &model.ProberShardRetentionReport{Mode: model.ProberShardRetentionModeOn, Stop: "budget"}, idle: false},
		{report: &model.ProberShardRetentionReport{Mode: model.ProberShardRetentionModeOn, Stop: "canceled"}, idle: false},
		{report: &model.ProberShardRetentionReport{Mode: model.ProberShardRetentionModeOn, Stop: "caught_up", CaughtUp: true}, idle: true},
		{report: &model.ProberShardRetentionReport{Mode: model.ProberShardRetentionModeOff, Stop: "off"}, idle: true},
		{report: &model.ProberShardRetentionReport{Mode: model.ProberShardRetentionModeOff, Stop: "control_unavailable"}, idle: true},
		{report: &model.ProberShardRetentionReport{Mode: model.ProberShardRetentionModeDryRun, Stop: "dry_run"}, idle: true},
	} {
		if proberShardRetentionRunIdle(sample.report) != sample.idle {
			t.Errorf("%+v: idle %t, want %t", sample.report, !sample.idle, sample.idle)
		}
	}
}
