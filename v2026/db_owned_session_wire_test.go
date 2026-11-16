// The caller keeps custody across actual lost pgx replies; no uncertain scope
// may release an inherited reference or invent a committed publication.
package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgxpool"
)

func ownedSessionWireRows(backend int, query string) ([]pgproto3.FieldDescription, [][][]byte) {
	if strings.HasPrefix(query, "SELECT pg_backend_pid(),owner.first,owner.second,") {
		fields, rows := ownedTransactionWireRows(backend, "SELECT pg_backend_pid()")
		fields = append(fields,
			pgproto3.FieldDescription{Name: []byte("first"), DataTypeOID: 23, DataTypeSize: 4},
			pgproto3.FieldDescription{Name: []byte("second"), DataTypeOID: 23, DataTypeSize: 4},
			pgproto3.FieldDescription{Name: []byte("success"), DataTypeOID: 16, DataTypeSize: 1})
		rows[0] = append(rows[0], []byte("17"), []byte("29"), []byte("t"))
		return fields, rows
	}
	return ownedTransactionWireRows(backend, query)
}

func TestOwnedSessionUnknownCommitKeepsCallerCustodyWithoutReplay(t *testing.T) {
	fixture, pool := newPgPoolWireFixture(t, func(_ int, query string) bool { return query != "commit" },
		func(context.Context, pgxpool.ShouldPingParams) bool { return false },
		func(fixture *pgPoolWireFixture, _ *pgxpool.Config) { fixture.queryRows = ownedSessionWireRows })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	conn := RaisePgResult(pool.open().Acquire(ctx))
	defer conn.Release()
	session := NewPgOwnedSession(conn)
	calls, posts := 0, 0
	var counter TxCommitCounter
	err := captureDbErrorPanic(func() {
		session.txWithResource(ctx, []PgOwnershipKey{{first: 17, second: 29}}, ownedTransactionWireResource(), func(tx PgTx) {
			calls++
			RaisePgResult(tx.Exec(ctx, `INSERT INTO synthetic_effect VALUES(1)`))
			AddTxCommitCount(tx, &counter, 1)
			AddTxPostCommit(tx, "unknown-session-commit", func() any { posts++; return nil })
		}, TxReadCommitted, OptRetryDefault())
	})
	if err == nil || calls != 1 || posts != 0 || session.Err() == nil || pool.open().Stat().AcquiredConns() != 1 {
		t.Fatal("unknown borrowed commit replayed, published or released caller custody", err, calls, posts, session.Err())
	}
	requireTxCommitSnapshot(t, &counter, 0, 1, 0)
	fixture.stateLock.Lock()
	defer fixture.stateLock.Unlock()
	commits := 0
	for _, query := range fixture.queries {
		if query == "commit" {
			commits++
		}
		if strings.Contains(query, "pg_advisory_unlock") {
			t.Fatal("unknown transaction tried to guess its reference cleanup")
		}
	}
	if commits != 1 {
		t.Fatal("unknown borrowed commit attempted another transaction", commits)
	}
}

func TestOwnedSessionLostUnlockPreservesAcknowledgedPostAndQuarantines(t *testing.T) {
	fixture, pool := newPgPoolWireFixture(t, func(_ int, query string) bool {
		return !strings.Contains(query, "pg_advisory_unlock(owner.first,owner.second)")
	}, func(context.Context, pgxpool.ShouldPingParams) bool { return false },
		func(fixture *pgPoolWireFixture, _ *pgxpool.Config) { fixture.queryRows = ownedSessionWireRows })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	conn := RaisePgResult(pool.open().Acquire(ctx))
	defer conn.Release()
	session := NewPgOwnedSession(conn)
	calls, posts := 0, 0
	var counter TxCommitCounter
	session.txWithResource(ctx, []PgOwnershipKey{{first: 17, second: 29}}, ownedTransactionWireResource(), func(tx PgTx) {
		calls++
		RaisePgResult(tx.Exec(ctx, `INSERT INTO synthetic_effect VALUES(1)`))
		AddTxCommitCount(tx, &counter, 1)
		AddTxPostCommit(tx, "acknowledged-session-commit", func() any {
			posts++
			if session.Err() == nil || pool.open().Stat().AcquiredConns() != 1 {
				t.Error("committed post lost its caller's quarantined custody")
			}
			return nil
		})
	}, TxReadCommitted)
	if calls != 1 || posts != 1 || session.Err() == nil {
		t.Fatal("uncertain cleanup changed an acknowledged commit", calls, posts, session.Err())
	}
	requireTxCommitSnapshot(t, &counter, 1, 0, 0)
	err := captureDbErrorPanic(func() {
		session.txWithResource(ctx, []PgOwnershipKey{{first: 17, second: 29}}, ownedTransactionWireResource(), func(PgTx) { calls++ })
	})
	if err != session.Err() || calls != 1 {
		t.Fatal("quarantined session admitted another callback", err)
	}
	fixture.stateLock.Lock()
	defer fixture.stateLock.Unlock()
	unlocks, begins := 0, 0
	for _, query := range fixture.queries {
		if strings.Contains(query, "pg_advisory_unlock(owner.first,owner.second)") {
			unlocks++
		}
		if strings.HasPrefix(query, "begin") {
			begins++
		}
		if strings.Contains(query, "pg_advisory_unlock_all") {
			t.Fatal("borrowed scope released every inherited lock")
		}
	}
	if begins != 1 || unlocks != 1 {
		t.Fatal("unknown unlock was replayed or followed by new work", begins, unlocks)
	}
}

func TestOwnedSessionUnknownAcquireDoesNotGuessAnUnlock(t *testing.T) {
	fixture, pool := newPgPoolWireFixture(t, func(_ int, query string) bool {
		return !strings.Contains(query, "pg_try_advisory_lock(owner.first,owner.second)")
	}, func(context.Context, pgxpool.ShouldPingParams) bool { return false },
		func(fixture *pgPoolWireFixture, _ *pgxpool.Config) { fixture.queryRows = ownedSessionWireRows })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	conn := RaisePgResult(pool.open().Acquire(ctx))
	defer conn.Release()
	session := NewPgOwnedSession(conn)
	calls := 0
	err := captureDbErrorPanic(func() {
		session.txWithResource(ctx, []PgOwnershipKey{{first: 17, second: 29}}, ownedTransactionWireResource(), func(PgTx) { calls++ })
	})
	if err == nil || calls != 0 || session.Err() == nil || pool.open().Stat().AcquiredConns() != 1 {
		t.Fatal("unknown admission entered business or surrendered caller custody", err, calls)
	}
	fixture.stateLock.Lock()
	defer fixture.stateLock.Unlock()
	for _, query := range fixture.queries {
		if strings.HasPrefix(query, "begin") || strings.Contains(query, "pg_advisory_unlock") {
			t.Fatal("unknown reference count caused business work or a guessed decrement")
		}
	}
}
