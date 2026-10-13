// Every start puts a retained expiry sweep on a fresh pass, never over a run
// in flight, never under a lapsed handback, and once per rolling restart.
package taskworker

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
	"github.com/urnetwork/server/taskworker/work"
)

type expiryFreshPassFunction = task.TaskFunction[*work.CloseExpiredContractsArgs, *work.CloseExpiredContractsResult]

// The retained 2026-10-13 shape, with synthetic times and identities: both
// backlog lanes done, the catch-up lane mid-pass days behind, the fresh window
// near the head, and an epoch nine days old.
func retainedExpirySweepArgs(now time.Time) *work.CloseExpiredContractsArgs {
	position := func(at time.Time) *model.ContractExpiryPosition {
		return &model.ContractExpiryPosition{CreateTime: at, ContractId: server.NewId()}
	}
	epoch := now.Add(-9 * 24 * time.Hour)
	freshBefore := now.Add(-time.Hour)
	catchupAfter := epoch.Add(2*24*time.Hour + 15*time.Hour)
	catchupBefore := catchupAfter.Add(21 * time.Hour)
	nextExpiration := now.Add(-15 * time.Hour)
	return &work.CloseExpiredContractsArgs{
		BlockSize:  1,
		BlockIndex: 0,
		Sweep: &model.ContractExpirySweepCursor{
			RecentAfter:    epoch,
			HistoricalDone: true,
			HistoricalNext: true,
			Fresh: &model.ContractExpiryCursor{
				ScanBefore:  freshBefore,
				Open:        position(freshBefore.Add(-2 * time.Minute)),
				Dispute:     position(freshBefore.Add(-time.Hour)),
				DisputeDone: true,
			},
			FreshBefore: freshBefore.Add(-49 * time.Minute),
			BacklogDone: true,
			Catchup: &model.ContractExpiryCursor{
				ScanBefore:  catchupBefore,
				Open:        position(catchupBefore.Add(-6 * time.Hour)),
				Dispute:     position(catchupBefore.Add(-3 * time.Hour)),
				DisputeDone: true,
			},
			CatchupAfter:   catchupAfter,
			CatchupBefore:  catchupAfter.Add(3 * time.Hour),
			CatchupChecked: catchupBefore.Add(8 * time.Hour),
		},
		NextExpiration: &nextExpiration,
	}
}

// What a run of the retained pass would hand back: the same epoch with its
// catch-up lane a little further along.
func retainedExpirySweepContinuation(retained *work.CloseExpiredContractsArgs) *work.CloseExpiredContractsArgs {
	sweep := *retained.Sweep
	catchup := *sweep.Catchup
	catchup.Open = &model.ContractExpiryPosition{CreateTime: catchup.Open.CreateTime.Add(time.Minute), ContractId: server.NewId()}
	sweep.Catchup = &catchup
	return &work.CloseExpiredContractsArgs{BlockSize: 1, BlockIndex: 0, Sweep: &sweep, NextExpiration: retained.NextExpiration}
}

// The sweep is at a fresh pass: no cursor, no lane state, no retained hint.
func expiryArgsAreFreshPass(args work.CloseExpiredContractsArgs) bool {
	return args.BlockSize == 1 && args.BlockIndex == 0 && args.Cursor == nil && args.Sweep == nil && args.NextExpiration == nil
}

type expiryFreshPassRow struct {
	id         server.Id
	raw        string
	args       work.CloseExpiredContractsArgs
	claim      int64
	generation int64
	errorCount int
}

// The singleton's one pending row under its production key.
func readExpiryFreshPassRow(t testing.TB, ctx context.Context) (row expiryFreshPassRow) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		var count int
		server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM pending_task WHERE run_once_key=$1`,
			task.RunOnce("close_expired_contracts_1_0").String()).Scan(&count))
		if count != 1 {
			t.Fatal("the expiry sweep does not have exactly one pending owner", count)
		}
		server.Raise(conn.QueryRow(ctx, `SELECT task_id,args_json,claim_generation,run_once_generation,reschedule_error_count
			FROM pending_task WHERE run_once_key=$1`, task.RunOnce("close_expired_contracts_1_0").String()).Scan(
			&row.id, &row.raw, &row.claim, &row.generation, &row.errorCount))
	})
	if json.Unmarshal([]byte(row.raw), &row.args) != nil {
		t.Fatal("the expiry sweep row holds unreadable arguments", row.raw)
	}
	return
}

// Seeds the production key due now, under the given function.
func seedExpiryFreshPassRow(t testing.TB, ctx context.Context, function expiryFreshPassFunction, args *work.CloseExpiredContractsArgs) server.Id {
	t.Helper()
	owner := session.NewLocalClientSession(ctx, "", nil)
	defer owner.Cancel()
	var id server.Id
	server.Tx(ctx, func(tx server.PgTx) {
		id = task.ScheduleTaskInTx(tx, function, args, owner, task.RunOnce("close_expired_contracts_1_0"),
			task.RunAt(time.Unix(1, 0).UTC()), task.MaxTime(30*time.Minute), task.Priority(task.TaskPriorityFastest))
	}, server.TxReadCommitted, server.OptNoRetry())
	return id
}

// Every startup test runs the real initializer with the shared global settings.
func runExpiryFreshPassEnv(t *testing.T, run func(t testing.TB, ctx context.Context)) {
	t.Setenv("WARP_DOMAIN", "startup-expiry-fresh-pass.example")
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		controller.SetStConfig(&controller.StConfig{Enabled: false})
		defer controller.SetStConfig(nil)
		controller.SetVerifySettings(model.DefaultVerifySettings())
		defer controller.SetVerifySettings(nil)
		run(t, ctx)
	})
}

// The production 2026-10-13 case: a pending row whose pass began nine days ago
// keeps its lanes through every RunOnce merge. Startup must replace them.
func TestTaskworkerStartupStartsRetainedExpirySweepOnFreshPass(t *testing.T) {
	testTaskworkerStartupStartsRetainedExpirySweepOnFreshPass(t, WorkloadProfileProduction)
}

// The scoped operator schedules the same sweep and resets it the same way.
func TestTaskworkerSubnetStartupStartsRetainedExpirySweepOnFreshPass(t *testing.T) {
	testTaskworkerStartupStartsRetainedExpirySweepOnFreshPass(t, WorkloadProfileSubnetOperator)
}

func testTaskworkerStartupStartsRetainedExpirySweepOnFreshPass(t *testing.T, profile WorkloadProfile) {
	runExpiryFreshPassEnv(t, func(t testing.TB, ctx context.Context) {
		id := seedExpiryFreshPassRow(t, ctx, work.CloseExpiredContracts, retainedExpirySweepArgs(server.NowUtc()))
		before := readExpiryFreshPassRow(t, ctx)
		server.Raise(initTaskScheduleForProfile(ctx, profile))
		after := readExpiryFreshPassRow(t, ctx)
		if after.id != id || !expiryArgsAreFreshPass(after.args) {
			t.Fatal("startup kept the retained sweep instead of starting a fresh pass", after.raw)
		}
		if after.claim != before.claim+1 {
			t.Fatal("the reset did not advance the claim generation that fences a lapsed handback", before.claim, after.claim)
		}
	})
}

// Six containers restart one after another. The first resets the retained
// pass; its first run records a new epoch, and every later start keeps it.
func TestTaskworkerRollingRestartStartsExpirySweepOnce(t *testing.T) {
	runExpiryFreshPassEnv(t, func(t testing.TB, ctx context.Context) {
		id := seedExpiryFreshPassRow(t, ctx, work.CloseExpiredContracts, retainedExpirySweepArgs(server.NowUtc()))
		server.Raise(initTaskScheduleForProfile(ctx, WorkloadProfileProduction))
		reset := readExpiryFreshPassRow(t, ctx)
		if reset.id != id || !expiryArgsAreFreshPass(reset.args) {
			t.Fatal("the first start of the rolling restart kept the retained sweep", reset.raw)
		}
		// The fresh pass's first run hands back its new epoch and cursors.
		epoch := server.NowUtc().Add(-time.Minute)
		historical := &model.ContractExpiryCursor{ScanBefore: epoch, DisputeDone: true,
			Open: &model.ContractExpiryPosition{CreateTime: epoch.Add(-6 * 24 * time.Hour), ContractId: server.NewId()}}
		progressed, err := json.Marshal(&work.CloseExpiredContractsArgs{BlockSize: 1, BlockIndex: 0, Cursor: historical,
			Sweep: &model.ContractExpirySweepCursor{Historical: historical, RecentAfter: epoch, HistoricalNext: false}})
		server.Raise(err)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET args_json=$2 WHERE task_id=$1`, id, string(progressed)))
		}, server.TxReadCommitted, server.OptNoRetry())
		for range 5 {
			server.Raise(initTaskScheduleForProfile(ctx, WorkloadProfileProduction))
		}
		after := readExpiryFreshPassRow(t, ctx)
		if after.id != id || after.raw != string(progressed) || after.claim != reset.claim {
			t.Fatal("a later start of the same rolling restart reset the new pass again", after.raw)
		}
	})
}

type startupFreshPassProbeKey struct{}

// Stands in for the expiry sweep under its production key. Each test supplies
// the run through the worker's context.
func startupFreshPassProbe(args *work.CloseExpiredContractsArgs, clientSession *session.ClientSession) (*work.CloseExpiredContractsResult, error) {
	run := clientSession.Ctx.Value(startupFreshPassProbeKey{}).(func(*work.CloseExpiredContractsArgs) (*work.CloseExpiredContractsResult, error))
	return run(args)
}

// Claims, runs and hands back through the ordinary worker. A refused handback
// raises out of EvalTasks; it is returned as the error.
func evalExpiryFreshPassProbe(ctx context.Context, run func(*work.CloseExpiredContractsArgs) (*work.CloseExpiredContractsResult, error)) (finished, retried []server.Id, evalErr error) {
	settings := task.DefaultTaskWorkerSettings()
	settings.ClaimRegisteredTargetsOnly = true
	worker := task.NewTaskWorker(context.WithValue(ctx, startupFreshPassProbeKey{}, run), settings)
	defer worker.Close()
	worker.AddTargets(task.NewTaskTargetWithPost(startupFreshPassProbe, work.CloseExpiredContractsPost))
	server.HandleError(func() {
		finished, retried, _, evalErr = worker.EvalTasks(1)
	}, func(err error) { evalErr = err })
	return
}

// The retained run's worker lost its lease timestamp but still runs, as when
// refreshes fail for longer than the lease. A new container's start sees an
// unleased row and resets it. The stale handback must not land over the reset.
func TestTaskworkerStartupRefusesLapsedExpiryRetry(t *testing.T) {
	testTaskworkerStartupRefusesLapsedExpiryHandback(t, false)
}

// The same lapsed worker finishing with a post that would schedule its cursor.
func TestTaskworkerStartupRefusesLapsedExpiryFinish(t *testing.T) {
	testTaskworkerStartupRefusesLapsedExpiryHandback(t, true)
}

func testTaskworkerStartupRefusesLapsedExpiryHandback(t *testing.T, finish bool) {
	runExpiryFreshPassEnv(t, func(t testing.TB, ctx context.Context) {
		retained := retainedExpirySweepArgs(server.NowUtc())
		id := seedExpiryFreshPassRow(t, ctx, startupFreshPassProbe, retained)
		continuation := retainedExpirySweepContinuation(retained)
		ran := false
		_, _, evalErr := evalExpiryFreshPassProbe(ctx, func(*work.CloseExpiredContractsArgs) (*work.CloseExpiredContractsResult, error) {
			ran = true
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET release_time=$2 WHERE task_id=$1`, id, server.NowUtc().Add(-time.Second)))
			}, server.TxReadCommitted, server.OptNoRetry())
			server.Raise(initTaskScheduleForProfile(ctx, WorkloadProfileProduction))
			if finish {
				return &work.CloseExpiredContractsResult{Full: true, Sweep: continuation.Sweep, NextExpiration: continuation.NextExpiration}, nil
			}
			return nil, task.WithRetryArgs(errors.New("synthetic lapsed owner"), continuation)
		})
		if !ran {
			t.Fatal("the lapsed owner never ran", evalErr)
		}
		row := readExpiryFreshPassRow(t, ctx)
		if !expiryArgsAreFreshPass(row.args) {
			t.Fatal("a worker whose lease lapsed handed back its retained cursor over the fresh pass", row.raw, evalErr)
		}
		var finishedCount int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM finished_task WHERE task_id=$1`, id).Scan(&finishedCount))
		})
		if row.id != id || row.errorCount != 0 || finishedCount != 0 || evalErr == nil {
			t.Fatal("the refused handback still committed", row.id == id, row.errorCount, finishedCount, evalErr)
		}
	})
}
