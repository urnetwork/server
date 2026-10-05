// Actual admission, observation and settlement owners supply each original.
// Clock and row-lock controls force the boundary rather than guessing timing.
package model

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urfoundation/sn/protocol"
	"github.com/urnetwork/server"
)

// Use the real database clock after admission. Waiting for a clock boundary is
// distinct from the explicit database blocking witness used for concurrency.
func providerWorkOpenTestBoundary(t testing.TB, f *providerWorkSessionFixture, id server.Id) time.Time {
	t.Helper()
	reservation := providerWorkFixtureReservation(t, providerWorkFixtureReceipts(t, f.ctx, id), id)
	ctx, cancel := context.WithTimeout(f.ctx, time.Minute)
	defer cancel()
	for {
		var boundary time.Time
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT clock_timestamp() AT TIME ZONE 'UTC'`).Scan(&boundary))
		})
		if boundary.UnixMicro() > reservation.Reservation.CreatedAtUnixMicro {
			return boundary
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
}

func TestProviderWorkOpenObservationActualOwnerRetainsFirstBoundary(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkSessionFixture(t)
		id := f.contract(t)
		boundary := providerWorkOpenTestBoundary(t, f, id)
		blockHash := [32]byte{131}
		originals, err := RetainProviderWorkOpenObservations(f.ctx, []server.Id{id}, 51, 501, blockHash, boundary)
		if err != nil || len(originals) != 1 {
			t.Fatal("actual open contract did not retain its first observation", len(originals), err)
		}
		original, err := protocol.DecodeProviderWorkReceipt(f.ctx, originals[0])
		if err != nil || original.Open == nil {
			t.Fatal("live observation did not retain canonical original bytes", err)
		}
		if err := protocol.VerifyProviderWorkReceiptAuthority(f.ctx, original, f.source.authority); err != nil {
			t.Fatal(err)
		}
		reservation := providerWorkFixtureReservation(t, providerWorkFixtureReceipts(t, f.ctx, id), id)
		hash, err := reservation.ContentHash(f.ctx)
		if err != nil || original.Open.ReservationHash != hash || original.Open.BoundaryUnixMicro != boundary.UnixMicro() || original.Open.ObservedAtUnixMicro < boundary.UnixMicro() {
			t.Fatal("observation lost its exact reservation or independently observed clock", original.Open, err)
		}
		retried, err := RetainProviderWorkOpenObservations(f.ctx, []server.Id{id}, 51, 501, blockHash, boundary)
		if err != nil || len(retried) != 1 || !bytes.Equal(retried[0], originals[0]) {
			t.Fatal("retry of a delivered or lost first reply minted a newer open tuple", err)
		}
		server.Db(f.ctx, func(conn server.PgConn) {
			var unresolved bool
			server.Raise(conn.QueryRow(f.ctx, `SELECT outcome IS NULL FROM transfer_contract WHERE contract_id=$1`, id).Scan(&unresolved))
			if !unresolved {
				t.Fatal("open observation invented a lifecycle transition")
			}
		})
		f.close(t, id)
		for _, receipt := range providerWorkFixtureReceipts(t, f.ctx, id) {
			if receipt.Outcome != nil && receipt.Outcome.ClosedAtUnixMicro < original.Open.ObservedAtUnixMicro {
				t.Fatal("later terminal original moved before the open observation")
			}
		}
		// Reading an original does not require its old private signer to exist.
		replayed, err := RetainProviderWorkOpenObservations(WithProviderWorkSessionSource(f.ctx, nil), []server.Id{id}, 51, 501, blockHash, boundary)
		if err != nil || len(replayed) != 1 || !bytes.Equal(replayed[0], originals[0]) {
			t.Fatal("later close or absent signer replaced the first original", err)
		}
		changed := original
		body := *original.Open
		body.ContractId = server.NewId().String()
		changed.Open = &body
		if err := changed.Verify(f.ctx); !errors.Is(err, protocol.ErrProviderWorkIntegrity) {
			t.Fatal("changed original open contract retained its authority", err)
		}
		for _, wrong := range []struct {
			block    uint64
			boundary time.Time
		}{{block: 502, boundary: boundary}, {block: 501, boundary: boundary.Add(time.Microsecond)}} {
			if _, err := RetainProviderWorkOpenObservations(f.ctx, []server.Id{id}, 51, wrong.block, blockHash, wrong.boundary); !errors.Is(err, protocol.ErrProviderWorkIntegrity) {
				t.Fatal("retained boundary identity was reinterpreted", err)
			}
		}
		var rewriteErr error
		server.HandleError(func() {
			server.Tx(f.ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(f.ctx, `UPDATE provider_work_open_original SET original=original WHERE contract_id=$1`, id))
			})
		}, func(err error) { rewriteErr = err })
		if rewriteErr == nil {
			t.Fatal("retained observation permitted a mutable rewrite")
		}
	})
}

func TestProviderWorkOpenObservationNeverBackdatesAClosedContract(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkSessionFixture(t)
		id := f.contract(t)
		boundary := providerWorkOpenTestBoundary(t, f, id)
		f.close(t, id)
		originals, err := RetainProviderWorkOpenObservations(f.ctx, []server.Id{id}, 52, 502, [32]byte{132}, boundary)
		if err != nil || len(originals) != 0 {
			t.Fatal("first acquisition after actual close fabricated an earlier open observation", len(originals), err)
		}
	})
}

func TestProviderWorkOpenObservationMissingOriginalRequestStaysUnknown(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkSessionFixture(t)
		id, err := CreateContractNoEscrow(f.ctx, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, 121)
		if err != nil {
			t.Fatal(err)
		}
		boundary := providerWorkOpenTestBoundary(t, f, id)
		originals, err := RetainProviderWorkOpenObservations(f.ctx, []server.Id{id}, 53, 503, [32]byte{133}, boundary)
		if err != nil || len(originals) != 0 {
			t.Fatal("missing original request acquired an original open boundary", len(originals), err)
		}
	})
}

func TestProviderWorkOpenObservationUnavailableAuthorityAndFutureBoundaryStayUnknown(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkSessionFixture(t)
		id := f.contract(t)
		boundary := providerWorkOpenTestBoundary(t, f, id)
		authorities := []protocol.ProviderWorkSourceAuthority{f.source.authority, f.source.authority, f.source.authority}
		authorities[0].ThroughUnixMicro = boundary.UnixMicro()
		authorities[1].FromUnixMicro = boundary.Add(time.Hour).UnixMicro()
		authorities[1].ThroughUnixMicro = boundary.Add(2 * time.Hour).UnixMicro()
		authorities[2].DomainHash[0] ^= 1
		sources := []*ProviderWorkSessionSource{nil}
		for _, authority := range authorities {
			source, err := NewProviderWorkSessionSource(authority, f.source.key)
			if err != nil {
				t.Fatal(err)
			}
			sources = append(sources, source)
		}
		for index, source := range sources {
			originals, err := RetainProviderWorkOpenObservations(WithProviderWorkSessionSource(f.ctx, source), []server.Id{id}, uint64(54+index), 504, [32]byte{134}, boundary)
			if err != nil || len(originals) != 0 {
				t.Fatal("absent, expired, future or foreign-domain source minted an observation", index, len(originals), err)
			}
		}
		originals, err := RetainProviderWorkOpenObservations(f.ctx, []server.Id{id}, 58, 508, [32]byte{138}, boundary.Add(time.Hour))
		if err != nil || len(originals) != 0 {
			t.Fatal("live owner predicted a future open boundary", len(originals), err)
		}
	})
}

func exerciseProviderWorkOpenFence(t *testing.T, cancelWait bool) {
	t.Helper()
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkSessionFixture(t)
		id := f.contract(t)
		boundary := providerWorkOpenTestBoundary(t, f, id)
		ctx, cancel := context.WithTimeout(f.ctx, 2*time.Minute)
		defer cancel()
		observerCtx, cancelObserver := context.WithCancel(ctx)
		defer cancelObserver()
		type result struct {
			originals [][]byte
			err       error
		}
		done := make(chan result, 1)
		joined := make(chan struct{})
		started := false
		defer func() {
			cancelObserver()
			cancel()
			if started {
				select {
				case <-joined:
				case <-time.After(90 * time.Second):
					t.Error("open observer did not join bounded cancellation cleanup")
				}
			}
		}()
		server.Db(ctx, func(conn server.PgConn) {
			tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
			if err != nil {
				t.Fatal(err)
			}
			defer rollbackCloseReportTestTransaction(ctx, tx)
			for _, party := range []server.Id{f.sourceId, f.destinationId} {
				if _, _, err := applyContractCloseReportInTx(ctx, tx, id, party, 121, false, nil); err != nil {
					t.Fatal(err)
				}
			}
			var ownerPid int
			server.Raise(tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&ownerPid))
			// These live lock views do not cache pg_stat_activity's earlier
			// transaction snapshot. Only this test's public observer is started.
			const waitingSql = `SELECT EXISTS(SELECT 1 FROM pg_locks waiter JOIN pg_locks owner
 ON owner.locktype=waiter.locktype AND owner.transactionid=waiter.transactionid
 WHERE owner.pid=$1 AND owner.locktype='transactionid' AND owner.granted
 AND NOT waiter.granted AND waiter.pid<>owner.pid AND $1=ANY(pg_blocking_pids(waiter.pid)))`
			var beforeWorker bool
			server.Raise(tx.QueryRow(ctx, waitingSql, ownerPid).Scan(&beforeWorker))
			if beforeWorker {
				t.Fatal("fixture already had an unrelated waiter on the terminal owner")
			}
			started = true
			go func() {
				defer close(joined)
				originals, err := RetainProviderWorkOpenObservations(observerCtx, []server.Id{id}, 59, 509, [32]byte{139}, boundary)
				done <- result{originals: originals, err: err}
			}()
			for {
				select {
				case value := <-done:
					t.Fatal("public observer crossed the actual settlement row fence", value.err, len(value.originals))
				default:
				}
				var blocked bool
				server.Raise(tx.QueryRow(ctx, waitingSql, ownerPid).Scan(&blocked))
				if blocked {
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-time.After(time.Millisecond):
				}
			}
			if cancelWait {
				cancelObserver()
				select {
				case value := <-done:
					if !errors.Is(value.err, context.Canceled) || len(value.originals) != 0 {
						t.Fatal("canceled fenced observer retained a partial write", len(value.originals), value.err)
					}
				case <-ctx.Done():
					t.Fatal("canceled observer did not release its bounded owner", ctx.Err())
				}
				var count int
				server.Raise(tx.QueryRow(ctx, `SELECT count(*) FROM provider_work_open_original WHERE contract_id=$1`, id).Scan(&count))
				if count != 0 {
					t.Fatal("canceled original acquisition leaked a retained tuple", count)
				}
				server.Raise(tx.Rollback(ctx))
				return
			}
			claimed, err := claimContractOutcomeInTx(ctx, tx, id, ContractOutcomeSettled)
			if err != nil || !claimed {
				t.Fatal("actual terminal owner did not claim its outcome", claimed, err)
			}
			server.Raise(tx.Commit(ctx))
		})
		if cancelWait {
			first, err := RetainProviderWorkOpenObservations(f.ctx, []server.Id{id}, 59, 509, [32]byte{139}, boundary)
			if err != nil || len(first) != 1 {
				t.Fatal("healthy retry did not acquire the still-open original", len(first), err)
			}
			replayed, err := RetainProviderWorkOpenObservations(f.ctx, []server.Id{id}, 59, 509, [32]byte{139}, boundary)
			if err != nil || len(replayed) != 1 || !bytes.Equal(first[0], replayed[0]) {
				t.Fatal("exact retry did not reconcile the first retained tuple", err)
			}
			return
		}
		select {
		case value := <-done:
			if value.err != nil || len(value.originals) != 0 {
				t.Fatal("observer certified its pre-fence unresolved snapshot", len(value.originals), value.err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	})
}

func TestProviderWorkOpenObservationWaitsForActualTerminalOwner(t *testing.T) {
	exerciseProviderWorkOpenFence(t, false)
}

func TestProviderWorkOpenObservationCanceledWaitReconcilesExactRetry(t *testing.T) {
	exerciseProviderWorkOpenFence(t, true)
}

func TestProviderWorkOpenObservationDeadlinePreservesApprovedBudgetAndParent(t *testing.T) {
	parent := WithProviderWorkSessionSource(context.Background(), nil)
	before := time.Now()
	ctx, cancel := providerWorkOpenContext(parent)
	defer cancel()
	after := time.Now()
	deadline, ok := ctx.Deadline()
	if !ok || deadline.Before(before.Add(300*time.Second)) || deadline.After(after.Add(300*time.Second)) {
		t.Fatal("open observation shortened the approved expected-read owner", deadline)
	}
	parentDeadline := before.Add(60 * time.Second)
	shortParent, shortCancel := context.WithDeadline(parent, parentDeadline)
	defer shortCancel()
	short, stop := providerWorkOpenContext(shortParent)
	defer stop()
	if deadline, ok := short.Deadline(); !ok || !deadline.Equal(parentDeadline) {
		t.Fatal("open observation extended its caller deadline", deadline)
	}
	shortCancel()
	if !errors.Is(short.Err(), context.Canceled) {
		t.Fatal("open observation escaped caller cancellation", short.Err())
	}
}

// This instance substitutes only the database clock result; all contract locks,
// report reads, SQL writes and original signing remain their actual owners.
type providerWorkOpenClockTx struct {
	server.PgTx
	at     time.Time
	called bool
}

func (self *providerWorkOpenClockTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if sql == `SELECT clock_timestamp() AT TIME ZONE 'UTC'` {
		self.called = true
		return providerWorkOpenClockRow{at: self.at}
	}
	return self.PgTx.QueryRow(ctx, sql, args...)
}

type providerWorkOpenClockRow struct{ at time.Time }

func (self providerWorkOpenClockRow) Scan(values ...any) error {
	if len(values) != 1 {
		return errors.New("synthetic database clock destination differs")
	}
	destination, ok := values[0].(*time.Time)
	if !ok {
		return errors.New("synthetic database clock destination is not time")
	}
	*destination = self.at
	return nil
}

func TestProviderWorkOpenObservationAndOutcomeUseDatabaseClock(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkSessionFixture(t)
		id := f.contract(t)
		boundary := providerWorkOpenTestBoundary(t, f, id)
		originals, err := RetainProviderWorkOpenObservations(f.ctx, []server.Id{id}, 60, 510, [32]byte{140}, boundary)
		if err != nil || len(originals) != 1 {
			t.Fatal("actual open observation missing", err)
		}
		original, err := protocol.DecodeProviderWorkReceipt(f.ctx, originals[0])
		if err != nil {
			t.Fatal(err)
		}
		// A database clock ahead of the handler reproduces the cross-clock
		// risk without replacing any process-global clock or live dependency.
		databaseTime := time.UnixMicro(original.Open.ObservedAtUnixMicro).Add(time.Minute)
		server.Tx(f.ctx, func(tx server.PgTx) {
			for _, party := range []server.Id{f.sourceId, f.destinationId} {
				if _, _, err := applyContractCloseReportInTx(f.ctx, tx, id, party, 121, false, nil); err != nil {
					t.Fatal(err)
				}
			}
			clockTx := &providerWorkOpenClockTx{PgTx: tx, at: databaseTime}
			claimed, err := claimContractOutcomeInTx(f.ctx, clockTx, id, ContractOutcomeSettled)
			if err != nil || !claimed || !clockTx.called {
				t.Fatal("terminal owner substituted its handler clock", claimed, clockTx.called, err)
			}
		}, server.TxReadCommitted)
		found := false
		for _, receipt := range providerWorkFixtureReceipts(t, f.ctx, id) {
			if receipt.Outcome != nil {
				found = true
				if receipt.Outcome.ClosedAtUnixMicro != databaseTime.UnixMicro() {
					t.Fatal("terminal original lost the fenced database clock", receipt.Outcome)
				}
			}
		}
		if !found {
			t.Fatal("terminal clock owner did not retain its original")
		}
		server.Db(f.ctx, func(conn server.PgConn) {
			var retained time.Time
			server.Raise(conn.QueryRow(f.ctx, `SELECT close_time FROM transfer_contract WHERE contract_id=$1`, id).Scan(&retained))
			if !retained.Equal(databaseTime) {
				t.Fatal("financial row and original outcome use different clocks")
			}
		})
	})
}
