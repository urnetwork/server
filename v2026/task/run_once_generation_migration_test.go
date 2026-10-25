// Migrate an actual old-format pending owner without changing its payload or lease.
package task

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

func TestRunOnceMigrationPreservesOldPendingOwner(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
		defer cancel()
		server.ApplyDbMigrationsUpTo(ctx, 795)
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		scope := server.NewId()
		prepared := prepareTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: scope, Cursor: 42}, owner,
			runOnceGenerationKey(scope), RunAt(server.NowUtc().Add(-time.Hour)))
		claimedAt := server.NowUtc().Truncate(time.Microsecond)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, queuedTaskInsertSql,
				prepared.taskId, prepared.functionName, prepared.argsJson, prepared.clientAddressHash,
				prepared.clientAddressPort, prepared.byJwtJson, prepared.runAt, prepared.runOnceKey,
				prepared.priority, prepared.maxTimeSeconds, claimedAt))
		})
		server.ApplyDbMigrationsUpTo(ctx, 796)
		if server.MinimumRuntimeMigrationVersion() < 796 {
			t.Fatal("new runtime permits startup without the generation schema")
		}
		row := GetTasks(ctx, prepared.taskId)[prepared.taskId]
		if row == nil || row.ArgsJson != string(prepared.argsJson) || !row.ClaimTime.Equal(claimedAt) || !row.ReleaseTime.Equal(claimedAt) ||
			row.RunOnceGeneration != 0 || row.ClaimGeneration != 0 {
			t.Fatal("additive schema changed existing payload, lease or default generations", row)
		}
		ScheduleTask(runOnceGenerationWork, &runOnceGenerationArgs{Scope: scope}, owner,
			runOnceGenerationKey(scope), RunAt(server.NowUtc().Add(-time.Hour)))
		if row := GetTasks(ctx, prepared.taskId)[prepared.taskId]; row == nil || row.RunOnceGeneration != 1 || row.ArgsJson != string(prepared.argsJson) {
			t.Fatal("new writer did not retain and mark the migrated owner")
		}
		// Explicit retirement of the old timestamp lease models the required
		// deployment boundary. The next new claim absorbs all preclaim work.
		ReleaseTask(ctx, prepared.taskId)
		worker := runOnceGenerationWorker(ctx, &runOnceGenerationTarget{Target: NewTaskTarget(runOnceGenerationWork)})
		defer worker.Close()
		finished, retried, posts, err := worker.EvalTasks(1)
		if err != nil || len(finished) != 1 || finished[0] != prepared.taskId || len(retried)+len(posts) != 0 || len(runOnceGenerationPending(ctx, []server.Id{scope})) != 0 {
			t.Fatal("new claim did not recover the migrated pending owner", err)
		}
	})
}
