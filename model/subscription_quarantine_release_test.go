// A quarantine's Redis release must survive the loss of the release after its
// commit, without giving the quarantine settlement authority: its escrow stays
// unsettled and no debit journal row appears.
package model

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// Fails the single-command release of one contract's reservation token, as an
// unstable Redis does. Installed on the deadline-bound pool that reservation
// releases use; the debit worker's pipelined release passes through.
type lostReservationReleaseHook struct {
	key        string
	contractId string
	enabled    atomic.Bool
	hits       atomic.Int64
}

func (self *lostReservationReleaseHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (self *lostReservationReleaseHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (self *lostReservationReleaseHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, command redis.Cmder) error {
		args := command.Args()
		if self.enabled.Load() && command.Name() == "eval" && len(args) > 3 {
			keyCount, ok := args[2].(int)
			if ok && keyCount > 0 && len(args) > 4+keyCount && args[3] == self.key && args[4+keyCount] == self.contractId &&
				(args[3+keyCount] == "release" || args[3+keyCount] == "settle") {
				self.hits.Add(1)
				err := errors.New("synthetic Redis failure of a reservation release")
				command.SetErr(err)
				return err
			}
		}
		return next(ctx, command)
	}
}

func installLostReservationReleaseHook(ctx context.Context, balanceId, contractId server.Id) *lostReservationReleaseHook {
	hook := &lostReservationReleaseHook{key: redisContractReservationKeys(balanceId)[0], contractId: contractId.String()}
	installCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	server.Raise(server.RedisWithDeadline(installCtx, func(r server.RedisClient) error {
		r.AddHook(hook)
		return nil
	}))
	return hook
}

// Quarantines the fixture's malformed claim while Redis fails its release, so
// the commit stands with the reservation still held.
func quarantineWithLostRelease(t testing.TB, ctx context.Context, fixture *forceCloseDisputeFixture, escrow ByteCount) *lostReservationReleaseHook {
	t.Helper()
	original := fixture.state(t, ctx)
	hook := installLostReservationReleaseHook(ctx, fixture.balanceId, fixture.contractId)
	hook.enabled.Store(true)
	t.Cleanup(func() { hook.enabled.Store(false) })
	_, closeErr := ForceCloseOpenContractIds(ctx, fixture.cutoff, 10, 1, 1, 0)
	if closeErr == nil || !errors.Is(closeErr, errContractInsufficientEscrow) || hook.hits.Load() == 0 {
		t.Fatal("the quarantine and its lost release were not exercised", closeErr, hook.hits.Load())
	}
	quarantined := fixture.state(t, ctx)
	if quarantined.outcome != ContractOutcomeSettled || quarantined.open || quarantined.dispute ||
		quarantined.requestTokenByteCount != escrow || quarantined.redisEscrowByteCount != escrow ||
		quarantined.payerBalanceByteCount != original.payerBalanceByteCount {
		t.Fatalf("quarantine did not commit with its reservation still held: %+v", quarantined)
	}
	requireQuarantineWithoutSettlement(t, ctx, fixture)
	return hook
}

// The quarantine invariant: no escrow settled or paid, and no debit journal row.
func requireQuarantineWithoutSettlement(t testing.TB, ctx context.Context, fixture *forceCloseDisputeFixture) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		var settled, journals int
		server.Raise(conn.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM transfer_escrow WHERE contract_id=$1 AND (settled OR COALESCE(payout_byte_count,0)<>0)),
			(SELECT count(*) FROM transfer_debit_journal WHERE contract_id=$1)`, fixture.contractId).Scan(&settled, &journals))
		if settled != 0 || journals != 0 {
			t.Fatal("quarantine gained settlement authority", settled, journals)
		}
	})
}

// Returns the one pending release for the contract, or fails when the
// quarantine committed none.
func requireQuarantineReleaseTask(t testing.TB, ctx context.Context, contractId server.Id) server.Id {
	t.Helper()
	var taskIds []server.Id
	var functionNames []string
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `SELECT task_id, function_name FROM pending_task WHERE run_once_key=$1`,
			quarantineReservationReleaseRunOnce(contractId).String())
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var taskId server.Id
				var functionName string
				server.Raise(rows.Scan(&taskId, &functionName))
				taskIds = append(taskIds, taskId)
				functionNames = append(functionNames, functionName)
			}
		})
	})
	if len(taskIds) != 1 {
		t.Fatal("the quarantine did not commit exactly one durable release; a lost release holds the reservation until its lease ends", len(taskIds))
	}
	if functionNames[0] != NewQuarantineReservationReleaseTaskTarget().TargetFunctionName() {
		t.Fatal("the durable release has another function", functionNames[0])
	}
	return taskIds[0]
}

// Lets the worker claim the task now, as if its delay and its last claim's
// release had passed.
func makeQuarantineReleaseDue(ctx context.Context, taskId server.Id) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET run_at=$2, release_time=$2 WHERE task_id=$1`,
			taskId, server.NowUtc().Add(-time.Minute)))
	})
}

// The release after the quarantine commit is lost to a Redis failure. The
// durable task committed with the quarantine retries with backoff while Redis
// still fails, then releases the reservation once Redis recovers and deletes
// itself. The payer is never charged and the escrow is never settled.
func TestQuarantineReleaseTaskReleasesAfterLostPost(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		const escrow = ByteCount(32 * 1024 * 1024)
		fixture := newForceCloseDisputeFixture(t, ctx, true, true, 2*escrow, 2*escrow, escrow)
		original := fixture.state(t, ctx)
		hook := quarantineWithLostRelease(t, ctx, fixture, escrow)
		taskId := requireQuarantineReleaseTask(t, ctx, fixture.contractId)
		// The fixture backdates its contract to a fixed close cutoff, but its
		// token was reserved just now. Match a real close two hours after
		// creation, well inside the token's lease.
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=$2 WHERE contract_id=$1`,
				fixture.contractId, server.NowUtc().Add(-2*time.Hour)))
		})
		// A repeat queues no second row.
		server.Tx(ctx, func(tx server.PgTx) {
			queueQuarantineReservationReleaseInTx(ctx, tx, fixture.contractId)
		}, server.TxReadCommitted, server.OptNoRetry())
		if repeated := requireQuarantineReleaseTask(t, ctx, fixture.contractId); repeated != taskId {
			t.Fatal("a repeat replaced the durable release", taskId, repeated)
		}

		settings := task.DefaultTaskWorkerSettings()
		settings.ClaimRegisteredTargetsOnly = true
		worker := task.NewTaskWorker(ctx, settings)
		defer worker.Close()
		worker.AddTargets(NewQuarantineReservationReleaseTaskTarget())

		// Redis still fails: the task stays pending and backs off.
		makeQuarantineReleaseDue(ctx, taskId)
		hits := hook.hits.Load()
		before := server.NowUtc()
		finished, rescheduled, posts, err := worker.EvalTasks(1)
		if err != nil || len(finished) != 0 || len(posts) != 0 || len(rescheduled) != 1 || rescheduled[0] != taskId || hook.hits.Load() == hits {
			t.Fatal("a failed release did not retry later", finished, rescheduled, posts, err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var runAt time.Time
			var errorCount int
			server.Raise(conn.QueryRow(ctx, `SELECT run_at, reschedule_error_count FROM pending_task WHERE task_id=$1`, taskId).Scan(&runAt, &errorCount))
			if errorCount != 1 || runAt.Before(before.Add(quarantineReservationReleaseRetryBase)) {
				t.Fatal("a failed release would retry hot", errorCount, runAt.Sub(before))
			}
		})
		if held := fixture.state(t, ctx); held.requestTokenByteCount != escrow || held.redisEscrowByteCount != escrow {
			t.Fatalf("the reservation changed while Redis failed: %+v", held)
		}

		// Redis recovers: the next run releases the reservation and the task
		// deletes itself.
		hook.enabled.Store(false)
		makeQuarantineReleaseDue(ctx, taskId)
		finished, rescheduled, posts, err = worker.EvalTasks(1)
		if err != nil || len(finished) != 1 || finished[0] != taskId || len(rescheduled)+len(posts) != 0 {
			t.Fatal("the release did not complete once Redis recovered", finished, rescheduled, posts, err)
		}
		if remaining := task.GetTasks(ctx, taskId); len(remaining) != 0 {
			t.Fatal("the completed release kept its pending row")
		}
		released := fixture.state(t, ctx)
		if released.requestTokenByteCount != 0 || released.redisEscrowByteCount != 0 || released.netEscrowByteCount != 0 ||
			released.payerBalanceByteCount != original.payerBalanceByteCount ||
			released.providerPayoutByteCount != 0 || released.providerEarnedByteCount != 0 {
			t.Fatalf("the durable release did not free the reservation without a charge: %+v", released)
		}
		requireQuarantineWithoutSettlement(t, ctx, fixture)
	})
}

// Redis drops a token when its lease ends, so a release that has failed past
// the lease horizon completes without another Redis call.
func TestQuarantineReleaseTaskEndsAfterLease(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		const escrow = ByteCount(32 * 1024 * 1024)
		fixture := newForceCloseDisputeFixture(t, ctx, true, true, 2*escrow, 2*escrow, escrow)
		hook := quarantineWithLostRelease(t, ctx, fixture, escrow)
		requireQuarantineReleaseTask(t, ctx, fixture.contractId)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=$2 WHERE contract_id=$1`,
				fixture.contractId, server.NowUtc().Add(-quarantineReservationReleaseHorizon-time.Minute)))
		})
		hits := hook.hits.Load()
		clientSession := session.NewLocalClientSession(ctx, "", nil)
		defer clientSession.Cancel()
		result, err := ReleaseQuarantinedContractReservation(&QuarantineReservationReleaseArgs{ContractId: fixture.contractId}, clientSession)
		if err != nil || result == nil || !result.LeaseEnded || hook.hits.Load() != hits {
			t.Fatal("a release past the lease did not end without Redis", result, err, hook.hits.Load()-hits)
		}
	})
}

// Failed releases back off exponentially from the base to the cap, with
// jitter at every step, and every delay is a valid task retry delay.
func TestQuarantineReleaseRetryDelayBacksOffWithJitter(t *testing.T) {
	base, retryCap := quarantineReservationReleaseRetryBase, quarantineReservationReleaseRetryCap
	cases := []struct {
		errorCount int
		low        time.Duration
		high       time.Duration
	}{
		{errorCount: -1, low: base, high: 2 * base},
		{errorCount: 0, low: base, high: 2 * base},
		{errorCount: 1, low: 2 * base, high: 3 * base},
		{errorCount: 5, low: 32 * base, high: 33 * base},
		{errorCount: 8, low: 256 * base, high: 257 * base},
		{errorCount: 9, low: retryCap / 2, high: retryCap + retryCap/2},
		{errorCount: 15, low: retryCap / 2, high: retryCap + retryCap/2},
		{errorCount: 16, low: retryCap / 2, high: retryCap + retryCap/2},
		{errorCount: 1000, low: retryCap / 2, high: retryCap + retryCap/2},
	}
	for _, c := range cases {
		least := quarantineReservationReleaseRetryDelay(c.errorCount, 0)
		middle := quarantineReservationReleaseRetryDelay(c.errorCount, 0.5)
		most := quarantineReservationReleaseRetryDelay(c.errorCount, 1)
		if least != c.low || !(least < middle && middle < most && most < c.high) {
			t.Errorf("error count %d: delays %v, %v, %v outside [%v, %v)", c.errorCount, least, middle, most, c.low, c.high)
		}
		if least < task.RescheduleTimeout || task.RescheduleBackoffMaxTimeout < most {
			t.Errorf("error count %d: delays %v to %v are not valid task retry delays", c.errorCount, least, most)
		}
	}
}
