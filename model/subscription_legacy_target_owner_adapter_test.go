package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/task"
)

// V13/V14 adapter. The runner overlays the separate no-owner adapter on V10.
// This executes the real claim/function/post/delete/final-record lifecycle.
func legacyTargetDrainMirrorOwners(t testing.TB, ctx context.Context, expected int) (int, int) {
	t.Helper()
	if expected == 0 {
		return 0, 0
	}
	if expected > 2 {
		t.Fatal("fixture mirror owner scope grew beyond its two grants", expected)
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET run_at=$1,release_time=$1 WHERE function_name=$2`, time.Time{}, task.NewTaskTarget(ApplyLegacyNetEscrowMirror).TargetFunctionName()))
	})
	settings := task.DefaultTaskWorkerSettings()
	settings.ClaimRegisteredTargetsOnly = true
	worker := task.NewTaskWorker(ctx, settings)
	defer worker.Close()
	worker.AddTargets(task.NewTaskTargetWithPost(ApplyLegacyNetEscrowMirror, ApplyLegacyNetEscrowMirrorPost))
	finished, retried, postRetried, err := worker.EvalTasks(expected)
	if err != nil || len(finished) != expected || len(retried) != 0 || len(postRetried) != 0 || legacyTargetMirrorQueueCount(ctx) != 0 {
		t.Fatal("fixture mirror owners failed ordinary finalization", len(finished), expected, err)
	}
	var starts int
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM finished_task WHERE task_id=ANY($1) AND run_start_time IS NOT NULL AND run_end_time IS NOT NULL`, finished).Scan(&starts))
	})
	if starts != expected {
		t.Fatal("mirror start/finish evidence did not join exact owner IDs", starts, expected)
	}
	return starts, len(finished)
}
