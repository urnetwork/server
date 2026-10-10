// Eligibility alarms use the live cursor and Run controller with explicit
// clocks and barriers. No short sleep or early financial execution proves them.
package task

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

// An elapsed boundary is consumed once, so a subsequent refused claim returns
// to the configured maximum instead of replaying an already-fired deadline.
func TestTaskClaimPollConsumesEligibilityWithoutExtendingBudget(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		availableAt time.Time
		want        time.Duration
	}{
		{want: 5 * time.Second},
		{availableAt: now.Add(2 * time.Second), want: 2 * time.Second},
		{availableAt: now.Add(20 * time.Second), want: 5 * time.Second},
		{availableAt: now, want: 0},
		{availableAt: now.Add(-time.Second), want: 0},
	} {
		poll := &taskClaimPoll{availableAt: c.availableAt}
		if got := poll.delay(now, 5*time.Second); got != c.want {
			t.Fatal("eligibility alarm changed its deadline or polling maximum", got, c.want)
		}
		if got := poll.delay(now, 5*time.Second); got != 5*time.Second {
			t.Fatal("consumed eligibility alarm could spin after an ordinary refusal", got)
		}
	}
}

// The real cursor ignores unregistered wrappers and saturated aliases before
// retaining the earliest future row. That row must never acquire any owner.
func TestTaskClaimPollRetainsScopedFutureWithoutAdmission(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Second).Add(2 * time.Minute)
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		allowed, limited := NewTaskTarget(claimProfileAllowed), NewTaskTarget(claimProfileExcluded)
		settings := DefaultTaskWorkerSettings()
		settings.ClaimRegisteredTargetsOnly = true
		settings.TargetClaimLimits = map[string]int{limited.TargetFunctionName(): 1}
		worker := NewTaskWorker(ctx, settings)
		defer worker.Close()
		worker.claimNow = func() time.Time { return now }
		worker.AddTargets(allowed, limited)
		reservation, admitted := worker.reserveTaskClaim(limited.TargetFunctionName())
		if !admitted || reservation == nil {
			t.Fatal("could not retain the exact target reservation")
		}
		defer reservation.release()
		limitedId := ScheduleTask(claimProfileExcluded, &claimProfileArgs{}, owner, RunAt(now))
		ScheduleTask(worker.RunPost, &RunPostArgs{TaskId: server.NewId()}, owner, RunAt(now))
		wanted := ScheduleTask(claimProfileAllowed, &claimProfileArgs{}, owner, RunAt(now.Add(time.Second)))
		leased := ScheduleTask(claimProfileAllowed, &claimProfileArgs{}, owner, RunAt(now.Add(-time.Hour)))
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET function_name=$2 WHERE task_id=$1`, limitedId,
				strings.Replace(limited.TargetFunctionName(), "/server/", "/server/v37/", 1)))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET function_name=$2 WHERE task_id=$1`, wanted,
				strings.Replace(allowed.TargetFunctionName(), "/server/", "/server/v37/", 1)))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET release_time=$2 WHERE task_id=$1`, leased, now.Add(3*time.Second)))
			rows := make([][]any, 4096)
			for index := range rows {
				rows[index] = []any{server.NewId(), allowed.TargetFunctionName(), `{}`, now.Add(time.Hour),
					DefaultPriority, int(DefaultMaxTime / time.Second), time.Time{}, time.Time{}}
			}
			_, err := tx.CopyFrom(ctx, pgx.Identifier{"pending_task"},
				[]string{"task_id", "function_name", "args_json", "run_at", "run_priority", "run_max_time_seconds", "claim_time", "release_time"}, pgx.CopyFromRows(rows))
			server.Raise(err)
			server.RaisePgResult(tx.Exec(ctx, `ANALYZE pending_task`))
		})
		var availableBlock int64
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT available_block FROM pending_task WHERE task_id=$1`, wanted).Scan(&availableBlock))
		})
		probeCount := 0
		worker.claimQueueAdmission = func(server.Id, bool) { probeCount++ }
		poll := &taskClaimPoll{}
		claimed, guard, _, err := worker.takeTasksWithGuard(ctx, 1, nil, taskClaimOptions{poll: poll})
		if guard != nil {
			defer guard.release()
		}
		if err != nil || guard != nil || len(claimed) != 0 || probeCount != 0 ||
			!poll.availableAt.Equal(time.Unix(availableBlock*BlockSizeSeconds, 0)) {
			t.Fatal("future discovery changed scope, lease eligibility or business admission", len(claimed), probeCount, poll.availableAt, err)
		}
		before := GetTasks(ctx, wanted)[wanted]
		if before == nil || !before.ClaimTime.IsZero() || before.ClaimGeneration != 0 {
			t.Fatal("a future wake hint became a claimed task")
		}
		// Direct finite evaluation retains its current-time predicate.
		claimed, directGuard, _, err := worker.takeTasksWithGuard(ctx, 1, nil, taskClaimOptions{})
		if directGuard != nil {
			defer directGuard.release()
		}
		if err != nil || directGuard != nil || len(claimed) != 0 || probeCount != 0 {
			t.Fatal("finite evaluation borrowed the Run lookahead", len(claimed), probeCount, err)
		}
		// Both cached-plan policies must retain the real ordered index. The
		// future suffix lies beyond the unchanged five-second polling horizon.
		query, args := worker.claimOwnershipCandidatesQuery(now.Add(settings.PollTimeout).Unix()/BlockSizeSeconds, 65, true)
		for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
			server.MaintenanceDb(ctx, func(conn server.PgConn) {
				tx, err := conn.Begin(ctx)
				server.Raise(err)
				defer tx.Rollback(ctx)
				server.RaisePgResult(tx.Exec(ctx, `SET LOCAL plan_cache_mode = `+mode))
				var raw []byte
				server.Raise(tx.QueryRow(ctx, `EXPLAIN (FORMAT JSON) DECLARE pending_task_poll_control NO SCROLL CURSOR FOR `+query, args...).Scan(&raw))
				var plans []struct {
					Plan taskClaimQueryTestPlan `json:"Plan"`
				}
				server.Raise(json.Unmarshal(raw, &plans))
				streaming := false
				var walk func(taskClaimQueryTestPlan)
				walk = func(plan taskClaimQueryTestPlan) {
					if plan.NodeType == "Sort" || plan.NodeType == "Incremental Sort" || plan.NodeType == "Materialize" {
						t.Fatal("future cursor materialized the queue", mode, string(raw))
					}
					streaming = streaming || plan.IndexName == "pending_task_poll_order"
					for _, child := range plan.Plans {
						walk(child)
					}
				}
				if len(plans) != 1 {
					t.Fatal("missing future cursor plan", mode)
				}
				walk(plans[0].Plan)
				if !streaming {
					t.Fatal("future cursor lost its bounded ordered index", mode, string(raw))
				}
			})
		}
	})
}

// A real claimed sibling holds its slot until the controller's actual alarm
// has been inspected. Every successful result still uses ordinary handback.
type taskClaimPollHeldTarget struct {
	Target
	started chan struct{}
	release chan struct{}
}

func (self *taskClaimPollHeldTarget) Run(ctx context.Context, queued *Task) (any, func(server.PgTx) ([]server.PostFunction, error), error) {
	close(self.started)
	select {
	case <-self.release:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
	return self.Target.Run(ctx, queued)
}

// Stop at the real poll arm, before any alarm can fire. A fixed virtual clock
// makes the required two-second interval an exact value, not a timing guess.
func taskClaimPollRunControl(t *testing.T, holdSibling bool) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		now := server.NowUtc().Truncate(time.Second).Add(2 * time.Minute)
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		settings := DefaultTaskWorkerSettings()
		settings.ClaimRegisteredTargetsOnly = true
		worker := NewTaskWorker(ctx, settings)
		defer worker.Close()
		worker.claimNow = func() time.Time { return now }
		worker.heartbeatNow = func() time.Time { return now }
		worker.AddTargets(NewTaskTarget(claimProfileAllowed))
		future := ScheduleTask(claimProfileAllowed, &claimProfileArgs{}, owner, RunAt(now.Add(time.Second)))
		held := &taskClaimPollHeldTarget{Target: NewTaskTarget(claimProfileExcluded), started: make(chan struct{}), release: make(chan struct{})}
		var heldId server.Id
		if holdSibling {
			worker.AddTargets(held)
			heldId = ScheduleTask(claimProfileExcluded, &claimProfileArgs{}, owner, RunAt(now.Add(-time.Hour)))
			// Fill the original four slots so only a later real refill can
			// discover the future row while this sibling keeps its owner.
			for range settings.BatchSize - 1 {
				ScheduleTask(claimProfileAllowed, &claimProfileArgs{}, owner, RunAt(now.Add(-time.Hour)))
			}
		}
		armed := make(chan time.Duration, 1)
		continuePoll, never := make(chan struct{}), make(chan time.Time)
		worker.pollAfter = func(delay time.Duration) <-chan time.Time {
			if holdSibling {
				select {
				case <-held.started:
				case <-ctx.Done():
					server.Raise(ctx.Err())
				}
			}
			armed <- delay
			<-continuePoll
			return never
		}
		done := make(chan struct{})
		var runErr any
		go func() {
			defer close(done)
			runErr = server.HandleError(worker.Run)
		}()
		var once sync.Once
		stop := func() {
			once.Do(func() {
				worker.runCancel()
				close(held.release)
				close(continuePoll)
			})
		}
		defer func() {
			stop()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Error("eligibility control failed to join its real Run owner")
			}
		}()
		select {
		case delay := <-armed:
			if delay != 2*time.Second {
				t.Fatal("Run ignored an observed eligibility boundary", holdSibling, delay)
			}
		case <-ctx.Done():
			t.Fatal("Run did not reach its owned eligibility alarm", ctx.Err())
		}
		pending := GetTasks(ctx, future)[future]
		if pending == nil || !pending.ClaimTime.IsZero() || pending.ClaimGeneration != 0 {
			t.Fatal("Run executed a future task before its actual eligibility boundary")
		}
		stop()
		select {
		case <-done:
		case <-ctx.Done():
			t.Fatal("Run did not join after stopping eligibility admission", ctx.Err())
		}
		if runErr != nil || worker.InflightCount() != 0 || (holdSibling && GetFinishedTasks(ctx, heldId)[heldId] == nil) {
			t.Fatal("eligibility observation lost held-sibling handback", runErr, worker.InflightCount())
		}
	})
}

func TestTaskRunEmptyPollUsesObservedEligibility(t *testing.T) {
	taskClaimPollRunControl(t, false)
}

func TestTaskRunRefillPollUsesObservedEligibility(t *testing.T) {
	taskClaimPollRunControl(t, true)
}
