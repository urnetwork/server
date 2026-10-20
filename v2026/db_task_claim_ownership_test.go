// The raw claim bridge independently proves its direct route and startup PID.
// Real pgx wire replies force each refusal before task advisory or row SQL.
package server

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgxpool"
)

func taskClaimWireRows(backend int, query string) ([]pgproto3.FieldDescription, [][][]byte) {
	pid := pgproto3.FieldDescription{Name: []byte("pid"), DataTypeOID: 23, DataTypeSize: 4}
	isolation := pgproto3.FieldDescription{Name: []byte("isolation"), DataTypeOID: 25, DataTypeSize: -1}
	if strings.HasPrefix(query, "SELECT pg_backend_pid(),current_setting") {
		return []pgproto3.FieldDescription{pid, isolation}, [][][]byte{{[]byte(strconv.Itoa(backend)), []byte("read committed")}}
	}
	if strings.HasPrefix(query, "SELECT current_setting('transaction_isolation'),pg_backend_pid(),") {
		acquired := pgproto3.FieldDescription{Name: []byte("acquired"), DataTypeOID: 16, DataTypeSize: 1}
		return []pgproto3.FieldDescription{isolation, pid, acquired}, [][][]byte{{[]byte("read committed"), []byte(strconv.Itoa(backend)), []byte("t")}}
	}
	return nil, nil
}

func taskClaimWireResource(t *testing.T, host string) {
	t.Helper()
	pop := Vault.PushSimpleResource(MaintenancePgVaultResourceName,
		[]byte(`{"authority":"`+host+`:5432","db":"synthetic","user":"synthetic"}`))
	t.Cleanup(pop)
}

// A declared but different resource must fail before even the backend probe.
func TestPgTaskClaimValidationRejectsWrongResourceBeforeProbe(t *testing.T) {
	taskClaimWireResource(t, "different-pg.example")
	fixture, pool := newPgPoolWireFixture(t, nil,
		func(context.Context, pgxpool.ShouldPingParams) bool { return false })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	conn := RaisePgResult(pool.open().Acquire(ctx))
	defer conn.Release()
	tx := RaisePgResult(conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: TxReadCommitted}))
	defer rollbackTaskClaimWire(ctx, tx)
	if err := ValidatePgTaskClaimTransaction(ctx, conn, tx); err == nil {
		t.Fatal("raw task claim accepted a different declared resource")
	}
	fixture.stateLock.Lock()
	defer fixture.stateLock.Unlock()
	for _, query := range fixture.queries {
		if strings.Contains(query, "pg_backend_pid") || strings.Contains(query, "pg_try_advisory") {
			t.Fatal("wrong task claim resource reached ownership SQL")
		}
	}
}

// A transaction-pooler-style startup identity cannot authorize the retained
// session guard even when its configured resource name matches.
func TestPgTaskClaimValidationRejectsIndirectBackend(t *testing.T) {
	taskClaimWireResource(t, "synthetic-pg.example")
	fixture, pool := newPgPoolWireFixture(t, nil,
		func(context.Context, pgxpool.ShouldPingParams) bool { return false },
		func(fixture *pgPoolWireFixture, _ *pgxpool.Config) {
			fixture.queryRows = func(backend int, query string) ([]pgproto3.FieldDescription, [][][]byte) {
				return taskClaimWireRows(backend+1000, query)
			}
		})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	conn := RaisePgResult(pool.open().Acquire(ctx))
	defer conn.Release()
	tx := RaisePgResult(conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: TxReadCommitted}))
	defer rollbackTaskClaimWire(ctx, tx)
	if err := ValidatePgTaskClaimTransaction(ctx, conn, tx); err == nil {
		t.Fatal("raw task claim accepted an indirect startup backend")
	}
	fixture.stateLock.Lock()
	defer fixture.stateLock.Unlock()
	for _, query := range fixture.queries {
		if strings.Contains(query, "pg_try_advisory") {
			t.Fatal("indirect startup backend reached task ownership SQL")
		}
	}
}

// Candidate admission repeats the identity check on the same raw transaction;
// a successful earlier check is not authority for a later switched backend.
func TestPgTaskClaimProbeRejectsBackendSwitch(t *testing.T) {
	taskClaimWireResource(t, "synthetic-pg.example")
	_, pool := newPgPoolWireFixture(t, nil,
		func(context.Context, pgxpool.ShouldPingParams) bool { return false },
		func(fixture *pgPoolWireFixture, _ *pgxpool.Config) {
			fixture.queryRows = func(backend int, query string) ([]pgproto3.FieldDescription, [][][]byte) {
				if strings.HasPrefix(query, "SELECT current_setting('transaction_isolation'),pg_backend_pid(),") {
					fields, rows := taskClaimWireRows(backend+1000, query)
					rows[0][2] = []byte("f")
					return fields, rows
				}
				return taskClaimWireRows(backend, query)
			}
		})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	conn := RaisePgResult(pool.open().Acquire(ctx))
	defer conn.Release()
	tx := RaisePgResult(conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: TxReadCommitted}))
	defer rollbackTaskClaimWire(ctx, tx)
	if err := ValidatePgTaskClaimTransaction(ctx, conn, tx); err != nil {
		t.Fatal("healthy raw task claim preflight failed", err)
	}
	admitted, err := TryPgTaskClaimOwnership(ctx, tx, NewPgOwnershipKey("synthetic-task-claim", NewId()))
	if err == nil || admitted {
		t.Fatal("raw task claim accepted a switched backend", admitted, err)
	}
}

// The healthy route uses the actual raw transaction through an explicit
// successful commit. Wrapped financial transactions cannot borrow this bridge.
func TestPgTaskClaimBridgePreservesHealthyRawTransaction(t *testing.T) {
	taskClaimWireResource(t, "synthetic-pg.example")
	fixture, pool := newPgPoolWireFixture(t, nil,
		func(context.Context, pgxpool.ShouldPingParams) bool { return false },
		func(fixture *pgPoolWireFixture, _ *pgxpool.Config) { fixture.queryRows = taskClaimWireRows })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	conn := RaisePgResult(pool.open().Acquire(ctx))
	defer conn.Release()
	tx := RaisePgResult(conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: TxReadCommitted}))
	defer rollbackTaskClaimWire(ctx, tx)
	key := NewPgOwnershipKey("synthetic-task-claim", NewId())
	wrapped := &postCommitPgTx{PgTx: tx}
	if err := ValidatePgTaskClaimTransaction(ctx, conn, wrapped); err == nil {
		t.Fatal("raw claim preflight accepted a wrapped financial transaction")
	}
	if acquired, err := TryPgTaskClaimOwnership(ctx, wrapped, key); acquired || err == nil {
		t.Fatal("raw claim bridge accepted a wrapped financial transaction")
	}
	if err := ValidatePgTaskClaimTransaction(ctx, conn, tx); err != nil {
		t.Fatal("healthy raw claim was refused", err)
	}
	if acquired, err := TryPgTaskClaimOwnership(ctx, tx, key); !acquired || err != nil {
		t.Fatal("healthy raw claim key was refused", acquired, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal("healthy raw claim could not commit", err)
	}
	fixture.stateLock.Lock()
	defer fixture.stateLock.Unlock()
	probes, commits := 0, 0
	for _, query := range fixture.queries {
		if strings.Contains(query, "pg_try_advisory_xact_lock") {
			probes++
		}
		if query == "commit" {
			commits++
		}
	}
	if probes != 1 || commits != 1 {
		t.Fatal("raw claim bridge replayed or hid transaction work", probes, commits)
	}
}

func rollbackTaskClaimWire(ctx context.Context, tx PgTx) {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	_ = tx.Rollback(cleanup)
}
