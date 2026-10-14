// A guarded args replacement writes only between claims and fences the claim
// generation; a leased, kept, absent or concurrently claimed row is untouched.
package task

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

type runOnceArgsReplaceRow struct {
	id         server.Id
	args       string
	claim      int64
	generation int64
	runAt      time.Time
	release    time.Time
}

func readRunOnceArgsReplaceRow(t testing.TB, ctx context.Context, runOnce *RunOnceOption) (row runOnceArgsReplaceRow) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT task_id,args_json,claim_generation,run_once_generation,run_at,release_time
			FROM pending_task WHERE run_once_key=$1`, runOnce.String()).Scan(
			&row.id, &row.args, &row.claim, &row.generation, &row.runAt, &row.release))
	})
	return
}

func scheduleRunOnceArgsReplaceRow(ctx context.Context, runOnce *RunOnceOption, cursor int) server.Id {
	owner := session.NewLocalClientSession(ctx, "", nil)
	defer owner.Cancel()
	var id server.Id
	server.Tx(ctx, func(tx server.PgTx) {
		id = ScheduleTaskInTx(tx, runOnceGenerationWork, &runOnceGenerationArgs{Scope: server.NewId(), Cursor: cursor}, owner, runOnce,
			RunAt(time.Unix(1, 0).UTC()))
	}, server.TxReadCommitted, server.OptNoRetry())
	return id
}

func replaceRunOnceArgs(ctx context.Context, runOnce *RunOnceOption, replace func(string) (string, bool)) (outcome RunOnceArgsReplacement) {
	server.Tx(ctx, func(tx server.PgTx) {
		outcome = ReplaceUnleasedRunOnceArgsInTx(ctx, tx, runOnce, server.NowUtc(), replace)
	}, server.TxReadCommitted, server.OptNoRetry())
	return
}

// A pending row takes the replacement in place, under a new claim generation;
// its wake generation and schedule are unchanged, and the next claim runs it.
func TestReplaceUnleasedRunOnceArgsReplacesBetweenClaims(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		key := runOnceGenerationKey(server.NewId())
		id := scheduleRunOnceArgsReplaceRow(ctx, key, 7)
		before := readRunOnceArgsReplaceRow(t, ctx, key)
		replacement := `{"scope":"00000000-0000-0000-0000-000000000000","cursor":0}`
		seen := ""
		outcome := replaceRunOnceArgs(ctx, key, func(args string) (string, bool) {
			seen = args
			return replacement, true
		})
		after := readRunOnceArgsReplaceRow(t, ctx, key)
		if outcome != RunOnceArgsReplaced || seen != before.args || after.id != id || after.args != replacement ||
			after.claim != before.claim+1 || after.generation != before.generation || !after.runAt.Equal(before.runAt) {
			t.Fatal("the unleased row did not take exactly the replacement", outcome, after)
		}
		var claimedArgs string
		target := &runOnceGenerationTarget{Target: NewTaskTarget(runOnceGenerationWork)}
		target.before = func(_ context.Context, queued *Task) error {
			claimedArgs = queued.ArgsJson
			return nil
		}
		settings := DefaultTaskWorkerSettings()
		settings.ClaimRegisteredTargetsOnly = true
		worker := NewTaskWorker(ctx, settings)
		defer worker.Close()
		worker.AddTargets(target)
		finished, _, _, err := worker.EvalTasks(1)
		if err != nil || len(finished) != 1 || finished[0] != id || claimedArgs != replacement {
			t.Fatal("the next claim did not run the replacement", finished, err, claimedArgs)
		}
	})
}

// While a lease holds the row, its run keeps its args and claim, and finishes.
func TestReplaceUnleasedRunOnceArgsLeavesLeasedRun(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		key := runOnceGenerationKey(server.NewId())
		id := scheduleRunOnceArgsReplaceRow(ctx, key, 7)
		var outcome RunOnceArgsReplacement
		var inFlight runOnceArgsReplaceRow
		target := &runOnceGenerationTarget{Target: NewTaskTarget(runOnceGenerationWork)}
		target.before = func(runCtx context.Context, queued *Task) error {
			outcome = replaceRunOnceArgs(runCtx, key, func(string) (string, bool) { return `{"cursor":0}`, true })
			server.Db(runCtx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(runCtx, `SELECT args_json,claim_generation FROM pending_task WHERE task_id=$1`, queued.TaskId).Scan(
					&inFlight.args, &inFlight.claim))
			})
			inFlight.id = queued.TaskId
			if inFlight.args != queued.ArgsJson || inFlight.claim != queued.ClaimGeneration {
				inFlight.id = server.Id{}
			}
			return nil
		}
		settings := DefaultTaskWorkerSettings()
		settings.ClaimRegisteredTargetsOnly = true
		worker := NewTaskWorker(ctx, settings)
		defer worker.Close()
		worker.AddTargets(target)
		finished, _, _, err := worker.EvalTasks(1)
		if outcome != RunOnceArgsLeased || inFlight.id != id {
			t.Fatal("the replacement touched a leased row", outcome)
		}
		if err != nil || len(finished) != 1 || finished[0] != id {
			t.Fatal("the run in flight did not finish under its own claim", finished, err)
		}
	})
}

// A caller that keeps the args writes nothing; an absent key reports absent.
func TestReplaceUnleasedRunOnceArgsKeepsArgsOrReportsAbsentKey(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		key := runOnceGenerationKey(server.NewId())
		scheduleRunOnceArgsReplaceRow(ctx, key, 7)
		before := readRunOnceArgsReplaceRow(t, ctx, key)
		if outcome := replaceRunOnceArgs(ctx, key, func(string) (string, bool) { return "", false }); outcome != RunOnceArgsKept {
			t.Fatal("kept args were not reported as kept", outcome)
		}
		if after := readRunOnceArgsReplaceRow(t, ctx, key); after != before {
			t.Fatal("keeping the args still wrote the row", after)
		}
		called := false
		outcome := replaceRunOnceArgs(ctx, runOnceGenerationKey(server.NewId()), func(string) (string, bool) {
			called = true
			return `{}`, true
		})
		if outcome != RunOnceArgsAbsent || called {
			t.Fatal("an absent key was not reported absent", outcome, called)
		}
	})
}

// A claim that commits between the read and the write wins: the write becomes
// a no-op and the claimed row keeps its args.
func TestReplaceUnleasedRunOnceArgsYieldsToConcurrentClaim(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		key := runOnceGenerationKey(server.NewId())
		id := scheduleRunOnceArgsReplaceRow(ctx, key, 7)
		before := readRunOnceArgsReplaceRow(t, ctx, key)
		outcome := replaceRunOnceArgs(ctx, key, func(args string) (string, bool) {
			// The decision itself is pure in production. Here a separate
			// transaction claims the row at exactly this point.
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET claim_time=$2,release_time=$3,claim_generation=claim_generation+1
					WHERE task_id=$1`, id, server.NowUtc(), server.NowUtc().Add(TaskLeaseTimeout)))
			}, server.TxReadCommitted, server.OptNoRetry())
			return `{"cursor":0}`, true
		})
		after := readRunOnceArgsReplaceRow(t, ctx, key)
		if outcome != RunOnceArgsChanged || after.args != before.args || after.claim != before.claim+1 {
			t.Fatal("the replacement overwrote a row claimed after it was read", outcome, after)
		}
	})
}
