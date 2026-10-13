// The startup retry loop waits out a run in flight, then starts its successor on
// a fresh pass. Its timer and attempts are driven through the start's context.
package taskworker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/task"
	"github.com/urnetwork/server/taskworker/work"
)

// A start finds the retained run in flight and leaves it alone; the run hands
// back its own checkpoint, and the next retry replaces the args its successor runs.
func TestTaskworkerStartupFreshPassFollowsReleasedExpiryRetry(t *testing.T) {
	testTaskworkerStartupFreshPassFollowsReleasedExpiryClaim(t, false)
}

// The same with a successful finish, whose post schedules a new successor row.
func TestTaskworkerStartupFreshPassFollowsReleasedExpiryFinish(t *testing.T) {
	testTaskworkerStartupFreshPassFollowsReleasedExpiryClaim(t, true)
}

func testTaskworkerStartupFreshPassFollowsReleasedExpiryClaim(t *testing.T, finish bool) {
	runExpiryFreshPassEnv(t, func(t testing.TB, ctx context.Context) {
		retained := retainedExpirySweepArgs(server.NowUtc())
		id := seedExpiryFreshPassRow(t, ctx, startupFreshPassProbe, retained)
		seeded := readExpiryFreshPassRow(t, ctx)
		continuation := retainedExpirySweepContinuation(retained)
		ticks := make(chan time.Time)
		outcomes := make(chan task.RunOnceArgsReplacement, 4)
		startCtx := context.WithValue(ctx, closeExpiredFreshPassHooksKey{}, &closeExpiredFreshPassHooks{
			after:     func(time.Duration) <-chan time.Time { return ticks },
			attempted: func(_ int, outcome task.RunOnceArgsReplacement) { outcomes <- outcome },
		})
		// The run's goroutine records what the start left; the test asserts it.
		var inFlight expiryFreshPassRow
		finished, retried, evalErr := evalExpiryFreshPassProbe(ctx, func(*work.CloseExpiredContractsArgs) (*work.CloseExpiredContractsResult, error) {
			server.Raise(initTaskScheduleForProfile(startCtx, WorkloadProfileProduction))
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT task_id,args_json,claim_generation FROM pending_task WHERE run_once_key=$1`,
					task.RunOnce("close_expired_contracts_1_0").String()).Scan(&inFlight.id, &inFlight.raw, &inFlight.claim))
			})
			if finish {
				return &work.CloseExpiredContractsResult{Full: true, Sweep: continuation.Sweep, NextExpiration: continuation.NextExpiration}, nil
			}
			return nil, task.WithRetryArgs(errors.New("synthetic busy owner"), continuation)
		})
		if outcome := <-outcomes; outcome != task.RunOnceArgsLeased {
			t.Fatal("startup did not leave the leased row to its run in flight", outcome)
		}
		if inFlight.id != id || inFlight.raw != seeded.raw || inFlight.claim != seeded.claim+1 {
			t.Fatal("startup disturbed the args or claim of the run in flight", inFlight.raw)
		}
		handedBack := readExpiryFreshPassRow(t, ctx)
		if finish {
			var recorded string
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT args_json FROM finished_task WHERE task_id=$1`, id).Scan(&recorded))
			})
			if evalErr != nil || len(finished) != 1 || finished[0] != id || handedBack.id == id || recorded != seeded.raw {
				t.Fatal("the run in flight lost its own finish", finished, evalErr)
			}
		} else if evalErr != nil || len(retried) != 1 || retried[0] != id || handedBack.id != id || handedBack.errorCount != 1 {
			t.Fatal("the run in flight lost its own retry", retried, evalErr)
		}
		if handedBack.args.Sweep == nil || handedBack.args.Sweep.Catchup == nil ||
			!handedBack.args.Sweep.Catchup.Open.CreateTime.Equal(continuation.Sweep.Catchup.Open.CreateTime) {
			t.Fatal("the handback did not keep its own checkpoint", handedBack.raw)
		}
		select {
		case ticks <- server.NowUtc():
		case <-ctx.Done():
			t.Fatal("startup stopped retrying while the run was in flight", ctx.Err())
		}
		if outcome := <-outcomes; outcome != task.RunOnceArgsReplaced {
			t.Fatal("the retry after the handback did not replace the successor's args", outcome)
		}
		successor := readExpiryFreshPassRow(t, ctx)
		if successor.id != handedBack.id || !expiryArgsAreFreshPass(successor.args) || successor.claim != handedBack.claim+1 {
			t.Fatal("the successor of the released claim did not start a fresh pass", successor.raw)
		}
	})
}
