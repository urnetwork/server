// The contract horizon reaper through real queue admission and completion: a
// failed batch checkpoints the last committed cursor into the retry, and a
// completed run rearms its successor with its cursor.
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

func TestRemoveContractsPastHorizonCheckpointsAndRearms(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		controlCtx, cancelControl := context.WithTimeout(ctx, 5*time.Second)
		defer cancelControl()
		server.Raise(model.SetContractHorizonControl(controlCtx, map[string]string{
			model.ContractHorizonControlFieldMode:             "on",
			model.ContractHorizonControlFieldBatchSize:        "125",
			model.ContractHorizonControlFieldMaxRowsPerSecond: "2000",
		}))
		defer func() {
			removeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			server.Raise(model.RemoveContractHorizonControl(removeCtx))
		}()

		// 130 canceled contracts minted and closed 120 days ago
		minted := server.NowUtc().Add(-120 * 24 * time.Hour)
		contractIds := make([]server.Id, 130)
		server.Tx(ctx, func(tx server.PgTx) {
			for i := range contractIds {
				contractIds[i] = server.ContractIdFloor(minted.Add(time.Duration(i) * time.Second))
				contractIds[i][15] = 1
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
					(contract_id,source_network_id,source_id,destination_network_id,destination_id,
					 transfer_byte_count,create_time,close_time,outcome)
					VALUES($1,$2,$3,$4,$5,0,$6,$6,'canceled')`,
					contractIds[i], server.NewId(), server.NewId(), server.NewId(), server.NewId(), minted.Add(time.Duration(i)*time.Second)))
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_extender(contract_id,extender_id,party,client_id,network_id)
					VALUES($1,$2,'source',$3,$4)`, contractIds[i], server.NewId(), server.NewId(), server.NewId()))
			}
		}, server.TxReadCommitted, server.OptNoRetry())
		remaining := func() (count int) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($1)`, contractIds).Scan(&count))
			})
			return
		}
		pendingArgs := func() (args RemoveContractsPastHorizonArgs, runAt time.Time, rescheduleError string) {
			var argsJson string
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT args_json,run_at,COALESCE(reschedule_error,'') FROM pending_task
					WHERE run_once_key=$1`, task.RunOnce("remove_contracts_past_horizon").String()).Scan(&argsJson, &runAt, &rescheduleError))
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
		server.RaisePgResult(held.Exec(ctx, `SELECT 1 FROM contract_extender WHERE contract_id=$1 FOR UPDATE`, contractIds[129]))

		settings := model.DefaultContractHorizonSettings()
		target := task.NewTaskTargetWithPost(RemoveContractsPastHorizon, RemoveContractsPastHorizonPost)
		var id server.Id
		server.Tx(ctx, func(tx server.PgTx) {
			scheduleRemoveContractsPastHorizon(owner, tx, model.ContractHorizonCursor{}, time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), settings)
		}, server.TxReadCommitted, server.OptNoRetry())
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT task_id FROM pending_task WHERE run_once_key=$1`,
				task.RunOnce("remove_contracts_past_horizon").String()).Scan(&id))
		})

		worker := startupClosureWorker(ctx, target)
		finished, retried, _, err := worker.EvalTasks(1)
		worker.Close()
		if err != nil || len(finished) != 0 || len(retried) != 1 || retried[0] != id {
			t.Fatal("the blocked run did not retry", finished, retried, err)
		}
		args, _, rescheduleError := pendingArgs()
		if remaining() != 5 || args.Cursor.ContractId != contractIds[124] || rescheduleError == "" {
			t.Fatalf("the retry does not resume after the committed batch: %d remaining, %+v, error %q", remaining(), args.Cursor, rescheduleError)
		}

		server.Raise(held.Rollback(ctx))
		makeCloseRetryTaskDue(ctx, id)
		worker = startupClosureWorker(ctx, target)
		finished, retried, _, err = worker.EvalTasks(1)
		worker.Close()
		if err != nil || len(finished) != 1 || finished[0] != id || len(retried) != 0 || remaining() != 0 {
			t.Fatal("the resumed run did not finish the pass", finished, retried, err, remaining())
		}
		successor, runAt, _ := pendingArgs()
		if successor.Cursor.ContractId != contractIds[129] ||
			runAt.Before(server.NowUtc().Add(settings.IdleInterval-time.Minute)) || server.NowUtc().Add(settings.IdleInterval).Before(runAt) {
			t.Fatalf("the caught up pass did not rearm at the idle interval with its cursor: %+v at %s", successor.Cursor, runAt)
		}

		// a run with work left comes back after the resume interval
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM pending_task WHERE run_once_key=$1`, task.RunOnce("remove_contracts_past_horizon").String()))
			server.Raise(RemoveContractsPastHorizonPost(
				&RemoveContractsPastHorizonArgs{},
				&RemoveContractsPastHorizonResult{Cursor: successor.Cursor},
				owner,
				tx,
			))
		}, server.TxReadCommitted, server.OptNoRetry())
		resumed, runAt, _ := pendingArgs()
		if resumed.Cursor.ContractId != contractIds[129] || server.NowUtc().Add(settings.ResumeInterval).Before(runAt) {
			t.Fatalf("a pass with work left did not resume soon: %+v at %s", resumed.Cursor, runAt)
		}
	})
}

// Pure: a run that reached the horizon, found the reaper off or its control
// unreadable, or only dry ran comes back after the idle interval; a run with
// work left comes back at once.
func TestContractHorizonRunIdle(t *testing.T) {
	for _, sample := range []struct {
		report *model.ContractHorizonReport
		idle   bool
	}{
		{report: &model.ContractHorizonReport{Mode: model.ContractHorizonModeOn, Stop: "budget"}, idle: false},
		{report: &model.ContractHorizonReport{Mode: model.ContractHorizonModeOn, Stop: "canceled"}, idle: false},
		{report: &model.ContractHorizonReport{Mode: model.ContractHorizonModeOn, Stop: "horizon", CaughtUp: true}, idle: true},
		{report: &model.ContractHorizonReport{Mode: model.ContractHorizonModeOff, Stop: "off"}, idle: true},
		{report: &model.ContractHorizonReport{Mode: model.ContractHorizonModeOff, Stop: "control_unavailable"}, idle: true},
		{report: &model.ContractHorizonReport{Mode: model.ContractHorizonModeDryRun, Stop: "budget"}, idle: true},
	} {
		if contractHorizonRunIdle(sample.report) != sample.idle {
			t.Errorf("%+v: idle %t, want %t", sample.report, !sample.idle, sample.idle)
		}
	}
}
