// A claim statement slower than its claim context leaves the collector's guard
// session usable.
package task

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// The collector's guard session keeps one live claimed task. A refill claim
// then runs a statement that would outlive the refill's context. The server
// must end it first: the claim fails as a statement timeout before its context
// expires, the session still answers, and the next refill on it claims the
// remaining task. A context deadline during the statement would instead make
// pgx close the session that live executions finalize on.
func TestTaskClaimStatementEndsBeforeItsClaimContext(t *testing.T) {
	runOnceGenerationEnv(t, func(t testing.TB, ctx context.Context) {
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		worker := NewTaskWorker(ctx, DefaultTaskWorkerSettings())
		defer worker.Close()
		worker.AddTargets(NewTaskTarget(claimProfileAllowed))
		past := time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC)
		first := ScheduleTask(claimProfileAllowed, &claimProfileArgs{}, owner, RunAt(past))
		second := ScheduleTask(claimProfileAllowed, &claimProfileArgs{}, owner, RunAt(past.Add(time.Minute)))
		claimed, guard, err := worker.takeTasks(1)
		if guard != nil {
			defer guard.release()
		}
		if err != nil || guard == nil || len(claimed) != 1 || claimed[first] == nil {
			t.Fatal("first claim did not keep a live task on its guard", len(claimed), err)
		}

		worker.claimBeforeQuery = func(tx server.PgTx) error {
			_, err := tx.Exec(ctx, `SELECT pg_sleep(60)`)
			return err
		}
		claimCtx, cancel := context.WithTimeout(ctx, claimStatementMargin+time.Second)
		refused, _, _, err := worker.takeTasksWithGuard(claimCtx, 1, guard, taskClaimOptions{ordinaryOnly: true})
		expired := claimCtx.Err()
		cancel()
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "57014" || expired != nil || len(refused) != 0 {
			t.Fatal("slow claim statement outlived its claim context", err, expired)
		}
		if err := guard.ping(ctx); err != nil {
			t.Fatal("slow claim closed the collector's guard session", err)
		}

		worker.claimBeforeQuery = nil
		refillCtx, cancel := context.WithTimeout(ctx, DefaultTaskFinalizeTimeout)
		defer cancel()
		refill, _, _, err := worker.takeTasksWithGuard(refillCtx, 1, guard, taskClaimOptions{ordinaryOnly: true})
		if err != nil || len(refill) != 1 || refill[second] == nil {
			t.Fatal("guard session could not claim after the ended statement", len(refill), err)
		}
	})
}
