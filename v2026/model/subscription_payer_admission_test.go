package model

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

func payerQueueCreate(ctx context.Context, f netEscrowOrderingTestFixture, bytes ByteCount) (escrow *TransferEscrow, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			if e, ok := recovered.(error); ok {
				err = e
			} else {
				err = fmt.Errorf("unexpected admission panic type %T", recovered)
			}
		}
	}()
	return createTransferEscrow(ctx, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, bytes)
}

func payerQueueReferences(q *payerAdmissionQueue, payer server.Id) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	if turn := q.turns[payer]; turn != nil {
		return turn.refs
	}
	return 0
}

func awaitPayerQueueReferences(t testing.TB, ctx context.Context, q *payerAdmissionQueue, payer server.Id, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		if payerQueueReferences(q, payer) == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("payer queue references=%d, want%d", payerQueueReferences(q, payer), want)
}

func TestPayerAdmissionQueueCanceledWaitersReleaseReferences(t *testing.T) {
	var q payerAdmissionQueue
	payer := server.NewId()
	release, err := q.acquire(t.Context(), payer)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	const waiters = 100
	done := make(chan error, waiters)
	for range waiters {
		go func() {
			release, err := q.acquire(ctx, payer)
			if release != nil {
				release()
			}
			done <- err
		}()
	}
	awaitPayerQueueReferences(t, t.Context(), &q, payer, waiters+1)
	cancel()
	for range waiters {
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("queued cancellation=%v", err)
		}
	}
	awaitPayerQueueReferences(t, t.Context(), &q, payer, 1)
	release()
	release() // release is idempotent and cannot return a second token.
	awaitPayerQueueReferences(t, t.Context(), &q, payer, 0)
	for range 100 {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if release, err := q.acquire(ctx, payer); release != nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("already canceled acquisition=%v", err)
		}
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.turns) != 0 {
		t.Fatal("canceled requests retained payer entries")
	}
}

func TestPayerAdmissionQueueCancellationDifferentPayerAndZero(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		other := newNetEscrowOrderingTestFixture(t, ctx)
		blocker := acquireContractLifecycleTestConnection(t, ctx)
		defer blocker.Release()
		held, err := blocker.Begin(ctx)
		server.Raise(err)
		defer held.Rollback(context.Background())
		server.RaisePgResult(held.Exec(ctx, `SELECT 1 FROM transfer_balance WHERE balance_id=$1 FOR UPDATE`, f.balanceId))
		first := make(chan error, 1)
		go func() { _, err := payerQueueCreate(ctx, f, 1); first <- err }()
		awaitPayerQueueReferences(t, ctx, &transferEscrowAdmissionQueue, f.sourceNetworkId, 1)
		queuedCtx, queuedCancel := context.WithCancel(ctx)
		defer queuedCancel()
		queued := make(chan error, 1)
		go func() { _, err := payerQueueCreate(queuedCtx, f, 1); queued <- err }()
		awaitPayerQueueReferences(t, ctx, &transferEscrowAdmissionQueue, f.sourceNetworkId, 2)
		queuedCancel()
		select {
		case err := <-queued:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("queued error=%v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("canceled payer waiter did not leave promptly")
		}
		awaitPayerQueueReferences(t, ctx, &transferEscrowAdmissionQueue, f.sourceNetworkId, 1)
		independentCtx, independentCancel := context.WithTimeout(ctx, 2*time.Second)
		defer independentCancel()
		if _, err := payerQueueCreate(independentCtx, other, 1); err != nil {
			t.Fatalf("another payer was blocked by the first payer's grant: %v", err)
		}
		if _, err := payerQueueCreate(independentCtx, f, 0); err != nil {
			t.Fatalf("zero-byte anchor waited for positive admission: %v", err)
		}
		server.Raise(held.Rollback(ctx))
		if err := <-first; err != nil {
			t.Fatal(err)
		}
		awaitPayerQueueReferences(t, ctx, &transferEscrowAdmissionQueue, f.sourceNetworkId, 0)
		if got := openEscrowReservedForBalances(ctx, []server.Id{f.balanceId})[f.balanceId].reserved; got != 1 {
			t.Fatalf("canceled/zero requests changed reservation: %d", got)
		}
	})
}

func TestPayerAdmissionQueueCompanionUsesDestinationPayer(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		if _, err := payerQueueCreate(ctx, f, 1); err != nil {
			t.Fatal(err)
		}
		release, err := transferEscrowAdmissionQueue.acquire(ctx, f.sourceNetworkId)
		server.Raise(err)
		defer release()
		done := make(chan error, 2)
		go func() { _, err := payerQueueCreate(ctx, f, 1); done <- err }()
		go func() {
			_, err := createCompanionTransferEscrow(ctx, f.destinationNetworkId, f.destinationId,
				f.sourceNetworkId, f.sourceId, 1, time.Minute)
			done <- err
		}()
		awaitPayerQueueReferences(t, ctx, &transferEscrowAdmissionQueue, f.sourceNetworkId, 3)
		if got := payerQueueReferences(&transferEscrowAdmissionQueue, f.destinationNetworkId); got != 0 {
			t.Fatalf("companion queued on its non-paying source: %d", got)
		}
		release()
		for range 2 {
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("payer creators did not join")
			}
		}
		if got := openEscrowReservedForBalances(ctx, []server.Id{f.balanceId})[f.balanceId].reserved; got != 3 {
			t.Fatalf("primary/companion reservations=%d, want3", got)
		}
		awaitPayerQueueReferences(t, ctx, &transferEscrowAdmissionQueue, f.sourceNetworkId, 0)
	})
}

func TestPayerAdmissionQueueRollbackRetryAndPosts(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
		defer cancel()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		sentinel := errors.New("payer queue rollback")
		func() {
			defer func() {
				if recovered := recover(); recovered != sentinel {
					t.Errorf("rollback panic=%v", recovered)
				}
			}()
			server.Raise(transferEscrowTx(ctx, f.sourceNetworkId, 1, func(tx server.PgTx) {
				_, _, err := createTransferEscrowInTx(ctx, tx, f.sourceNetworkId, f.sourceId,
					f.destinationNetworkId, f.destinationId, f.sourceNetworkId, 1, nil)
				server.Raise(err)
				panic(sentinel)
			}))
		}()
		awaitPayerQueueReferences(t, ctx, &transferEscrowAdmissionQueue, f.sourceNetworkId, 0)
		if got := openEscrowReservedForBalances(ctx, []server.Id{f.balanceId})[f.balanceId].reserved; got != 0 {
			t.Fatalf("panic committed reservation=%d", got)
		}

		var attempts atomic.Int32
		retryEntered := make(chan struct{})
		continueRetry := make(chan struct{})
		var unblockRetry sync.Once
		defer unblockRetry.Do(func() { close(continueRetry) })
		first := make(chan error, 1)
		var posts []func() any
		go func() {
			first <- transferEscrowTx(ctx, f.sourceNetworkId, 1, func(tx server.PgTx) {
				if attempts.Add(1) == 1 {
					server.RaisePgResult(tx.Exec(ctx, `DO $$ BEGIN RAISE SQLSTATE '40001'; END $$`))
				}
				close(retryEntered)
				select {
				case <-continueRetry:
				case <-ctx.Done():
					server.Raise(ctx.Err())
				}
				var err error
				_, posts, err = createTransferEscrowInTx(ctx, tx, f.sourceNetworkId, f.sourceId,
					f.destinationNetworkId, f.destinationId, f.sourceNetworkId, 1, nil)
				server.Raise(err)
			})
		}()
		select {
		case <-retryEntered:
		case <-ctx.Done():
			t.Fatal("transaction did not retry")
		}
		second := make(chan error, 1)
		go func() { _, err := payerQueueCreate(ctx, f, 1); second <- err }()
		awaitPayerQueueReferences(t, ctx, &transferEscrowAdmissionQueue, f.sourceNetworkId, 2)
		unblockRetry.Do(func() { close(continueRetry) })
		if err := <-first; err != nil {
			t.Fatal(err)
		}
		if err := <-second; err != nil {
			t.Fatal(err)
		}
		if attempts.Load() != 2 {
			t.Fatalf("retry attempts=%d", attempts.Load())
		}

		postEntered := make(chan struct{})
		postRelease := make(chan struct{})
		var unblockPost sync.Once
		defer unblockPost.Do(func() { close(postRelease) })
		postDone := make(chan struct{})
		go func() {
			defer close(postDone)
			server.RunPosts(ctx, func() any {
				close(postEntered)
				select {
				case <-postRelease:
				case <-ctx.Done():
				}
				server.RunPosts(ctx, posts...)
				return nil
			})
		}()
		<-postEntered
		awaitPayerQueueReferences(t, ctx, &transferEscrowAdmissionQueue, f.sourceNetworkId, 0)
		if _, err := payerQueueCreate(ctx, f, 1); err != nil {
			t.Fatalf("post-commit work retained payer turn: %v", err)
		}
		unblockPost.Do(func() { close(postRelease) })
		<-postDone
		if got := Testing_NetEscrowByteCount(ctx, f.balanceId); got != 3 {
			t.Fatalf("delayed post after retry overwrote newer committed reservation: %d", got)
		}
	})
}

// The payer is already serialized by its durable grant. Waiting requests must
// queue before taking PostgreSQL connections instead of each joining its tuple
// lock queue. Exercise the public creator, including the committed Redis posts.
func TestPayerAdmissionQueueBoundsPostgresWaiters(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
		defer cancel()
		f := seedAdmissionCacheHistory(t, ctx, 10001, 8)
		blocker := acquireContractLifecycleTestConnection(t, ctx)
		defer blocker.Release()
		held, err := blocker.Begin(ctx)
		server.Raise(err)
		defer held.Rollback(context.Background())
		server.RaisePgResult(held.Exec(ctx, `SELECT 1 FROM transfer_balance WHERE balance_id=$1 FOR UPDATE`, f.balanceId))
		observer := acquireContractLifecycleTestConnection(t, ctx)
		defer observer.Release()

		const requests = 20
		results := make(chan error, requests)
		var started sync.WaitGroup
		started.Add(requests)
		start := make(chan struct{})
		for range requests {
			go func() {
				started.Done()
				<-start
				_, err := payerQueueCreate(ctx, f, 1)
				results <- err
			}()
		}
		started.Wait()
		close(start)
		maxWaiters := 0
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			var waiting int
			server.Raise(observer.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
				WHERE datname=current_database() AND state='active' AND wait_event_type='Lock' AND query=$1`,
				escrowTransferBalanceSql+` ORDER BY balance_id FOR UPDATE`).Scan(&waiting))
			maxWaiters = max(maxWaiters, waiting)
			time.Sleep(10 * time.Millisecond)
		}
		server.Raise(held.Rollback(ctx))
		accepted, insufficient := 0, 0
		for range requests {
			select {
			case err := <-results:
				if err == nil {
					accepted++
				} else if strings.Contains(err.Error(), "Insufficient balance") {
					insufficient++
				} else {
					t.Errorf("admission: %v", err)
				}
			case <-ctx.Done():
				t.Fatal("admissions did not join after releasing the grant")
			}
		}
		if got := openEscrowReservedForBalances(ctx, []server.Id{f.balanceId})[f.balanceId].reserved; got != 10009 {
			t.Fatalf("reserved=%d, want10009", got)
		}
		t.Logf("requests=%d maximum_postgres_grant_waiters=%d accepted=%d insufficient=%d", requests, maxWaiters, accepted, insufficient)
		if maxWaiters != 1 || accepted != 8 || insufficient != 12 {
			t.Fatalf("want one PostgreSQL waiter with unchanged8/12 financial outcomes")
		}
	})
}
