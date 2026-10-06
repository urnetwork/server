// The task framework's own writes in a post's transaction raise a failed
// statement at the statement. Dropping or returning the error left the
// transaction aborted: a later statement was refused with no cause, or the
// commit rolled back and server.Tx retried it for a minute.
package task

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// Far inside server.Tx's one-minute retry window, so a call that still holds
// its transaction fails at the deadline.
const postStatementFailureCallTimeout = 10 * time.Second

// Fails every UPDATE of finished_task with "injected failure on UPDATE
// finished_task", until the returned func removes the failure.
func failFinishedTaskUpdates(ctx context.Context) (restore func()) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `
			CREATE FUNCTION synthetic_finished_task_update_failure() RETURNS trigger
			LANGUAGE plpgsql AS $$
			BEGIN
				RAISE EXCEPTION 'injected failure on % %', TG_OP, TG_TABLE_NAME;
			END
			$$;
			CREATE TRIGGER synthetic_finished_task_update_failure
			BEFORE UPDATE ON finished_task
			FOR EACH ROW EXECUTE FUNCTION synthetic_finished_task_update_failure();
		`))
	})
	return func() {
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `
				DROP TRIGGER synthetic_finished_task_update_failure ON finished_task;
				DROP FUNCTION synthetic_finished_task_update_failure();
			`))
		})
	}
}

// Renames finished_task away, so every statement on it fails with
// undefined_table, until the returned func renames it back.
func makeFinishedTaskUnavailable(ctx context.Context) (restore func()) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `ALTER TABLE finished_task RENAME TO finished_task_forced_unavailable`))
	})
	return func() {
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `ALTER TABLE finished_task_forced_unavailable RENAME TO finished_task`))
		})
	}
}

// Whether any database error in the chain has the code.
func hasPgErrorCode(err error, code string) bool {
	if pgErr, ok := err.(*pgconn.PgError); ok && pgErr.Code == code {
		return true
	}
	switch v := err.(type) {
	case interface{ Unwrap() []error }:
		for _, wrapped := range v.Unwrap() {
			if hasPgErrorCode(wrapped, code) {
				return true
			}
		}
	case interface{ Unwrap() error }:
		return hasPgErrorCode(v.Unwrap(), code)
	}
	return false
}

// The finishing transaction records a post's error on its finished task. When
// that update fails, the finish ends with the update's own error; no later
// statement runs refused in the aborted transaction.
func TestFinishRaisesFailedPostErrorUpdate(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "192.0.2.1:1", nil)
		defer clientSession.Cancel()

		probe := &commitPostProbe{
			ctx:                 ctx,
			failedPostNumbers:   map[int32]bool{1: true},
			unmarkedPostNumbers: map[int32]bool{1: true},
		}
		worker := newCommitPostWorker(ctx, probe.post)
		defer worker.Close()
		scheduleCommitPostWork(clientSession)

		restore := failFinishedTaskUpdates(ctx)
		var evalPanic any
		func() {
			defer func() {
				evalPanic = recover()
			}()
			worker.EvalTasks(1)
		}()
		restore()

		err, _ := evalPanic.(error)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "P0001" || pgErr.Message != "injected failure on UPDATE finished_task" {
			t.Fatalf("finish panic = %v, want the post error update's failure", evalPanic)
		}
		if hasPgErrorCode(err, "25P02") {
			t.Fatalf("finish panic = %v, want the update raised before any refused statement", evalPanic)
		}
	})
}

// The post of a post retry marks its finished task complete. A failed update
// raises at the statement instead of returning to the post retry's callback.
func TestRunPostPostRaisesFailedUpdate(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "192.0.2.1:1", nil)
		defer clientSession.Cancel()
		worker := NewTaskWorker(ctx, DefaultTaskWorkerSettings())
		defer worker.Close()

		restore := makeFinishedTaskUnavailable(ctx)
		postReturned := false
		var postPanic any
		func() {
			callCtx, cancel := context.WithTimeout(ctx, postStatementFailureCallTimeout)
			defer cancel()
			defer func() {
				postPanic = recover()
			}()
			server.Tx(callCtx, func(tx server.PgTx) {
				postReturned = false
				// as the post retry does, a returned error ends the callback
				// normally
				_ = worker.RunPostPost(&RunPostArgs{TaskId: server.NewId()}, &RunPostResult{}, clientSession, tx)
				postReturned = true
			})
		}()
		restore()

		err, _ := postPanic.(error)
		var pgErr *pgconn.PgError
		switch {
		case !errors.As(err, &pgErr) || pgErr.Code != "42P01":
			t.Fatalf("post panic = %v, want the update's undefined table failure", postPanic)
		case postReturned:
			t.Fatalf("the post returned after its failed update (%v), want it raised", postPanic)
		case errors.Is(err, pgx.ErrTxCommitRollback):
			t.Fatalf("post panic = %v, want the update raised, not a commit that rolled back", postPanic)
		}
	})
}
