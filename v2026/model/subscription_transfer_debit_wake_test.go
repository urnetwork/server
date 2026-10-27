// Both settlement paths that journal a debit wake their partition's backed-off
// flush chain after the commit, and leave an active chain's queue row alone. A
// scoped expiry repair does not wake (subscription_contract_redis_expiry_projection_test.go).
package model

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

// The stand-in chain's arguments and result carry nothing.
type transferDebitWakeStandInArgs struct{}
type transferDebitWakeStandInResult struct{}

// Holds a partition's chain key in the queue; no worker runs it here.
func transferDebitWakeStandIn(*transferDebitWakeStandInArgs, *session.ClientSession) (*transferDebitWakeStandInResult, error) {
	return &transferDebitWakeStandInResult{}, nil
}

// The raw key's stored RunOnce form is the literal the debit monitor's query and
// the startup ownership keys build.
func TestTransferDebitFlushRunOnceKeyMatchesTheStoredChainKey(t *testing.T) {
	if key := TransferDebitFlushRunOnceKey(3); key != "flush_transfer_debits_3" || task.RunOnce(key).String() != `["flush_transfer_debits_3"]` {
		t.Fatal("the debit chain key changed", key, task.RunOnce(key).String())
	}
}

// A stand-in for the partition's backed-off chain: a queue row under the
// chain's key due at the idle delay, and, when marked, the chain's idle mark.
func backOffTransferDebitWakeStandIn(t testing.TB, ctx context.Context, shard int, marked bool) {
	t.Helper()
	owner := session.NewLocalClientSession(ctx, "", nil)
	defer owner.Cancel()
	key := TransferDebitFlushRunOnceKey(shard)
	server.Tx(ctx, func(tx server.PgTx) {
		task.ScheduleTaskInTx(tx, transferDebitWakeStandIn, &transferDebitWakeStandInArgs{}, owner,
			task.RunOnce(key), task.RunAt(server.NowUtc().Add(30*time.Second)))
	}, server.TxReadCommitted, server.OptNoRetry())
	if marked {
		server.Raise(task.MarkIdleRunOnce(ctx, key, task.DefaultIdleWakeSettings()))
	}
}

// The stand-in row's run_at and row version.
func readTransferDebitWakeStandIn(t testing.TB, ctx context.Context, shard int) (runAt time.Time, version string) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT run_at,xmin::text FROM pending_task WHERE run_once_key=$1`,
			task.RunOnce(TransferDebitFlushRunOnceKey(shard)).String()).Scan(&runAt, &version))
	})
	return
}

// The ordinary settlement journals its debit and, once the commit returned,
// kicks the backed-off chain of the debit's partition. A later settlement of
// the same partition finds no mark and leaves the chain's row unwritten.
func TestAsyncDebitSettlementWakesOnlyABackedOffFlushChain(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		shard := transferDebitShard(f.balanceId)
		backOffTransferDebitWakeStandIn(t, ctx, shard, true)
		contract := createRedisAdmissionTest(ctx, f, 100)
		posts := asyncDebitTestSettle(ctx, contract.ContractId, 11)
		if _, pending, _ := asyncDebitTestState(t, ctx, f.balanceId); pending != 1 {
			t.Fatal("the settlement did not journal its debit", pending)
		}
		if runAt, _ := readTransferDebitWakeStandIn(t, ctx, shard); runAt.Before(server.NowUtc().Add(10 * time.Second)) {
			t.Fatal("the chain moved before the settlement's posts ran", runAt)
		}
		server.RunPosts(ctx, posts...)
		runAt, version := readTransferDebitWakeStandIn(t, ctx, shard)
		if runAt.After(server.NowUtc()) {
			t.Fatal("the committed debit did not kick its backed-off chain", runAt)
		}
		second := createRedisAdmissionTest(ctx, f, 100)
		server.RunPosts(ctx, asyncDebitTestSettle(ctx, second.ContractId, 11)...)
		if _, after := readTransferDebitWakeStandIn(t, ctx, shard); after != version {
			t.Fatal("a settlement wrote the queue row of a chain that was not backed off")
		}
	})
}

// An ordinary bilateral close settles through the foreground owner without an
// expiry scope, and its committed debit kicks the backed-off chain as the
// deferred settlement's does. Only a scoped repair leaves the chain alone.
func TestAsyncDebitBilateralCloseWakesABackedOffFlushChain(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		shard := transferDebitShard(f.balanceId)
		backOffTransferDebitWakeStandIn(t, ctx, shard, true)
		contract := createRedisAdmissionTest(ctx, f, 100)
		server.Raise(CloseContract(ctx, contract.ContractId, f.sourceId, 11, false))
		if runAt, _ := readTransferDebitWakeStandIn(t, ctx, shard); runAt.Before(server.NowUtc().Add(10 * time.Second)) {
			t.Fatal("one party's close moved the chain", runAt)
		}
		server.Raise(CloseContract(ctx, contract.ContractId, f.destinationId, 11, false))
		if _, pending, applied := asyncDebitTestState(t, ctx, f.balanceId); pending+applied != 1 {
			t.Fatal("the bilateral close did not journal its debit", pending, applied)
		}
		if runAt, _ := readTransferDebitWakeStandIn(t, ctx, shard); runAt.After(server.NowUtc()) {
			t.Fatal("the bilateral close's debit did not kick its backed-off chain", runAt)
		}
	})
}

// A deadline close of a Redis contract journals its debit too, and its
// committed posts kick the partition's backed-off chain.
func TestDeadlineCloseWakesABackedOffFlushChain(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		shard := transferDebitShard(f.balanceId)
		backOffTransferDebitWakeStandIn(t, ctx, shard, true)
		escrow := createRedisAdmissionTest(ctx, f, 100)
		keys := deadlineFinancialOwnershipKeys(escrow.ContractId, readDeadlineBalanceIds(ctx, escrow.ContractId))
		var result *ContractDeadlineReconciliation
		var posts []server.PostFunction
		server.OwnedTx(ctx, keys, func(tx server.PgTx) {
			result, posts = reconcileContractAtDeadlineWithOwnershipInTx(ctx, tx, escrow.ContractId, server.NowUtc(), true)
		}, server.TxReadCommitted, server.OptNoRetry())
		if result == nil || result.AlreadyClosed || result.Missing {
			t.Fatalf("the contract did not close at its deadline: %+v", result)
		}
		if runAt, _ := readTransferDebitWakeStandIn(t, ctx, shard); runAt.Before(server.NowUtc().Add(10 * time.Second)) {
			t.Fatal("the chain moved before the close's posts ran", runAt)
		}
		server.RunPosts(ctx, posts...)
		if runAt, _ := readTransferDebitWakeStandIn(t, ctx, shard); runAt.After(server.NowUtc()) {
			t.Fatal("the deadline close's debit did not kick its backed-off chain", runAt)
		}
	})
}

// Fails a producer wake's idle-mark deletion on the deadline pool at once,
// recording how long its caller would have waited for a stalled Redis.
type transferDebitWakeBoundHook struct {
	enabled   atomic.Bool
	stateLock sync.Mutex
	remaining []time.Duration
}

func (self *transferDebitWakeBoundHook) DialHook(next redis.DialHook) redis.DialHook {
	return next
}

func (self *transferDebitWakeBoundHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return next
}

func (self *transferDebitWakeBoundHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, commands []redis.Cmder) error {
		if !self.enabled.Load() || len(commands) == 0 || commands[0].Name() != "del" || len(commands[0].Args()) < 2 {
			return next(ctx, commands)
		}
		key, ok := commands[0].Args()[1].(string)
		if !ok || !strings.Contains(key, "{task_idle_wake}") {
			return next(ctx, commands)
		}
		remaining := time.Duration(math.MaxInt64)
		if deadline, bounded := ctx.Deadline(); bounded {
			remaining = time.Until(deadline)
		}
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.remaining = append(self.remaining, remaining)
		}()
		return errors.New("synthetic idle mark unavailable")
	}
}

// The remaining wait of every intercepted wake.
func (self *transferDebitWakeBoundHook) snapshot() []time.Duration {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]time.Duration(nil), self.remaining...)
}

// A producer's wake is optional work its caller waits for after the commit. With
// Redis stalled, it must cost a settlement no more than the contract-hole delta
// that every terminal claim already spends, and a failed wake moves nothing.
func TestAsyncDebitSettlementWakeWaitsNoLongerThanTheLifecycleDelta(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		shard := transferDebitShard(f.balanceId)
		backOffTransferDebitWakeStandIn(t, ctx, shard, true)
		contract := createRedisAdmissionTest(ctx, f, 100)
		hook := &transferDebitWakeBoundHook{}
		install, cancelInstall := context.WithTimeout(ctx, time.Second)
		server.Raise(server.RedisWithDeadline(install, func(client server.RedisClient) error {
			client.AddHook(hook)
			return nil
		}))
		cancelInstall()
		hook.enabled.Store(true)
		defer hook.enabled.Store(false)
		server.RunPosts(ctx, asyncDebitTestSettle(ctx, contract.ContractId, 11)...)
		hook.enabled.Store(false)
		remaining := hook.snapshot()
		if len(remaining) != 1 || remaining[0] <= 0 || contractHoleRedisTimeout < remaining[0] {
			t.Fatal("the settlement's wake could wait longer than the lifecycle delta's bound", remaining)
		}
		if runAt, _ := readTransferDebitWakeStandIn(t, ctx, shard); runAt.Before(server.NowUtc().Add(10 * time.Second)) {
			t.Fatal("a failed wake moved the backed-off chain", runAt)
		}
	})
}
