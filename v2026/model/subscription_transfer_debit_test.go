package model

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server/v2026"
)

func asyncDebitTestState(t testing.TB, ctx context.Context, balanceId server.Id) (credit ByteCount, pending, applied int) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$1`, balanceId).Scan(&credit))
		server.Raise(conn.QueryRow(ctx, `SELECT count(*) FILTER(WHERE NOT applied),count(*) FILTER(WHERE applied) FROM transfer_debit_journal WHERE balance_id=$1`, balanceId).Scan(&pending, &applied))
	})
	return
}

// Real outcome owner with its post callbacks deliberately retained by the test.
func asyncDebitTestSettle(ctx context.Context, contractId server.Id, amount ByteCount) (posts []func() any) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close(contract_id,party,used_transfer_byte_count,close_time,checkpoint)
      VALUES($1,'source',$2,clock_timestamp() AT TIME ZONE 'UTC',false),($1,'destination',$2,clock_timestamp() AT TIME ZONE 'UTC',false)`, contractId, amount))
		var closed bool
		var err error
		posts, closed, err = settleEscrowInTx(ctx, tx, contractId, ContractOutcomeSettled)
		server.Raise(err)
		if !closed {
			panic("test outcome was not claimed")
		}
	}, server.TxReadCommitted, server.OptNoRetry())
	return
}

func TestAsyncDebitLostPostsReplayAndRedisPrecedence(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		contract := createRedisAdmissionTest(ctx, f, 100)
		_ = createRedisAdmissionTest(ctx, f, 200)
		posts := asyncDebitTestSettle(ctx, contract.ContractId, 11)
		credit, pending, applied := asyncDebitTestState(t, ctx, f.balanceId)
		if credit != 1000 || pending != 1 || applied != 0 || Testing_NetEscrowByteCount(ctx, f.balanceId) != 300 {
			t.Fatal("lost callback changed durable credit or released reservation")
		}
		ReconcileRedisContractReservation(ctx, contract.ContractId)
		server.RunPosts(ctx, posts...)
		server.RunPosts(ctx, posts...)
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 211 {
			t.Fatal("pending consumption or neighboring reservation lost", got)
		}
		if got := GetActiveTransferBalanceByteCount(ctx, f.sourceNetworkId); got != 789 {
			t.Fatal("pending credit counted as available", got)
		}
		n, released, busy, err := flushTransferDebitBalance(ctx, f.balanceId)
		if err != nil || n != 1 || released != 1 || busy {
			t.Fatal("grouped debit did not complete", n, released, busy, err)
		}
		// An old callback/reconcile cannot restore a token after the journal was drained.
		server.RunPosts(ctx, posts...)
		ReconcileRedisContractReservation(ctx, contract.ContractId)
		n, released, busy, err = flushTransferDebitBalance(ctx, f.balanceId)
		credit, pending, applied = asyncDebitTestState(t, ctx, f.balanceId)
		if err != nil || n != 0 || released != 0 || busy || credit != 989 || pending+applied != 0 || Testing_NetEscrowByteCount(ctx, f.balanceId) != 200 {
			t.Fatal("flush replay or late post changed accounting", credit, pending, applied, err)
		}
	})
}

func TestAsyncDebitRollbackCannotPublishOrConsume(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		contract := createRedisAdmissionTest(ctx, f, 100)
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		tx, err := conn.Begin(ctx)
		server.Raise(err)
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close(contract_id,party,used_transfer_byte_count,close_time,checkpoint) VALUES($1,'source',11,now(),false),($1,'destination',11,now(),false)`, contract.ContractId))
		posts, closed, err := settleEscrowInTx(ctx, tx, contract.ContractId, ContractOutcomeSettled)
		server.Raise(err)
		if !closed {
			t.Fatal("rollback owner not exercised")
		}
		server.Raise(tx.Rollback(ctx))
		server.RunPosts(ctx, posts...)
		credit, pending, applied := asyncDebitTestState(t, ctx, f.balanceId)
		if credit != 1000 || pending+applied != 0 || Testing_NetEscrowByteCount(ctx, f.balanceId) != 100 {
			t.Fatal("rolled back outcome affected debit or reservation")
		}
	})
}

func asyncDebitInstallHook(ctx context.Context, hook *asyncDebitReleaseHook) {
	bounded, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	server.Raise(server.RedisWithDeadline(bounded, func(r server.RedisClient) error { r.AddHook(hook); return nil }))
}

type asyncDebitReleaseHook struct {
	key     string
	enabled atomic.Bool
	hits    atomic.Int64
	after   bool
}

func (self *asyncDebitReleaseHook) DialHook(next redis.DialHook) redis.DialHook          { return next }
func (self *asyncDebitReleaseHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook { return next }
func (self *asyncDebitReleaseHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, commands []redis.Cmder) error {
		matched := false
		for _, command := range commands {
			a := command.Args()
			if command.Name() == "eval" && len(a) > 3 {
				keyCount, ok := a[2].(int)
				if ok && keyCount > 0 && len(a) > 3+keyCount && a[3] == self.key && a[3+keyCount] == "release" {
					matched = true
				}
			}
		}
		if matched && self.enabled.Load() {
			self.hits.Add(1)
			if self.after {
				if err := next(ctx, commands); err != nil {
					return err
				}
			}
			return errors.New("synthetic release acknowledgement unavailable")
		}
		return next(ctx, commands)
	}
}

func TestAsyncDebitCommittedBatchSurvivesRedisFailureAndLostAck(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		for _, after := range []bool{false, true} {
			f := newNetEscrowOrderingTestFixture(t, ctx)
			contract := createRedisAdmissionTest(ctx, f, 100)
			_ = asyncDebitTestSettle(ctx, contract.ContractId, 11)
			hook := &asyncDebitReleaseHook{key: redisContractReservationKeys(f.balanceId)[0], after: after}
			hook.enabled.Store(true)
			asyncDebitInstallHook(ctx, hook)
			_, _, _, err := flushTransferDebitBalance(ctx, f.balanceId)
			if err == nil || hook.hits.Load() == 0 {
				t.Fatal("actual post-commit Redis failure not exercised")
			}
			credit, pending, applied := asyncDebitTestState(t, ctx, f.balanceId)
			if credit != 989 || pending != 0 || applied != 1 {
				t.Fatal("committed batch lost its replay evidence", credit, pending, applied)
			}
			hook.enabled.Store(false)
			n, released, busy, err := flushTransferDebitBalance(ctx, f.balanceId)
			if err != nil || n != 0 || released != 1 || busy {
				t.Fatal("release recovery re-debited credit", n, released, busy, err)
			}
			credit, pending, applied = asyncDebitTestState(t, ctx, f.balanceId)
			if credit != 989 || pending+applied != 0 || Testing_NetEscrowByteCount(ctx, f.balanceId) != 0 {
				t.Fatal("recovery changed balance")
			}
		}
	})
}

func TestAsyncDebitRedisLossPreservesJournalAndNeighbor(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		contract := createRedisAdmissionTest(ctx, f, 100)
		posts := asyncDebitTestSettle(ctx, contract.ContractId, 11)
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Del(ctx, redisContractReservationKeys(f.balanceId)[:3]...).Err())
		})
		_ = createRedisAdmissionTest(ctx, f, 23)
		// Cache loss may temporarily over-admit. Reconcile must not consume the new
		// generation's neighbor or invent a second durable debit from that mirror.
		ReconcileRedisContractReservation(ctx, contract.ContractId)
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 23 {
			t.Fatal("lost-generation reconcile changed neighbor", got)
		}
		_, _, _, err := flushTransferDebitBalance(ctx, f.balanceId)
		server.Raise(err)
		server.RunPosts(ctx, posts...)
		credit, pending, applied := asyncDebitTestState(t, ctx, f.balanceId)
		if credit != 989 || pending+applied != 0 || Testing_NetEscrowByteCount(ctx, f.balanceId) != 23 {
			t.Fatal("cache loss destroyed durable consumption")
		}
	})
}

func TestAsyncDebitBusyGrantAdvancesFairCursor(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		first := newNetEscrowOrderingTestFixture(t, ctx)
		second := newNetEscrowOrderingTestFixture(t, ctx)
		replacement := second.balanceId
		replacement[15] = first.balanceId[15]
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET balance_id=$2 WHERE balance_id=$1`, second.balanceId, replacement))
		})
		second.balanceId = replacement
		if second.balanceId.Less(first.balanceId) {
			first, second = second, first
		}
		for _, f := range []netEscrowOrderingTestFixture{first, second} {
			c := createRedisAdmissionTest(ctx, f, 100)
			_ = asyncDebitTestSettle(ctx, c.ContractId, 11)
		}
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		held, err := conn.Begin(ctx)
		server.Raise(err)
		defer held.Rollback(context.Background())
		server.RaisePgResult(held.Exec(ctx, `SELECT balance_id FROM transfer_balance WHERE balance_id=$1 FOR UPDATE`, first.balanceId))
		result, err := FlushTransferDebits(ctx, transferDebitShard(first.balanceId), nil, 1)
		if err != nil || result.Busy != 1 || result.LastBalanceId == nil || *result.LastBalanceId != first.balanceId {
			t.Fatal("busy first grant did not advance cursor", result, err)
		}
		next, err := FlushTransferDebits(ctx, transferDebitShard(first.balanceId), result.LastBalanceId, 64)
		if err != nil || next.Applied != 1 || next.Failed != 0 {
			t.Fatal("busy grant starved unrelated payer", next, err)
		}
		server.Raise(held.Rollback(ctx))
		last, err := FlushTransferDebits(ctx, transferDebitShard(first.balanceId), next.LastBalanceId, 64)
		if err != nil || last.Applied != 1 {
			t.Fatal("cursor wrap lost skipped grant", last, err)
		}
		for _, f := range []netEscrowOrderingTestFixture{first, second} {
			credit, pending, applied := asyncDebitTestState(t, ctx, f.balanceId)
			if credit != 989 || pending+applied != 0 {
				t.Fatal("fair drain changed consumption")
			}
		}
	})
}

func TestAsyncDebitPendingAndAppliedRowsFenceBalanceRetention(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		contract := createRedisAdmissionTest(ctx, f, 100)
		posts := asyncDebitTestSettle(ctx, contract.ContractId, 11)
		server.RunPosts(ctx, posts...)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET end_time=(clock_timestamp() AT TIME ZONE 'UTC')-interval '1 minute' WHERE balance_id=$1`, f.balanceId))
		})
		removeCompletedTransferBalanceBatch(ctx, []server.Id{f.balanceId}, server.NowUtc())
		credit, pending, _ := asyncDebitTestState(t, ctx, f.balanceId)
		if credit != 1000 || pending != 1 {
			t.Fatal("retention removed pending debit")
		}
		hook := &asyncDebitReleaseHook{key: redisContractReservationKeys(f.balanceId)[0]}
		hook.enabled.Store(true)
		asyncDebitInstallHook(ctx, hook)
		_, _, _, err := flushTransferDebitBalance(ctx, f.balanceId)
		if err == nil {
			t.Fatal("release failure missing")
		}
		removeCompletedTransferBalanceBatch(ctx, []server.Id{f.balanceId}, server.NowUtc())
		credit, _, applied := asyncDebitTestState(t, ctx, f.balanceId)
		if credit != 989 || applied != 1 {
			t.Fatal("retention removed unapplied Redis cleanup")
		}
		hook.enabled.Store(false)
		_, _, _, err = flushTransferDebitBalance(ctx, f.balanceId)
		server.Raise(err)
		removeCompletedTransferBalanceBatch(ctx, []server.Id{f.balanceId}, server.NowUtc())
		var remains bool
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM transfer_balance WHERE balance_id=$1)`, f.balanceId).Scan(&remains))
		})
		if remains {
			t.Fatal("drained expired balance could not be retained normally")
		}
	})
}

func TestAsyncDebitShardHardDeleteWaitsForWriteback(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		owner := shardTestOwner(t, ctx, shardTestKey(0))
		peer := newEscrowSelectionTestClients(t, ctx)
		contract, err := CreateTransferEscrow(ctx, owner.NetworkId, owner.ClientId, peer.providerNetworkId, peer.providerId, 4096)
		server.Raise(err)
		server.Raise(DrainProberShard(ctx, owner.Key))
		server.Raise(CloseContract(ctx, contract.ContractId, owner.ClientId, 1024, false))
		server.Raise(CloseContract(ctx, contract.ContractId, peer.providerId, 1024, false))
		deleted, err := ReapProberShard(ctx, owner.Key)
		if err != nil || deleted {
			t.Fatal("shard erased pending consumption", deleted, err)
		}
		credit, pending, _ := asyncDebitTestState(t, ctx, owner.BalanceId)
		if credit != 64*1024 || pending != 1 {
			t.Fatal("pending shard accounting changed")
		}
		_, _, _, err = flushTransferDebitBalance(ctx, owner.BalanceId)
		server.Raise(err)
		credit, pending, _ = asyncDebitTestState(t, ctx, owner.BalanceId)
		if credit != 64*1024-1024 || pending != 0 {
			t.Fatal("shard consumption did not flush")
		}
		deleted, err = ReapProberShard(ctx, owner.Key)
		if err != nil || !deleted {
			t.Fatal("drained shard could not retire", deleted, err)
		}
		if n, c, b := shardTestRows(t, ctx, owner); n != 0 || c != 0 || b != 0 {
			t.Fatal("retired shard retained live identities or credit")
		}
	})
}

// Migration protection also covers an old reaper that lacks the new predicate.
func TestAsyncDebitOldReaperCannotDeleteUnflushedBalance(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		c := createRedisAdmissionTest(ctx, f, 100)
		server.RunPosts(ctx, asyncDebitTestSettle(ctx, c.ContractId, 11)...)
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		tx, err := conn.Begin(ctx)
		server.Raise(err)
		_, err = tx.Exec(ctx, `DELETE FROM transfer_balance WHERE balance_id=$1`, f.balanceId)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "55000" {
			t.Fatal("old cleanup bypassed durable debit guard", err)
		}
		server.Raise(tx.Rollback(ctx))
		credit, pending, _ := asyncDebitTestState(t, ctx, f.balanceId)
		if credit != 1000 || pending != 1 {
			t.Fatal("old cleanup damaged debt")
		}
	})
}

func TestAsyncDebitDistinctPayerDrainCapacity(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
		defer cancel()
		const count = 512
		balances := make([]server.Id, 0, count)
		for index := range count {
			f := newNetEscrowOrderingTestFixture(t, ctx)
			replacement := f.balanceId
			replacement[15] = byte(index % TransferDebitShardCount)
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET balance_id=$2 WHERE balance_id=$1`, f.balanceId, replacement))
			})
			f.balanceId = replacement
			contract := createRedisAdmissionTest(ctx, f, 100)
			_ = asyncDebitTestSettle(ctx, contract.ContractId, 11)
			balances = append(balances, f.balanceId)
		}
		type outcome struct {
			result TransferDebitFlushResult
			err    error
		}
		results := make(chan outcome, TransferDebitShardCount)
		start := make(chan struct{})
		began := time.Now()
		for shard := range TransferDebitShardCount {
			go func() {
				<-start
				r, err := FlushTransferDebits(ctx, shard, nil, 64)
				results <- outcome{result: r, err: err}
			}()
		}
		close(start)
		applied, released := 0, 0
		for range TransferDebitShardCount {
			out := <-results
			if out.err != nil || out.result.Failed != 0 || out.result.Busy != 0 {
				t.Fatal("distinct-payer writeback failed", out.result, out.err)
			}
			applied += out.result.Applied
			released += out.result.Released
		}
		elapsed := time.Since(began)
		if applied != count || released != count {
			t.Fatal("partitioned worker did not drain distinct payers", applied, released)
		}
		var correct, journals int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_balance WHERE balance_id=ANY($1) AND balance_byte_count=989`, balances).Scan(&correct))
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_debit_journal WHERE balance_id=ANY($1)`, balances).Scan(&journals))
		})
		if correct != count || journals != 0 {
			t.Fatal("distinct payer durable accounting differs", correct, journals)
		}
		if elapsed > 5*time.Second {
			t.Fatal("distinct payer drain exceeded local release budget", elapsed)
		}
		t.Logf("distinct payers=%d logical partitions=%d actual simultaneous worker calls=%d elapsed=%s", count, TransferDebitShardCount, TransferDebitShardCount, elapsed)
	})
}

// A failed Redis acknowledgment must publish its cursor before subsequent
// failing keys could exhaust the bounded owner and replay the same prefix.
func TestAsyncDebitFailedReleaseAdvancesFairCursor(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		first := newNetEscrowOrderingTestFixture(t, ctx)
		second := newNetEscrowOrderingTestFixture(t, ctx)
		// Fixtures create independent payers; put both balance keys in one partition.
		replacement := second.balanceId
		replacement[15] = first.balanceId[15]
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET balance_id=$2 WHERE balance_id=$1`, second.balanceId, replacement))
		})
		second.balanceId = replacement
		if second.balanceId.Less(first.balanceId) {
			first, second = second, first
		}
		a := createRedisAdmissionTest(ctx, first, 100)
		b := createRedisAdmissionTest(ctx, second, 100)
		asyncDebitTestSettle(ctx, a.ContractId, 11)
		asyncDebitTestSettle(ctx, b.ContractId, 11)
		hook := &asyncDebitReleaseHook{key: redisContractReservationKeys(first.balanceId)[0]}
		hook.enabled.Store(true)
		asyncDebitInstallHook(ctx, hook)
		defer hook.enabled.Store(false)
		result, err := FlushTransferDebits(ctx, transferDebitShard(first.balanceId), nil, 64)
		if err != nil || result.Failed != 1 || result.LastBalanceId == nil || *result.LastBalanceId != first.balanceId || hook.hits.Load() == 0 {
			t.Fatal("failed prefix lost its cursor", result, err)
		}
		result, err = FlushTransferDebits(ctx, transferDebitShard(second.balanceId), result.LastBalanceId, 64)
		if err != nil || result.Failed != 0 || result.Released != 1 {
			t.Fatal("failed first key starved independent payer", result, err)
		}
		credit, pending, applied := asyncDebitTestState(t, ctx, second.balanceId)
		if credit != 989 || pending+applied != 0 {
			t.Fatal("healthy neighbor accounting changed")
		}
		hook.enabled.Store(false)
		result, err = FlushTransferDebits(ctx, transferDebitShard(first.balanceId), nil, 64)
		if err != nil || result.Applied != 0 || result.Released != 1 {
			t.Fatal("failed acknowledgment replay debited again", result, err)
		}
	})
}

func TestAsyncDebitSkewedPartitionContinuesPastKeyPage(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		for range 65 {
			f := newNetEscrowOrderingTestFixture(t, ctx)
			replacement := f.balanceId
			replacement[15] = 0
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET balance_id=$2 WHERE balance_id=$1`, f.balanceId, replacement))
			})
			f.balanceId = replacement
			contract := createRedisAdmissionTest(ctx, f, 100)
			asyncDebitTestSettle(ctx, contract.ContractId, 11)
		}
		first, err := FlushTransferDebits(ctx, 0, nil, 64)
		if err != nil || first.Applied != 64 || first.Released != 64 || !first.More || first.LastBalanceId == nil {
			t.Fatal("full key page did not retain continuation", first, err)
		}
		second, err := FlushTransferDebits(ctx, 0, first.LastBalanceId, 64)
		if err != nil || second.Applied != 1 || second.Released != 1 || second.More || second.LastBalanceId != nil {
			t.Fatal("skewed partition tail did not drain", second, err)
		}
	})
}

// PG rollback after an attempted balance debit cannot publish Redis release;
// overlapping recovery owners still apply the journal exactly once.
func TestAsyncDebitWorkerRollbackAndConcurrentReplay(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		contract := createRedisAdmissionTest(ctx, f, 100)
		asyncDebitTestSettle(ctx, contract.ContractId, 11)
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		held, err := conn.Begin(ctx)
		server.Raise(err)
		defer held.Rollback(context.Background())
		server.RaisePgResult(held.Exec(ctx, `SELECT contract_id FROM transfer_escrow WHERE contract_id=$1 FOR UPDATE`, contract.ContractId))
		applied, released, _, err := flushTransferDebitBalance(ctx, f.balanceId)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "55P03" || applied != 0 || released != 0 {
			t.Fatal("worker did not preserve lock-timeout rollback", applied, released, err)
		}
		credit, pending, committed := asyncDebitTestState(t, ctx, f.balanceId)
		if credit != 1000 || pending != 1 || committed != 0 || Testing_NetEscrowByteCount(ctx, f.balanceId) != 100 {
			t.Fatal("rolled back worker altered durable debit or Redis")
		}
		server.Raise(held.Rollback(ctx))
		type result struct {
			applied, released int
			err               error
		}
		results := make(chan result, 2)
		start := make(chan struct{})
		for range 2 {
			go func() { <-start; a, r, _, e := flushTransferDebitBalance(ctx, f.balanceId); results <- result{a, r, e} }()
		}
		close(start)
		applied, released = 0, 0
		for range 2 {
			r := <-results
			if r.err != nil {
				t.Fatal(r.err)
			}
			applied += r.applied
			released += r.released
		}
		credit, pending, committed = asyncDebitTestState(t, ctx, f.balanceId)
		if applied != 1 || released != 1 || credit != 989 || pending+committed != 0 || Testing_NetEscrowByteCount(ctx, f.balanceId) != 0 {
			t.Fatal("concurrent recovery repeated or lost consumption", applied, released, credit)
		}
	})
}
