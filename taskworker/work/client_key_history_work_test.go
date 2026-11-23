// The key history retention through real queue admission and completion. A
// batch that runs into its bound fails the run, so the task reports it, and
// the retry resumes after the batches that committed. Until 2026-11-23 such a
// run reported success: on Main every run's first batch was canceled at its
// 5 s statement bound from 2026-11-21 18Z, nothing was deleted for two days,
// and the task recorded 79 successes a day (DBDISK-RETENTION-REVIEW.md §3.3).
package work

import (
	"bytes"
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

// A page of due clients and one more, one registration each, the one more
// held by a writer past the batch's lock bound: the run deletes the page,
// fails at the next batch and is retried from the page's last record; once
// the writer lets go the retry deletes the last client and completes, and its
// Post rearms the chain with the pass ended.
func TestRemoveGoneStClientKeyHistoryBoundedBatchFailsTheRun(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		controlCtx, cancelControl := context.WithTimeout(ctx, 5*time.Second)
		defer cancelControl()
		server.Raise(model.SetStClientKeyHistoryRetentionControl(controlCtx, map[string]string{
			model.StClientKeyHistoryRetentionControlFieldMode:                "on",
			model.StClientKeyHistoryRetentionControlFieldMaxClientsPerSecond: "2000",
		}))
		defer func() {
			removeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			server.Raise(model.RemoveStClientKeyHistoryRetentionControl(removeCtx))
		}()

		settings := model.DefaultStClientKeyHistoryRetentionSettings()
		// gone 10 days ago, a second apart, so the last is the record after
		// the first page
		clientIds := make([]server.Id, settings.DeleteBatchCount+1)
		for i := range clientIds {
			clientIds[i] = server.NewId()
		}
		domainHash := bytes.Repeat([]byte{0x48}, 32)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `
				INSERT INTO st_client_key_history (client_id, generation, domain_hash, registration_hash, evidence_hash, evidence)
				SELECT
					c,
					1,
					$2,
					sha256(convert_to(c::text, 'UTF8')),
					'sha256:' || encode(sha256(convert_to(c::text, 'UTF8')), 'hex'),
					'\x7b7d'::bytea
				FROM unnest($1::uuid[]) AS c
			`, clientIds, domainHash))
			server.RaisePgResult(tx.Exec(ctx, `
				INSERT INTO st_client_key_head (client_id, domain_hash, network_id, generation)
				SELECT c, $2, $3, 1 FROM unnest($1::uuid[]) AS c
			`, clientIds, domainHash, server.NewId()))
			server.RaisePgResult(tx.Exec(ctx, `
				INSERT INTO st_client_key_gone (client_id, gone_time)
				SELECT c, (now() AT TIME ZONE 'UTC') - interval '10 days' + (i * interval '1 second')
				FROM unnest($1::uuid[]) WITH ORDINALITY AS u(c, i)
			`, clientIds))
		}, server.TxReadCommitted, server.OptNoRetry())
		remaining := func() (count int) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM st_client_key_history WHERE client_id = ANY($1)`, clientIds).Scan(&count))
			})
			return
		}
		runOnceKey := task.RunOnce("remove_gone_st_client_key_history").String()
		pendingArgs := func() (args RemoveGoneStClientKeyHistoryArgs, runAt time.Time, rescheduleError string) {
			var argsJson string
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT args_json, run_at, COALESCE(reschedule_error, '') FROM pending_task WHERE run_once_key = $1`, runOnceKey).Scan(&argsJson, &runAt, &rescheduleError))
			})
			server.Raise(json.Unmarshal([]byte(argsJson), &args))
			return
		}

		// a writer holds the last client's head past the lock bound
		holder, err := server.AcquireMaintenanceDbConn(ctx)
		server.Raise(err)
		defer holder.Release()
		held, err := holder.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		server.Raise(err)
		defer held.Rollback(context.WithoutCancel(ctx))
		server.RaisePgResult(held.Exec(ctx, `SELECT 1 FROM st_client_key_head WHERE client_id = $1 FOR UPDATE`, clientIds[len(clientIds)-1]))

		target := task.NewTaskTargetWithPost(RemoveGoneStClientKeyHistory, RemoveGoneStClientKeyHistoryPost)
		now := server.NowUtc()
		server.Tx(ctx, func(tx server.PgTx) {
			// a discovery pass just completed, so the run only deletes
			scheduleRemoveGoneStClientKeyHistory(owner, tx, &RemoveGoneStClientKeyHistoryArgs{
				Cursor: model.StClientKeyHistoryRetentionCursor{DiscoverCompleteTime: &now},
			}, time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC))
		}, server.TxReadCommitted, server.OptNoRetry())
		var id server.Id
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT task_id FROM pending_task WHERE run_once_key = $1`, runOnceKey).Scan(&id))
		})

		worker := startupClosureWorker(ctx, target)
		finished, retried, _, err := worker.EvalTasks(1)
		worker.Close()
		if err != nil || len(finished) != 0 || len(retried) != 1 || retried[0] != id {
			t.Fatal("the run whose batch ran into its bound did not fail and retry", finished, retried, err)
		}
		args, _, rescheduleError := pendingArgs()
		if remaining() != 1 || rescheduleError == "" ||
			args.Cursor.DeleteAfterClientId == nil || *args.Cursor.DeleteAfterClientId != clientIds[len(clientIds)-2] ||
			args.Cursor.DeletePassStartTime == nil || args.Cursor.DiscoverCompleteTime == nil {
			t.Fatalf("the retry does not resume after the committed batch: %d remaining, cursor %+v, error %q", remaining(), args.Cursor, rescheduleError)
		}

		server.Raise(held.Rollback(ctx))
		makeCloseRetryTaskDue(ctx, id)
		worker = startupClosureWorker(ctx, target)
		finished, retried, _, err = worker.EvalTasks(1)
		worker.Close()
		if err != nil || len(finished) != 1 || finished[0] != id || len(retried) != 0 || remaining() != 0 {
			t.Fatal("the retry did not delete the last client and complete", finished, retried, err, remaining())
		}
		successor, _, rescheduleError := pendingArgs()
		if successor.Cursor.DeleteAfterClientId != nil || successor.Cursor.DeleteAfterGoneTime != nil || successor.Cursor.DeletePassStartTime != nil || rescheduleError != "" {
			t.Fatalf("the completed run did not rearm with its pass ended: %+v, error %q", successor.Cursor, rescheduleError)
		}
	})
}
