// Drive the production page traversal at explicit ownership transitions. The
// timeout cause is delivered after a completed balance, without timing a sleep.
package model

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/urnetwork/server"
)

// Both pgx's wrapped deadline and the pool's historical cancellation sentinel
// must yield the completed prefix when only the worker's page budget expired.
func TestAsyncDebitPageDeadlinePreservesVisitedCursor(t *testing.T) {
	for _, lookupErr := range []error{
		context.DeadlineExceeded,
		fmt.Errorf("synthetic query wrapper: %w", context.DeadlineExceeded),
		server.DbContextDoneError,
		fmt.Errorf("synthetic pool wrapper: %w", server.DbContextDoneError),
	} {
		ctx := t.Context()
		bounded, expire := context.WithCancelCause(ctx)
		first := server.NewId()
		lookups, flushes := 0, 0
		result, err := flushTransferDebitPage(ctx, bounded, 0, nil, 64, transferDebitPageOperations{
			nextBalance: func(_ context.Context, shard int, after *server.Id) *server.Id {
				lookups++
				if shard != 0 {
					t.Fatal("page changed its partition")
				}
				if lookups == 1 {
					if after != nil {
						t.Fatal("first lookup invented a cursor")
					}
					return &first
				}
				if after == nil || *after != first {
					t.Fatal("next lookup did not follow the completed balance")
				}
				expire(errTransferDebitPageDeadline)
				panic(lookupErr)
			},
			flushBalance: func(_ context.Context, id server.Id) (int, int, bool, error) {
				flushes++
				if id != first {
					t.Fatal("page flushed an unselected balance")
				}
				return 3, 3, false, nil
			},
		})
		expire(nil)
		if err != nil || lookups != 2 || flushes != 1 || result.Balances != 1 || result.Applied != 3 ||
			result.Released != 3 || result.Busy != 0 || result.Failed != 0 || !result.More ||
			result.LastBalanceId == nil || *result.LastBalanceId != first {
			t.Errorf("lookup error %v discarded completed page: result=%+v err=%v lookups=%d flushes=%d", lookupErr, result, err, lookups, flushes)
		}
	}
}

// A busy balance is visited but not paid. It still advances the fair cursor;
// Released=0 keeps the task's existing delayed continuation policy intact.
func TestAsyncDebitPageDeadlinePreservesBusyCursor(t *testing.T) {
	ctx := t.Context()
	bounded, expire := context.WithCancelCause(ctx)
	defer expire(nil)
	first := server.NewId()
	result, err := flushTransferDebitPage(ctx, bounded, 0, nil, 64, transferDebitPageOperations{
		nextBalance: func(_ context.Context, _ int, after *server.Id) *server.Id {
			if after == nil {
				return &first
			}
			expire(errTransferDebitPageDeadline)
			panic(server.DbContextDoneError)
		},
		flushBalance: func(context.Context, server.Id) (int, int, bool, error) { return 0, 0, true, nil },
	})
	if err != nil || result.Balances != 1 || result.Busy != 1 || result.Applied != 0 || result.Released != 0 ||
		result.Failed != 0 || !result.More || result.LastBalanceId == nil || *result.LastBalanceId != first {
		t.Fatalf("busy prefix lost its fair cursor or invented accounting: result=%+v err=%v", result, err)
	}
}

// A deadline is authority to checkpoint only this page's recorded progress.
// Parent cancellation, absent progress and unrelated or mixed failures stay loud.
func TestAsyncDebitPageDeadlineKeepsForeignFailures(t *testing.T) {
	unrelated := errors.New("synthetic next-key failure")
	foreignBudget := errors.New("synthetic unrelated context owner")
	for _, c := range []struct {
		name         string
		progress     bool
		pageCause    error
		cancelParent bool
		lookupErr    error
	}{
		{name: "no_progress", pageCause: errTransferDebitPageDeadline, lookupErr: server.DbContextDoneError},
		{name: "parent_cancel", progress: true, cancelParent: true, lookupErr: server.DbContextDoneError},
		{name: "parent_and_page_cancel", progress: true, pageCause: errTransferDebitPageDeadline, cancelParent: true, lookupErr: server.DbContextDoneError},
		{name: "foreign_budget", progress: true, pageCause: foreignBudget, lookupErr: context.DeadlineExceeded},
		{name: "unexpected_query", progress: true, lookupErr: unrelated},
		{name: "unexpected_after_deadline", progress: true, pageCause: errTransferDebitPageDeadline, lookupErr: unrelated},
		{name: "mixed_error", progress: true, pageCause: errTransferDebitPageDeadline, lookupErr: errors.Join(context.DeadlineExceeded, unrelated)},
		{name: "unowned_pool_done", progress: true, lookupErr: server.DbContextDoneError},
	} {
		ctx, cancelParent := context.WithCancel(t.Context())
		bounded, expire := context.WithCancelCause(ctx)
		first, prior := server.NewId(), server.NewId()
		lookups := 0
		result, err := flushTransferDebitPage(ctx, bounded, 0, &prior, 64, transferDebitPageOperations{
			nextBalance: func(_ context.Context, _ int, after *server.Id) *server.Id {
				lookups++
				if c.progress && lookups == 1 {
					if after == nil || *after != prior {
						t.Fatal("page lost its input cursor")
					}
					return &first
				}
				if c.pageCause != nil {
					expire(c.pageCause)
				}
				if c.cancelParent {
					cancelParent()
				}
				panic(c.lookupErr)
			},
			flushBalance: func(context.Context, server.Id) (int, int, bool, error) { return 1, 1, false, nil },
		})
		expire(nil)
		cancelParent()
		if err != c.lookupErr {
			t.Errorf("%s changed failure: got %v want %v", c.name, err, c.lookupErr)
		}
		if c.progress {
			if result.LastBalanceId == nil || *result.LastBalanceId != first || result.Balances != 1 || result.Applied != 1 || result.Released != 1 {
				t.Errorf("%s corrupted the observed prefix: %+v", c.name, result)
			}
		} else if result.LastBalanceId != nil || result.Balances != 0 || result.Applied != 0 || result.Released != 0 {
			t.Errorf("%s invented progress: %+v", c.name, result)
		}
	}
}

// Exercise actual admission, settlement, debit, Redis release and journal
// deletion before interrupting the following real pool lookup. Its durable
// cursor must then resume the untouched payer without repeating either debit.
func TestAsyncDebitPageDeadlineResumesCommittedAccounting(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
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
			contract := createRedisAdmissionTest(ctx, f, 100)
			asyncDebitTestSettle(ctx, contract.ContractId, 11)
		}
		bounded, expire := context.WithCancelCause(ctx)
		defer expire(nil)
		lookups := 0
		var observedLookupErr error
		result, err := flushTransferDebitPage(ctx, bounded, transferDebitShard(first.balanceId), nil, 64, transferDebitPageOperations{
			nextBalance: func(pageCtx context.Context, shard int, after *server.Id) *server.Id {
				lookups++
				if lookups == 2 {
					credit, pending, applied := asyncDebitTestState(t, ctx, first.balanceId)
					if credit != 989 || pending != 0 || applied != 0 || Testing_NetEscrowByteCount(ctx, first.balanceId) != 0 {
						t.Fatal("deadline seam preceded complete first-balance writeback")
					}
					expire(errTransferDebitPageDeadline)
					server.HandleError(func() { nextTransferDebitBalance(pageCtx, shard, after) }, func(err error) { observedLookupErr = err })
					if observedLookupErr != server.DbContextDoneError {
						t.Fatalf("real next-key checkout did not report its cancellation: %v", observedLookupErr)
					}
					panic(observedLookupErr)
				}
				return nextTransferDebitBalance(pageCtx, shard, after)
			},
			flushBalance: flushTransferDebitBalance,
		})
		if err != nil || observedLookupErr == nil || lookups != 2 || result.LastBalanceId == nil || *result.LastBalanceId != first.balanceId ||
			result.Balances != 1 || result.Applied != 1 || result.Released != 1 || result.Failed != 0 || !result.More {
			t.Fatalf("completed writeback could not checkpoint: result=%+v err=%v", result, err)
		}
		credit, pending, applied := asyncDebitTestState(t, ctx, second.balanceId)
		if credit != 1000 || pending != 1 || applied != 0 || Testing_NetEscrowByteCount(ctx, second.balanceId) != 100 {
			t.Fatal("interrupted lookup changed unvisited payer accounting")
		}
		next, err := FlushTransferDebits(ctx, transferDebitShard(first.balanceId), result.LastBalanceId, 64)
		if err != nil || next.Balances != 1 || next.Applied != 1 || next.Released != 1 || next.Failed != 0 || next.More || next.LastBalanceId != nil {
			t.Fatalf("continuation did not finish the untouched payer: result=%+v err=%v", next, err)
		}
		for _, f := range []netEscrowOrderingTestFixture{first, second} {
			credit, pending, applied := asyncDebitTestState(t, ctx, f.balanceId)
			if credit != 989 || pending != 0 || applied != 0 || Testing_NetEscrowByteCount(ctx, f.balanceId) != 0 {
				t.Fatal("deadline continuation repeated or lost durable consumption")
			}
		}
		replay, err := FlushTransferDebits(ctx, transferDebitShard(first.balanceId), nil, 64)
		if err != nil || replay.Balances != 0 || replay.Applied != 0 || replay.Released != 0 || replay.LastBalanceId != nil || replay.More {
			t.Fatal("completed page replay invented new consumption", replay, err)
		}
	})
}
