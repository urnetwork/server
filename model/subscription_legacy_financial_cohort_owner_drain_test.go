// Finite diagnostic drains distinguish queued owners from currently eligible
// owners using the same stored availability block and client clock as claims.
package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/task"
)

// Timestamp claims are not proof of a live advisory owner. They must be absent
// in this isolated, single-worker fixture before a future-availability wait.
type legacyFinancialDrainState struct {
	NowBlock        int64 `json:"client_now_block"`
	Pending         int   `json:"pending"`
	Eligible        int   `json:"eligible"`
	Future          int   `json:"future"`
	TimestampClaims int   `json:"future_timestamp_claims"`
	Rescheduled     int   `json:"owners_with_retry_errors"`
	MinBlock        int64 `json:"min_available_block"`
	MaxBlock        int64 `json:"max_available_block"`
}

// All observations and actual waiting remain in the owner-drain denominator.
type legacyFinancialDrainResult struct {
	Initial      int                         `json:"initial_pending"`
	Finished     int                         `json:"finished"`
	Remaining    int                         `json:"remaining"`
	Claims       int                         `json:"claim_calls"`
	EmptyClaims  int                         `json:"empty_claim_calls"`
	FutureWaits  int                         `json:"future_waits"`
	FutureWaitNs int64                       `json:"future_wait_ns"`
	BeforeEmpty  []legacyFinancialDrainState `json:"before_empty,omitempty"`
	AfterEmpty   []legacyFinancialDrainState `json:"after_empty,omitempty"`
}

// Capture the cutoff before the read and subsequent actual EvalTasks call. A
// later clock tick may make an after-empty row eligible; it cannot retroactively
// prove that row was eligible during the preceding claim.
func legacyFinancialDrainSnapshot(ctx context.Context, names []string) legacyFinancialDrainState {
	now := server.NowUtc()
	state := legacyFinancialDrainState{NowBlock: now.Unix() / task.BlockSizeSeconds}
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT count(*),
            count(*) FILTER (WHERE available_block <= $2),
            count(*) FILTER (WHERE available_block > $2),
            count(*) FILTER (WHERE release_time > $3),
            count(*) FILTER (WHERE reschedule_error_count > 0),
            COALESCE(min(available_block),0),COALESCE(max(available_block),0)
            FROM pending_task WHERE function_name=ANY($1)`, names, state.NowBlock, now).Scan(
			&state.Pending, &state.Eligible, &state.Future, &state.TimestampClaims, &state.Rescheduled, &state.MinBlock, &state.MaxBlock))
	})
	return state
}

// Only a known future-before-claim empty result may wait. Ready-but-unclaimed,
// retrying, leased or changing owners remain a failure. The optional observation
// hook is test-only and cannot replace the real claim or alter the money path.
func legacyFinancialDrainOwners(t testing.TB, ctx context.Context, expected int, onFuture func(legacyFinancialDrainState)) (result legacyFinancialDrainResult, returnErr error) {
	t.Helper()
	server.HandleError(func() {
		names := []string{task.NewTaskTarget(ApplyLegacyProviderTotals).TargetFunctionName(), task.NewTaskTarget(ApplyLegacyNetEscrowMirror).TargetFunctionName()}
		state := legacyFinancialDrainSnapshot(ctx, names)
		result.Initial, result.Remaining = state.Pending, state.Pending
		if state.Pending != expected {
			returnErr = fmt.Errorf("durable output cardinality changed: pending=%d expected=%d", state.Pending, expected)
			return
		}
		settings := task.DefaultTaskWorkerSettings()
		settings.ClaimRegisteredTargetsOnly = true
		worker := task.NewTaskWorker(ctx, settings)
		defer worker.Close()
		worker.AddTargets(NewLegacyProviderTotalsTaskTarget(), task.NewTaskTargetWithPost(ApplyLegacyNetEscrowMirror, ApplyLegacyNetEscrowMirrorPost))
		finishedIds := map[server.Id]bool{}
		for state.Pending > 0 {
			if result.Claims >= 2*expected+8 {
				returnErr = errors.New("finite durable owner drain exhausted its claim bound")
				return
			}
			before := state
			result.Claims++
			done, retried, postRetried, err := worker.EvalTasks(min(64, before.Pending))
			if err != nil || len(retried) != 0 || len(postRetried) != 0 {
				returnErr = fmt.Errorf("durable output failed: finished=%d retry=%d post_retry=%d error=%v", len(done), len(retried), len(postRetried), err)
				return
			}
			state = legacyFinancialDrainSnapshot(ctx, names)
			result.Remaining = state.Pending
			if len(done) == 0 {
				result.EmptyClaims++
				result.BeforeEmpty = append(result.BeforeEmpty, before)
				result.AfterEmpty = append(result.AfterEmpty, state)
				raw, err := json.Marshal(map[string]any{"before_claim": before, "after_empty_claim": state})
				server.Raise(err)
				t.Logf("legacy_financial_owner_empty_claim=%s", raw)
				if before.Eligible != 0 || before.Future != before.Pending || before.MinBlock <= before.NowBlock ||
					before.TimestampClaims != 0 || before.Rescheduled != 0 || state.Pending != before.Pending ||
					state.MinBlock != before.MinBlock || state.MaxBlock != before.MaxBlock || state.TimestampClaims != 0 || state.Rescheduled != 0 {
					returnErr = errors.New("empty owner claim lacked stable unleased future availability")
					return
				}
				result.FutureWaits++
				if onFuture != nil {
					onFuture(before)
				}
				if err := ctx.Err(); err != nil {
					returnErr = err
					return
				}
				until := time.Unix(before.MinBlock*task.BlockSizeSeconds, 0)
				if delay := until.Sub(server.NowUtc()); delay > 0 {
					started := time.Now()
					select {
					case <-time.After(delay):
					case <-ctx.Done():
						returnErr = ctx.Err()
					}
					result.FutureWaitNs += time.Since(started).Nanoseconds()
					if returnErr != nil {
						return
					}
				}
				state = legacyFinancialDrainSnapshot(ctx, names)
				continue
			}
			for _, id := range done {
				if finishedIds[id] {
					returnErr = errors.New("owner finalized twice within the fixed drain")
					return
				}
				finishedIds[id] = true
			}
			result.Finished += len(done)
			if state.Pending != before.Pending-len(done) || state.Rescheduled != 0 {
				returnErr = errors.New("owner finalization changed the fixed durable cardinality")
				return
			}
		}
		if result.Finished != expected {
			returnErr = fmt.Errorf("durable owner completion differs: finished=%d expected=%d", result.Finished, expected)
		}
	}, func(err error) { returnErr = err })
	return
}

// Force the original pending-but-not-eligible condition with synthetic scheduling
// metadata at insertion. No timestamp is rewritten after ownership exists. The
// real worker must claim nothing; the tested future-wait observation cancels its
// own context, making the classification proof independent of sleeps or ticks.
func TestLegacyFinancialCohortFutureOwnerWaitIsNotLoss(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		f := legacyFinancialCohortSeed(t, ctx, 8)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `CREATE FUNCTION synthetic_future_financial_output() RETURNS trigger LANGUAGE plpgsql AS $$
                BEGIN NEW.run_at := clock_timestamp() AT TIME ZONE 'UTC' + interval '1 day'; RETURN NEW; END $$;
                CREATE TRIGGER synthetic_future_financial_output BEFORE INSERT ON pending_task
                FOR EACH ROW EXECUTE FUNCTION synthetic_future_financial_output()`))
		})
		beforeCounter := contractClosedCounter.Snapshot()
		page, err := FlushLegacySettlements(ctx, 1, nil, 8)
		afterCounter := contractClosedCounter.Snapshot()
		if err != nil || page.Completed != 8 || page.Visited != 8 || page.Failed != 0 || page.BusyOrGone != 0 {
			t.Fatal("synthetic future output changed ordinary settlement", page, err)
		}
		if !beforeCounter.Stable || !afterCounter.Stable || afterCounter.Confirmed-beforeCounter.Confirmed != 8 || afterCounter.Uncertain != beforeCounter.Uncertain || afterCounter.Untracked != beforeCounter.Untracked {
			t.Fatal("future output changed the committed financial count", beforeCounter, afterCounter)
		}
		legacyFinancialCohortRequire(t, ctx, f, legacyFinancialCohortCompleted(f.ids))
		waitCtx, cancelWait := context.WithCancel(ctx)
		defer cancelWait()
		observed := 0
		drain, drainErr := legacyFinancialDrainOwners(t, waitCtx, 9, func(state legacyFinancialDrainState) {
			observed++
			cancelWait()
		})
		if ctx.Err() != nil {
			t.Fatal("future-owner control canceled its financial parent", ctx.Err())
		}
		legacyFinancialCohortRequire(t, ctx, f, legacyFinancialCohortCompleted(f.ids))
		if replay, err := FlushLegacySettlements(ctx, 1, nil, 8); err != nil || replay.Visited != 0 || replay.Completed != 0 {
			t.Fatal("future owner changed financial replay", replay, err)
		}
		legacyFinancialCohortRequire(t, ctx, f, legacyFinancialCohortCompleted(f.ids))
		names := []string{task.NewTaskTarget(ApplyLegacyProviderTotals).TargetFunctionName(), task.NewTaskTarget(ApplyLegacyNetEscrowMirror).TargetFunctionName()}
		retained := legacyFinancialDrainSnapshot(ctx, names)
		if retained.Pending != 9 || retained.Eligible != 0 || retained.Future != 9 || retained.TimestampClaims != 0 || retained.Rescheduled != 0 {
			t.Fatal("empty claim lost or changed future durable owners", retained)
		}
		raw, err := json.Marshal(map[string]any{"drain": drain, "retained": retained,
			"qualifier": "Synthetic future scheduling proves the empty-claim mechanism; it does not establish the prior serial failure's unretained eligibility state."})
		server.Raise(err)
		t.Logf("legacy_financial_future_owner_control=%s", raw)
		if !errors.Is(drainErr, context.Canceled) || observed != 1 || drain.Initial != 9 || drain.Remaining != 9 || drain.Finished != 0 || drain.Claims != 1 || drain.EmptyClaims != 1 || drain.FutureWaits != 1 {
			t.Fatal("pending future owners were treated as lost instead of reaching their bounded wait", drain, drainErr, observed)
		}
	})
}
