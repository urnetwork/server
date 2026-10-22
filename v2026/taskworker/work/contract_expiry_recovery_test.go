// Recovery publishes durable normal owners without discarding in-flight scans.
package work

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

type expiryRecoveryQueued struct {
	id          server.Id
	function    string
	args        string
	runAt       time.Time
	claimTime   time.Time
	releaseTime time.Time
	wakeAt      *time.Time
	generation  int64
	claim       int64
}

func readExpiryRecoveryQueue(t testing.TB, ctx context.Context) map[string]expiryRecoveryQueued {
	t.Helper()
	queued := map[string]expiryRecoveryQueued{}
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `SELECT run_once_key,task_id,function_name,args_json,run_at,
            claim_time,release_time,run_once_wake_at,run_once_generation,claim_generation
            FROM pending_task ORDER BY run_once_key`)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var key string
				var row expiryRecoveryQueued
				server.Raise(rows.Scan(&key, &row.id, &row.function, &row.args, &row.runAt,
					&row.claimTime, &row.releaseTime, &row.wakeAt, &row.generation, &row.claim))
				queued[key] = row
			}
		})
	}, server.OptNoRetry())
	return queued
}

// Absent owners used to make a key-only kick a successful no-op. The recovery
// command must create the recurring expiry owner and every legacy dispatcher.
func TestQueueContractExpiryRecoveryCreatesEveryNormalOwner(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		before := server.NowUtc()
		result, err := QueueContractExpiryRecovery(ctx)
		after := server.NowUtc()
		if err != nil || result.SweepRequests != 1 || result.LegacyDispatcherRequests != 16 ||
			result.RequestedAt.Before(before) || result.RequestedAt.After(after) {
			t.Fatal("recovery did not acknowledge all committed normal owners", err)
		}
		queued := readExpiryRecoveryQueue(t, ctx)
		if len(queued) != 17 {
			t.Fatal("recovery did not create exactly the existing bounded owners", len(queued))
		}
		sweep, found := queued[task.RunOnce("close_expired_contracts_1_0").String()]
		var sweepArgs CloseExpiredContractsArgs
		if !found || json.Unmarshal([]byte(sweep.args), &sweepArgs) != nil || sweepArgs.BlockSize != 1 || sweepArgs.BlockIndex != 0 ||
			sweep.function != task.NewTaskTarget(CloseExpiredContracts).TargetFunctionName() {
			t.Fatal("recovery created a replacement expiry architecture")
		}
		for shard := range model.LegacySettlementShardCount {
			row, found := queued[task.RunOnce(fmt.Sprintf("flush_legacy_settlements_%d", shard)).String()]
			var args FlushLegacySettlementsArgs
			if !found || json.Unmarshal([]byte(row.args), &args) != nil || args.Shard != shard ||
				row.function != NewLegacySettlementDispatcherTaskTarget().TargetFunctionName() {
				t.Fatal("recovery omitted an existing intent dispatcher", shard)
			}
		}
		for _, row := range queued {
			if row.runAt.Before(before) || row.runAt.After(after) {
				t.Fatal("recovery deferred a dispatcher or the sweep")
			}
		}
	})
}

// A global kick must not replace captured epochs, source/payer positions,
// registration progress or claim leases, and it must keep an earlier RunAt.
func TestQueueContractExpiryRecoveryKeepsExistingCustodyAndEarliestWake(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		stamp := server.NowUtc().Truncate(time.Microsecond)
		id := server.NewId()
		cursor := &model.ContractExpiryCursor{ScanBefore: stamp.Add(-time.Hour),
			Open: &model.ContractExpiryPosition{CreateTime: stamp.Add(-2 * time.Hour), ContractId: id}, DisputeDone: true}
		legacy := &model.LegacySettlementCursor{ContractId: id, NextAttemptTime: stamp.Add(-time.Minute), PassEndTime: stamp}
		payer := &model.LegacySettlementPayerCursor{After: &id, End: id, PassEndTime: stamp}
		registration := &model.LegacySettlementOwnerCursor{After: &id, End: id}
		keys := append(legacySettlementStartupOwnershipKeys(), task.RunOnceOwnershipKey(task.RunOnce("close_expired_contracts_1_0")))
		server.OwnedTx(ctx, keys, func(tx server.PgTx) {
			scheduleCloseExpiredContractsPage(owner, tx, 0, true, cursor,
				&model.ContractExpirySweepCursor{Historical: cursor, RecentAfter: stamp, HistoricalNext: true})
			for shard := range model.LegacySettlementShardCount {
				scheduleFlushLegacySettlements(owner, tx, shard, legacy, payer, false, payer, registration)
			}
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET run_at=$1,claim_time=$2,release_time=$3,claim_generation=7`,
				stamp.Add(time.Hour), stamp.Add(-time.Second), stamp.Add(2*time.Hour)))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET run_at=$2 WHERE run_once_key=$1`,
				task.RunOnce("flush_legacy_settlements_0").String(), stamp.Add(-time.Minute)))
		}, server.TxReadCommitted, server.OptNoRetry())
		before := readExpiryRecoveryQueue(t, ctx)
		for attempt := range 2 {
			requestedBefore := server.NowUtc()
			if _, err := QueueContractExpiryRecovery(ctx); err != nil {
				t.Fatal(err)
			}
			requestedAfter := server.NowUtc()
			current := readExpiryRecoveryQueue(t, ctx)
			if len(current) != len(before) {
				t.Fatal("coalesced recovery duplicated an owner")
			}
			for key, original := range before {
				row, found := current[key]
				if !found || row.id != original.id || row.args != original.args || row.function != original.function ||
					!row.claimTime.Equal(original.claimTime) || !row.releaseTime.Equal(original.releaseTime) || row.claim != original.claim ||
					row.generation != original.generation+int64(attempt)+1 || row.wakeAt == nil || row.wakeAt.After(requestedAfter) {
					t.Fatal("recovery replaced captured scan state or active custody")
				}
				if original.runAt.Before(requestedBefore) {
					if !row.runAt.Equal(original.runAt) {
						t.Fatal("recovery postponed an earlier request")
					}
				} else if row.runAt.After(requestedAfter) {
					t.Fatal("recovery retained a future dispatcher delay")
				}
			}
		}
	})
}

// A request after Run returns must survive the normal EOF Post and its random
// idle delay. The real evaluator owns claim, completion and generation handoff.
type expiryRecoveryCompletionBarrier struct {
	task.Target
	entered chan struct{}
	release <-chan struct{}
}

func (self *expiryRecoveryCompletionBarrier) Run(ctx context.Context, queued *task.Task) (any, func(server.PgTx) ([]server.PostFunction, error), error) {
	result, post, err := self.Target.Run(ctx, queued)
	if err != nil {
		return result, post, err
	}
	select {
	case self.entered <- struct{}{}:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
	select {
	case <-self.release:
		return result, post, nil
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
}

func TestQueueContractExpiryRecoveryDuringActiveEofKeepsSuccessor(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		server.Tx(ctx, func(tx server.PgTx) { ScheduleCloseExpiredContracts(owner, tx, 0, false) })
		queued, found := readExpiryRecoveryQueue(t, ctx)[task.RunOnce("close_expired_contracts_1_0").String()]
		if !found {
			t.Fatal("ordinary expiry owner was not scheduled")
		}
		// available_block rounds beyond RunAt. Force the synthetic task due
		// before the single evaluator call; the barrier still owns the race.
		makeCloseRetryTaskDue(ctx, queued.id)
		release := make(chan struct{})
		var released sync.Once
		target := &expiryRecoveryCompletionBarrier{Target: task.NewTaskTargetWithPost(CloseExpiredContracts, CloseExpiredContractsPost),
			entered: make(chan struct{}, 1), release: release}
		settings := task.DefaultTaskWorkerSettings()
		settings.ClaimRegisteredTargetsOnly = true
		worker := task.NewTaskWorker(ctx, settings)
		worker.AddTargets(target)
		var finished, retried []server.Id
		var evalErr error
		done := make(chan struct{})
		go func() {
			defer close(done)
			server.HandleError(func() { finished, retried, _, evalErr = worker.EvalTasks(1) }, func(err error) { evalErr = err })
		}()
		defer func() {
			released.Do(func() { close(release) })
			select {
			case <-done:
			case <-ctx.Done():
			}
			worker.Close()
		}()
		select {
		case <-target.entered:
		case <-done:
			t.Fatal("expiry finished without entering the completion boundary", evalErr)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		if _, err := QueueContractExpiryRecovery(ctx); err != nil {
			t.Fatal(err)
		}
		requestedAfter := server.NowUtc()
		released.Do(func() { close(release) })
		select {
		case <-done:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		if evalErr != nil || len(finished) != 1 || len(retried) != 0 {
			t.Fatal("ordinary expiry completion failed", evalErr)
		}
		row, found := readExpiryRecoveryQueue(t, ctx)[task.RunOnce("close_expired_contracts_1_0").String()]
		var args CloseExpiredContractsArgs
		if !found || row.runAt.After(requestedAfter) || json.Unmarshal([]byte(row.args), &args) != nil || args.Cursor != nil || args.Sweep != nil {
			t.Fatal("EOF dropped the explicit recovery generation or delayed its next full pass")
		}
	})
}
