// An expiry page leaves a held payer's due rows to the holder without locking
// them, while other payers' due rows close on the same page.
package model

import (
	"context"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
)

// Counts locked statements whose first argument is one of the contracts.
type forceCloseLockTrace struct {
	stateLock   sync.Mutex
	contractIds []server.Id
	locked      int
}

func (self *forceCloseLockTrace) record(sql string, args []any) {
	if !strings.Contains(sql, "FOR UPDATE") || len(args) == 0 {
		return
	}
	contractId, ok := args[0].(server.Id)
	if !ok {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if slices.Contains(self.contractIds, contractId) {
		self.locked++
	}
}

func (self *forceCloseLockTrace) count() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.locked
}

func (self *forceCloseLockTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	self.record(data.SQL, data.Args)
	return ctx
}

func (self *forceCloseLockTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

func (self *forceCloseLockTrace) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	return ctx
}

func (self *forceCloseLockTrace) TraceBatchQuery(_ context.Context, _ *pgx.Conn, data pgx.TraceBatchQueryData) {
	self.record(data.SQL, data.Args)
}

func (self *forceCloseLockTrace) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {
}

// Another closer holds one payer's grant while an expiry page reaches three of
// its contracts past their stored deadline, beside a due contract of another
// payer. The held payer's first row tries the grant once, before any lock;
// its other rows skip without a try. All three stay open as completed visits
// counted owner_busy, and the page succeeds with the other payer's contract
// closed. Each held row used to lock its contract, read its settlement rows,
// refuse under the lock with an error report, and fail the page.
func TestForceClosePageLeavesAHeldPayersDueRowsWithoutLockingThem(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()
		held := newNetEscrowOrderingTestFixture(t, ctx)
		other := newNetEscrowOrderingTestFixture(t, ctx)
		heldIds := []server.Id{}
		for _, amount := range []ByteCount{100, 200, 300} {
			heldIds = append(heldIds, createRedisAdmissionTest(ctx, held, amount).ContractId)
		}
		otherId := createRedisAdmissionTest(ctx, other, 100).ContractId
		now := server.NowUtc()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=$2,expiration_time=$3 WHERE contract_id=ANY($1)`,
				append(slices.Clone(heldIds), otherId), now.Add(-2*time.Hour), now.Add(-time.Hour)))
		}, server.TxReadCommitted, server.OptNoRetry())

		// The scope must precede every connection the test holds.
		trace := &forceCloseLockTrace{contractIds: heldIds}
		scope, err := server.NewTestPgQueryScope(ctx, trace)
		server.Raise(err)
		scopeClosed := false
		defer func() {
			if !scopeClosed {
				server.Raise(scope.Close())
			}
		}()
		grant := server.NewPgOwnershipKey("transfer_balance", held.balanceId)
		admitted, release, holderDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
		go func() {
			defer close(holderDone)
			server.OwnedTx(ctx, []server.PgOwnershipKey{grant}, func(tx server.PgTx) {
				close(admitted)
				<-release
			}, server.TxReadCommitted, server.OptNoRetry())
		}()
		<-admitted
		released := false
		defer func() {
			if !released {
				close(release)
			}
			<-holderDone
		}()

		var refused atomic.Int32
		observed := server.Testing_WithPgOwnershipObservation(ctx, func(event server.PgOwnershipEvent) {
			if event.Kind == server.PgOwnershipRefused && slices.Contains(event.Keys, grant) {
				refused.Add(1)
			}
		})
		busyBefore := forceCloseCount("owner_busy")
		count, cursor, _, pageErr := forceCloseOpenContractIdsPage(observed, now, 10, 2, 0, 0, nil)
		close(release)
		released = true
		<-holderDone
		scopeClosed = true
		server.Raise(scope.Close())

		if pageErr != nil || cursor != nil {
			t.Error("a held payer's due rows failed the page", pageErr, cursor != nil)
		}
		if locked := trace.count(); locked != 0 {
			t.Error("held payer's due rows were locked before their grant was tried", locked)
		}
		if tries := refused.Load(); tries != 1 {
			t.Error("a held payer's grant was tried per row instead of once per page", tries)
		}
		if busy := forceCloseCount("owner_busy") - busyBefore; busy != float64(len(heldIds)) {
			t.Error("held rows were not counted owner_busy", busy)
		}
		for _, id := range heldIds {
			if _, terminal := GetContractClose(ctx, id); terminal {
				t.Fatal("a held payer's row closed under another closer's grant")
			}
		}
		if close, terminal := GetContractClose(ctx, otherId); !terminal || close.Outcome != ContractOutcomeSettled || count != 1 {
			t.Fatal("another payer's due row did not close on the same page", count)
		}
	})
}
