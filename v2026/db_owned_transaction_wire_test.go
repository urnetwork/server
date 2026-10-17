// Typed pgx replies exercise ownership admission and unknown transport outcomes.
package server

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgxpool"
)

// One-key protocol rows preserve the real pgx startup backend identity. The
// fixture can deliberately switch it before BEGIN or lose one actual reply.
func ownedTransactionWireRows(backend int, query string) ([]pgproto3.FieldDescription, [][][]byte) {
	fields := []pgproto3.FieldDescription{{Name: []byte("pid"), DataTypeOID: 23, DataTypeSize: 4}}
	if strings.HasPrefix(query, "SELECT pg_backend_pid(),") {
		fields = append(fields, pgproto3.FieldDescription{Name: []byte("acquired"), DataTypeOID: 16, DataTypeSize: 1})
		return fields, [][][]byte{{[]byte(strconv.Itoa(backend)), []byte("t")}}
	}
	if query == "SELECT pg_backend_pid()" {
		return fields, [][][]byte{{[]byte(strconv.Itoa(backend))}}
	}
	return nil, nil
}

func ownedTransactionWireResource() pgOwnershipResource {
	return pgOwnershipResource{host: "synthetic-pg.example", port: 5432, database: "synthetic", user: "synthetic"}
}

func TestOwnedTxUnknownCommitDoesNotReplayOrPublish(t *testing.T) {
	fixture, pool := newPgPoolWireFixture(t, func(_ int, query string) bool { return query != "commit" },
		func(context.Context, pgxpool.ShouldPingParams) bool { return false },
		func(fixture *pgPoolWireFixture, _ *pgxpool.Config) { fixture.queryRows = ownedTransactionWireRows })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var calls, posts atomic.Int32
	var counter TxCommitCounter
	var events []PgOwnershipEventKind
	ctx = Testing_WithPgOwnershipObservation(ctx, func(event PgOwnershipEvent) { events = append(events, event.Kind) })
	err := captureDbErrorPanic(func() {
		ownedTxWithResource(ctx, []PgOwnershipKey{NewPgOwnershipKey("owned-tx-test", NewId())},
			ownedTransactionWireResource(), pool.open().Acquire, func(tx PgTx) {
				calls.Add(1)
				RaisePgResult(tx.Exec(ctx, `INSERT INTO synthetic_effect VALUES(1)`))
				AddTxCommitCount(tx, &counter, 1)
				AddTxPostCommit(tx, "must-not-publish", func() any { posts.Add(1); return nil })
			}, OptRetryDefault())
	})
	if err == nil || calls.Load() != 1 || posts.Load() != 0 {
		t.Fatal("unknown owned commit replayed or acknowledged", err)
	}
	requireTxCommitSnapshot(t, &counter, 0, 1, 0)
	if len(events) != 2 || events[0] != PgOwnershipAdmitted || events[1] != PgOwnershipUncertain {
		t.Fatal("unknown commit invented an acknowledged ownership release", events)
	}
	fixture.stateLock.Lock()
	defer fixture.stateLock.Unlock()
	commits := 0
	for _, query := range fixture.queries {
		if query == "commit" {
			commits++
		}
	}
	if commits != 1 {
		t.Fatal("unknown commit created another transaction", commits)
	}
}

// A correct resource name cannot turn a transaction pooler into a direct
// session. A backend swap after admission is refused before monetary callback.
func TestOwnedTxBackendSwapBeforeBusinessRefusesAndDiscards(t *testing.T) {
	fixture, pool := newPgPoolWireFixture(t, nil,
		func(context.Context, pgxpool.ShouldPingParams) bool { return false },
		func(fixture *pgPoolWireFixture, _ *pgxpool.Config) {
			fixture.queryRows = func(backend int, query string) ([]pgproto3.FieldDescription, [][][]byte) {
				if query == "SELECT pg_backend_pid()" {
					backend += 1000
				}
				return ownedTransactionWireRows(backend, query)
			}
		})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	calls := 0
	err := captureDbErrorPanic(func() {
		ownedTxWithResource(ctx, []PgOwnershipKey{NewPgOwnershipKey("owned-tx-test", NewId())},
			ownedTransactionWireResource(), pool.open().Acquire, func(PgTx) { calls++ })
	})
	if err == nil || calls != 0 {
		t.Fatal("changed backend entered monetary SQL", err)
	}
	fresh := RaisePgResult(pool.open().Acquire(ctx))
	fresh.Release()
	fixture.stateLock.Lock()
	defer fixture.stateLock.Unlock()
	if fixture.dialCount != 2 {
		t.Fatal("unsafe ownership backend returned to pool", fixture.dialCount)
	}
}

// A pooler's synthetic startup identity already differs at the acquisition
// statement. No business BEGIN may run on that unproved physical authority.
func TestOwnedTxIndirectStartupIdentityRefusesBeforeBegin(t *testing.T) {
	fixture, pool := newPgPoolWireFixture(t, nil,
		func(context.Context, pgxpool.ShouldPingParams) bool { return false },
		func(fixture *pgPoolWireFixture, _ *pgxpool.Config) {
			fixture.queryRows = func(backend int, query string) ([]pgproto3.FieldDescription, [][][]byte) {
				return ownedTransactionWireRows(backend+1000, query)
			}
		})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	calls := 0
	err := captureDbErrorPanic(func() {
		ownedTxWithResource(ctx, []PgOwnershipKey{NewPgOwnershipKey("owned-tx-test", NewId())},
			ownedTransactionWireResource(), pool.open().Acquire, func(PgTx) { calls++ })
	})
	if err == nil || calls != 0 {
		t.Fatal("indirect startup authority entered monetary SQL", err)
	}
	fixture.stateLock.Lock()
	defer fixture.stateLock.Unlock()
	for _, query := range fixture.queries {
		if strings.HasPrefix(query, "begin") {
			t.Fatal("indirect authority began a business transaction")
		}
	}
}

// Unknown cleanup stays secondary after an acknowledged financial commit,
// while its checkout is destroyed and the optional post still runs once.
func TestOwnedTxLostUnlockDiscardsSessionWithoutChangingCommit(t *testing.T) {
	fixture, pool := newPgPoolWireFixture(t, func(_ int, query string) bool { return query != "SELECT pg_advisory_unlock_all()" },
		func(context.Context, pgxpool.ShouldPingParams) bool { return false },
		func(fixture *pgPoolWireFixture, _ *pgxpool.Config) { fixture.queryRows = ownedTransactionWireRows })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var counter TxCommitCounter
	var posts atomic.Int32
	var events []PgOwnershipEventKind
	ctx = Testing_WithPgOwnershipObservation(ctx, func(event PgOwnershipEvent) { events = append(events, event.Kind) })
	ownedTxWithResource(ctx, []PgOwnershipKey{NewPgOwnershipKey("owned-tx-test", NewId())},
		ownedTransactionWireResource(), pool.open().Acquire, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, `INSERT INTO synthetic_effect VALUES(1)`))
			AddTxCommitCount(tx, &counter, 1)
			AddTxPostCommit(tx, "confirmed-post", func() any { posts.Add(1); return nil })
		})
	requireTxCommitSnapshot(t, &counter, 1, 0, 0)
	if len(events) != 2 || events[0] != PgOwnershipAdmitted || events[1] != PgOwnershipUncertain {
		t.Fatal("lost unlock invented an acknowledged ownership release", events)
	}
	if posts.Load() != 1 {
		t.Fatal("lost unlock erased acknowledged financial publication")
	}
	fresh := RaisePgResult(pool.open().Acquire(ctx))
	fresh.Release()
	fixture.stateLock.Lock()
	defer fixture.stateLock.Unlock()
	if fixture.dialCount != 2 {
		t.Fatal("unknown cleanup returned its old checkout", fixture.dialCount)
	}
}

func TestOwnedTxExplicitResourceMismatchRefusesBusiness(t *testing.T) {
	fixture, pool := newPgPoolWireFixture(t, nil,
		func(context.Context, pgxpool.ShouldPingParams) bool { return false },
		func(fixture *pgPoolWireFixture, _ *pgxpool.Config) { fixture.queryRows = ownedTransactionWireRows })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	resource := ownedTransactionWireResource()
	resource.host = "different-direct-pg.example"
	calls := 0
	err := captureDbErrorPanic(func() {
		ownedTxWithResource(ctx, []PgOwnershipKey{NewPgOwnershipKey("owned-tx-test", NewId())}, resource,
			pool.open().Acquire, func(PgTx) { calls++ })
	})
	if err == nil || calls != 0 {
		t.Fatal("cached fallback pool entered monetary callback", err)
	}
	fixture.stateLock.Lock()
	defer fixture.stateLock.Unlock()
	for _, query := range fixture.queries {
		if strings.HasPrefix(query, "begin") || strings.Contains(query, "pg_try_advisory_lock") {
			t.Fatal("mismatched route used financial authority")
		}
	}
}

func TestOwnedTxMalformedDirectResourceDoesNotFallBack(t *testing.T) {
	pop := Vault.PushSimpleResource(MaintenancePgVaultResourceName, []byte("{}"))
	defer pop()
	called := false
	err := captureDbErrorPanic(func() {
		OwnedTx(t.Context(), []PgOwnershipKey{NewPgOwnershipKey("owned-tx-test", NewId())}, func(PgTx) { called = true })
	})
	if err == nil || called {
		t.Fatal("malformed explicit route fell back to ordinary business execution")
	}
}

// Panic unwinding cannot advertise a confirmed transaction release.
func TestTryTxOwnershipCommitPanicStaysUncertain(t *testing.T) {
	failure := errors.New("synthetic commit panic")
	var events []PgOwnershipEventKind
	tx := &postCommitPgTx{PgTx: txCommitObservationPanicTx{failure: failure}, ownership: &pgTransactionOwnership{
		admitted: true, observation: &PgOwnershipObservation{Observe: func(event PgOwnershipEvent) { events = append(events, event.Kind) }}}}
	got := captureDbErrorPanic(func() { _ = tx.Commit(t.Context()) })
	if got != failure || len(events) != 1 || events[0] != PgOwnershipUncertain {
		t.Fatal("commit panic became confirmed ownership release", got, events)
	}
}
