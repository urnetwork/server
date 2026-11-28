// A debit flush behind deadline close turns that hold its grant's owner back
// to back. A flush that only probed the owner met it held on every pass, so
// its grant applied no batch and deleted no applied batch for hours
// (2026-11-25). Queued, the flush is admitted when a turn releases the owner,
// ahead of the turn that follows, for the apply and again for the cleanup.
package model

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Whether the backend waits for an advisory lock. A flush's admission session
// holds no other lock, so its one waiting advisory lock is the owner key.
func transferDebitTestOwnerQueued(t testing.TB, ctx context.Context, backendPid uint32) (queued bool) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks
			WHERE pid=$1 AND locktype='advisory' AND NOT granted)`, backendPid).Scan(&queued))
	}, server.OptNoRetry())
	return
}

// One probe of the grant's owner, as a deadline close turn admits it.
func transferDebitTestTurnProbe(ctx context.Context, balanceId server.Id) bool {
	return server.TryOwnedTx(ctx, transferBalanceOwnershipKeys([]server.Id{balanceId}), func(server.PgTx) {},
		server.TxReadCommitted, server.OptNoRetry())
}

// Turn A holds the owner when the flush starts. The flush must queue rather
// than skip; when A releases, the next turn's probe is refused and the flush
// applies. Turn C takes the owner in the gap before the cleanup; the cleanup
// queues, and when C releases, the next probe is refused again and the
// cleanup deletes. Each step is ordered by an ownership event or by the
// flush's own waiting lock in pg_locks, never by elapsed time; the seam lifts
// the wait's bound above the test's deadline.
func TestAsyncDebitFlushQueuesBehindBackToBackOwnerTurns(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		contract := createRedisAdmissionTest(ctx, f, 100)
		_ = createRedisAdmissionTest(ctx, f, 200)
		_ = asyncDebitTestSettle(ctx, contract.ContractId, 11)
		key := server.NewPgOwnershipKey("transfer_balance", f.balanceId)

		releaseTurnA := holdTransferBalanceTestOwner(t, ctx, []server.Id{f.balanceId})

		type gate struct {
			reached  chan struct{}
			proceed  chan struct{}
			once     sync.Once
			released sync.Once
		}
		newGate := func() *gate { return &gate{reached: make(chan struct{}), proceed: make(chan struct{})} }
		pass := func(g *gate) {
			g.once.Do(func() { close(g.reached) })
			select {
			case <-g.proceed:
			case <-ctx.Done():
			}
		}
		open := func(g *gate) { g.released.Do(func() { close(g.proceed) }) }
		applyAdmitted, applyReleased, cleanupAdmitted := newGate(), newGate(), newGate()
		var stateLock sync.Mutex
		var waitingPids []uint32
		var refused, admitted, released int
		observed := server.Testing_WithPgOwnershipObservation(testingWithTransferDebitOwnerWait(ctx, time.Minute), func(event server.PgOwnershipEvent) {
			if !slices.Contains(event.Keys, key) {
				return
			}
			stateLock.Lock()
			var hold *gate
			switch event.Kind {
			case server.PgOwnershipWaiting:
				waitingPids = append(waitingPids, event.BackendPid)
			case server.PgOwnershipRefused:
				refused++
			case server.PgOwnershipAdmitted:
				admitted++
				hold = map[int]*gate{1: applyAdmitted, 2: cleanupAdmitted}[admitted]
			case server.PgOwnershipReleased:
				released++
				if released == 1 {
					hold = applyReleased
				}
			}
			stateLock.Unlock()
			if hold != nil {
				pass(hold)
			}
		})
		defer func() {
			for _, g := range []*gate{applyAdmitted, applyReleased, cleanupAdmitted} {
				open(g)
			}
		}()

		type flushResult struct {
			applied  int
			released int
			busy     bool
			err      error
		}
		done := make(chan flushResult, 1)
		go func() {
			applied, releasedCount, busy, err := flushTransferDebitBalance(observed, f.balanceId)
			done <- flushResult{applied: applied, released: releasedCount, busy: busy, err: err}
		}()
		joined := false
		defer func() {
			if !joined {
				select {
				case <-done:
				case <-time.After(10 * time.Second):
					t.Error("debit flush did not join")
				}
			}
		}()
		flushReturned := func(result flushResult, while string) {
			joined = true
			t.Fatalf("the flush returned %s instead of waiting for the owner: applied=%d released=%d busy=%t err=%v",
				while, result.applied, result.released, result.busy, result.err)
		}
		// The flush's admission session waits in the owner's lock queue.
		awaitQueued := func(waiter int, while string) {
			t.Helper()
			ticker := time.NewTicker(time.Millisecond)
			defer ticker.Stop()
			for {
				stateLock.Lock()
				var pid uint32
				if waiter < len(waitingPids) {
					pid = waitingPids[waiter]
				}
				stateLock.Unlock()
				if pid != 0 && transferDebitTestOwnerQueued(t, ctx, pid) {
					return
				}
				select {
				case result := <-done:
					flushReturned(result, while)
				case <-ctx.Done():
					t.Fatal("the flush did not queue for the owner", while, ctx.Err())
				case <-ticker.C:
				}
			}
		}
		await := func(g *gate, while string) {
			t.Helper()
			select {
			case <-g.reached:
			case result := <-done:
				flushReturned(result, while)
			case <-ctx.Done():
				t.Fatal("the flush did not reach its admission", while, ctx.Err())
			}
		}

		awaitQueued(0, "while turn A held the owner")
		releaseTurnA()
		// A held the owner until its release returned, and that release
		// admitted the queued flush, which now holds the owner at its gate.
		await(applyAdmitted, "after turn A")
		if transferDebitTestTurnProbe(ctx, f.balanceId) {
			t.Fatal("the turn after A was admitted ahead of the queued flush")
		}
		open(applyAdmitted)

		// The apply committed and released the owner; turn C takes it
		// before the cleanup asks again.
		await(applyReleased, "after the apply")
		releaseTurnC := holdTransferBalanceTestOwner(t, ctx, []server.Id{f.balanceId})
		open(applyReleased)
		awaitQueued(1, "while turn C held the owner")
		releaseTurnC()
		await(cleanupAdmitted, "after turn C")
		if transferDebitTestTurnProbe(ctx, f.balanceId) {
			t.Fatal("the turn after C was admitted ahead of the queued cleanup")
		}
		open(cleanupAdmitted)

		var result flushResult
		select {
		case result = <-done:
			joined = true
		case <-ctx.Done():
			t.Fatal("the flush did not finish", ctx.Err())
		}
		stateLock.Lock()
		waits, refusals, admissions := len(waitingPids), refused, admitted
		stateLock.Unlock()
		if result.err != nil || result.applied != 1 || result.released != 1 || result.busy ||
			waits != 2 || refusals != 0 || admissions != 2 {
			t.Fatal("the queued flush did not apply and delete its debit once", result, waits, refusals, admissions)
		}
		credit, pending, applied := asyncDebitTestState(t, ctx, f.balanceId)
		if credit != 989 || pending+applied != 0 || Testing_NetEscrowByteCount(ctx, f.balanceId) != 200 {
			t.Fatal("the queued flush changed accounting or the neighboring reservation", credit, pending, applied)
		}
	})
}

// An owner held for longer than the bound: the flush waits out its bound and
// leaves the grant for a later pass, with no debit and no deletion; the
// released owner then flushes the retained journal exactly once.
func TestAsyncDebitFlushQueueGivesUpAfterItsBound(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		contract := createRedisAdmissionTest(ctx, f, 100)
		_ = asyncDebitTestSettle(ctx, contract.ContractId, 11)
		key := server.NewPgOwnershipKey("transfer_balance", f.balanceId)
		release := holdTransferBalanceTestOwner(t, ctx, []server.Id{f.balanceId})
		var waiting, refused, admitted int
		observed := server.Testing_WithPgOwnershipObservation(testingWithTransferDebitOwnerWait(ctx, time.Millisecond), func(event server.PgOwnershipEvent) {
			if !slices.Contains(event.Keys, key) {
				return
			}
			switch event.Kind {
			case server.PgOwnershipWaiting:
				waiting++
			case server.PgOwnershipRefused:
				refused++
			case server.PgOwnershipAdmitted:
				admitted++
			}
		})
		applied, released, busy, err := flushTransferDebitBalance(observed, f.balanceId)
		if err != nil || applied != 0 || released != 0 || !busy || waiting != 1 || refused != 1 || admitted != 0 {
			t.Fatal("a flush behind a held owner did not wait its bound and skip", applied, released, busy, err, waiting, refused, admitted)
		}
		credit, pending, journalApplied := asyncDebitTestState(t, ctx, f.balanceId)
		if credit != 1000 || pending != 1 || journalApplied != 0 || Testing_NetEscrowByteCount(ctx, f.balanceId) != 100 {
			t.Fatal("a skipped flush changed accounting", credit, pending, journalApplied)
		}
		release()
		applied, released, busy, err = flushTransferDebitBalance(ctx, f.balanceId)
		credit, pending, journalApplied = asyncDebitTestState(t, ctx, f.balanceId)
		if err != nil || applied != 1 || released != 1 || busy || credit != 989 || pending+journalApplied != 0 ||
			Testing_NetEscrowByteCount(ctx, f.balanceId) != 0 {
			t.Fatal("the released owner did not flush the retained debit once", applied, released, busy, err, credit, pending, journalApplied)
		}
	})
}
