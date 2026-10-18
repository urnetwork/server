// Real debit workers refuse acknowledged owner contention before financial or
// cleanup SQL. Durable journal custody survives both boundaries and later replay.
package model

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// The old waiting entry is canceled at its first acknowledged refusal. This
// deterministic control needs no elapsed timeout to expose the lost fair pass.
func TestAsyncDebitBusyAdmissionAdvancesFairCursor(t *testing.T) {
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
		for _, fixture := range []netEscrowOrderingTestFixture{first, second} {
			contract := createRedisAdmissionTest(ctx, fixture, 100)
			_ = createRedisAdmissionTest(ctx, fixture, 200)
			_ = asyncDebitTestSettle(ctx, contract.ContractId, 11)
		}
		var reruns atomic.Int32
		ctx = server.Testing_WithTxRerunHook(ctx, func() { reruns.Add(1) })
		release := holdTransferBalanceTestOwner(t, ctx, []server.Id{first.balanceId})
		defer release()
		attemptCtx, cancelAttempt := context.WithCancel(ctx)
		defer cancelAttempt()
		key := server.NewPgOwnershipKey("transfer_balance", first.balanceId)
		waiting, refused, admitted := 0, 0, 0
		observed := server.Testing_WithPgOwnershipObservation(attemptCtx, func(event server.PgOwnershipEvent) {
			if !slices.Contains(event.Keys, key) {
				return
			}
			switch event.Kind {
			case server.PgOwnershipWaiting:
				waiting++
				cancelAttempt()
			case server.PgOwnershipRefused:
				refused++
			case server.PgOwnershipAdmitted:
				admitted++
			}
		})
		result, err := FlushTransferDebits(observed, transferDebitShard(first.balanceId), nil, 64)
		if err != nil || waiting != 0 || refused != 1 || admitted != 0 || result.Balances != 2 ||
			result.Applied != 1 || result.Released != 1 || result.Busy != 1 || result.Failed != 0 || result.LastBalanceId != nil {
			t.Fatal("known busy admission waited, entered financial SQL or prevented independent progress", result, err, waiting, refused, admitted)
		}
		credit, pending, applied := asyncDebitTestState(t, ctx, first.balanceId)
		if credit != 1000 || pending != 1 || applied != 0 || Testing_NetEscrowByteCount(ctx, first.balanceId) != 300 {
			t.Fatal("refused owner changed pending debit or neighboring reservation", credit, pending, applied)
		}
		credit, pending, applied = asyncDebitTestState(t, ctx, second.balanceId)
		if credit != 989 || pending+applied != 0 || Testing_NetEscrowByteCount(ctx, second.balanceId) != 200 {
			t.Fatal("independent payer did not finish with its neighbor intact", credit, pending, applied)
		}
		release()
		result, err = FlushTransferDebits(ctx, transferDebitShard(first.balanceId), result.LastBalanceId, 64)
		if err != nil || result.Applied != 1 || result.Released != 1 || result.Busy != 0 || result.Failed != 0 {
			t.Fatal("released owner did not resume its retained debit exactly once", result, err)
		}
		result, err = FlushTransferDebits(ctx, transferDebitShard(first.balanceId), result.LastBalanceId, 64)
		credit, pending, applied = asyncDebitTestState(t, ctx, first.balanceId)
		if err != nil || result.Balances != 0 || credit != 989 || pending+applied != 0 ||
			Testing_NetEscrowByteCount(ctx, first.balanceId) != 200 || reruns.Load() != 0 {
			t.Fatal("fair retry lost journal custody, repeated a debit or reran a transaction", result, err, credit, pending, applied)
		}
	})
}

// Hold the balance only after the first owner has committed and released its
// checkout. A refused deletion keeps applied debt after real Redis release.
func TestAsyncDebitBusyCleanupPreservesAppliedJournal(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		fixture := newNetEscrowOrderingTestFixture(t, ctx)
		contract := createRedisAdmissionTest(ctx, fixture, 100)
		_ = createRedisAdmissionTest(ctx, fixture, 200)
		_ = asyncDebitTestSettle(ctx, contract.ContractId, 11)
		var reruns atomic.Int32
		ctx = server.Testing_WithTxRerunHook(ctx, func() { reruns.Add(1) })
		attemptCtx, cancelAttempt := context.WithCancel(ctx)
		defer cancelAttempt()
		key := server.NewPgOwnershipKey("transfer_balance", fixture.balanceId)
		committed, continueRelease := make(chan struct{}), make(chan struct{})
		var continueOnce sync.Once
		resume := func() { continueOnce.Do(func() { close(continueRelease) }) }
		var paused atomic.Bool
		var waiting, refused, admitted atomic.Int32
		observed := server.Testing_WithPgOwnershipObservation(attemptCtx, func(event server.PgOwnershipEvent) {
			if !slices.Contains(event.Keys, key) {
				return
			}
			switch event.Kind {
			case server.PgOwnershipAdmitted:
				admitted.Add(1)
			case server.PgOwnershipReleased:
				if paused.CompareAndSwap(false, true) {
					close(committed)
					select {
					case <-continueRelease:
					case <-attemptCtx.Done():
					}
				}
			case server.PgOwnershipWaiting:
				waiting.Add(1)
				cancelAttempt()
			case server.PgOwnershipRefused:
				refused.Add(1)
			}
		})
		type flushResult struct {
			applied  int
			released int
			busy     bool
			err      error
		}
		done := make(chan flushResult, 1)
		go func() {
			applied, released, busy, err := flushTransferDebitBalance(observed, fixture.balanceId)
			done <- flushResult{applied: applied, released: released, busy: busy, err: err}
		}()
		joined := false
		defer func() {
			resume()
			cancelAttempt()
			if !joined {
				select {
				case <-done:
				case <-time.After(10 * time.Second):
					t.Error("debit cleanup participant did not join")
				}
			}
		}()
		select {
		case <-committed:
		case result := <-done:
			joined = true
			t.Fatal("debit did not reach acknowledged financial commit", result)
		case <-ctx.Done():
			t.Fatal("debit commit barrier did not arrive", ctx.Err())
		}
		release := holdTransferBalanceTestOwner(t, ctx, []server.Id{fixture.balanceId})
		defer release()
		resume()
		var result flushResult
		select {
		case result = <-done:
			joined = true
		case <-ctx.Done():
			t.Fatal("refused cleanup did not return", ctx.Err())
		}
		if result.err != nil || result.applied != 1 || result.released != 0 || !result.busy ||
			waiting.Load() != 0 || refused.Load() != 1 || admitted.Load() != 1 {
			t.Fatal("known busy cleanup waited, entered deletion SQL or forgot the committed debit", result, waiting.Load(), refused.Load(), admitted.Load())
		}
		credit, pending, applied := asyncDebitTestState(t, ctx, fixture.balanceId)
		if credit != 989 || pending != 0 || applied != 1 || Testing_NetEscrowByteCount(ctx, fixture.balanceId) != 200 {
			t.Fatal("cleanup refusal lost applied journal custody or repeated Redis release", credit, pending, applied)
		}
		release()
		count, released, busy, err := flushTransferDebitBalance(ctx, fixture.balanceId)
		if err != nil || count != 0 || released != 1 || busy {
			t.Fatal("cleanup recovery did not delete its applied journal without another debit", count, released, busy, err)
		}
		count, released, busy, err = flushTransferDebitBalance(ctx, fixture.balanceId)
		credit, pending, applied = asyncDebitTestState(t, ctx, fixture.balanceId)
		if err != nil || count != 0 || released != 0 || busy || credit != 989 || pending+applied != 0 ||
			Testing_NetEscrowByteCount(ctx, fixture.balanceId) != 200 || reruns.Load() != 0 {
			t.Fatal("cleanup replay changed accounting or neighboring reservation", count, released, busy, err, credit, pending, applied)
		}
	})
}
