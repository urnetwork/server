// The claim pair uses one actual pgx exchange, including refusal and failure.
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

// The prior two-statement path gets the same typed replies, so its causal
// control reaches the exchange-count assertion instead of a decoding failure.
func combinedClaimWireRows(backend int, query string, isolation string, session, queue bool) ([]pgproto3.FieldDescription, [][][]byte) {
	boolean := func(value bool) []byte {
		if value {
			return []byte("t")
		}
		return []byte("f")
	}
	flag := pgproto3.FieldDescription{Name: []byte("acquired"), DataTypeOID: 16, DataTypeSize: 1}
	if strings.HasPrefix(query, "WITH task_claim_session AS MATERIALIZED") {
		return []pgproto3.FieldDescription{
			{Name: []byte("isolation"), DataTypeOID: 25, DataTypeSize: -1},
			{Name: []byte("pid"), DataTypeOID: 23, DataTypeSize: 4}, flag, flag,
		}, [][][]byte{{[]byte(isolation), []byte(strconv.Itoa(backend)), boolean(session), boolean(queue)}}
	}
	if strings.HasPrefix(query, "SELECT pg_try_advisory_lock(") {
		return []pgproto3.FieldDescription{flag}, [][][]byte{{boolean(session)}}
	}
	fields, rows := taskClaimWireRows(backend, query)
	if strings.HasPrefix(query, "SELECT current_setting('transaction_isolation'),pg_backend_pid(),") {
		rows[0][0], rows[0][2] = []byte(isolation), boolean(queue)
	}
	return fields, rows
}

func requireCombinedClaimExchangeCount(t *testing.T, fixture *pgPoolWireFixture, want int) {
	t.Helper()
	fixture.stateLock.Lock()
	defer fixture.stateLock.Unlock()
	count := 0
	for _, query := range fixture.queries {
		if strings.Contains(query, "pg_try_advisory") {
			count++
		}
	}
	if count != want {
		t.Fatalf("task claim admission used %d ownership exchanges, want %d", count, want)
	}
}

func TestPgTaskClaimCombinedProbeUsesOneExchange(t *testing.T) {
	for _, tc := range []struct {
		name           string
		session, queue bool
	}{{"admitted", true, true}, {"queue_busy", true, false}, {"session_busy", false, false}} {
		t.Run(tc.name, func(t *testing.T) {
			taskClaimWireResource(t, "synthetic-pg.example")
			fixture, pool := newPgPoolWireFixture(t, nil,
				func(context.Context, pgxpool.ShouldPingParams) bool { return false },
				func(fixture *pgPoolWireFixture, _ *pgxpool.Config) {
					fixture.queryRows = func(backend int, query string) ([]pgproto3.FieldDescription, [][][]byte) {
						return combinedClaimWireRows(backend, query, "read committed", tc.session, tc.queue)
					}
				})
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			conn := RaisePgResult(pool.open().Acquire(ctx))
			defer conn.Release()
			tx := RaisePgResult(conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: TxReadCommitted}))
			defer rollbackTaskClaimWire(ctx, tx)
			Raise(ValidatePgTaskClaimTransaction(ctx, conn, tx))
			session, queue, err := TryPgTaskClaimSessionAndQueueOwnership(ctx, tx, 17, NewPgOwnershipKey("claim-wire", NewId()))
			if err != nil || session != tc.session || queue != tc.queue {
				t.Fatal("claim pair changed admission outcome", session, queue, err)
			}
			requireCombinedClaimExchangeCount(t, fixture, 1)
			Raise(tx.Commit(ctx))
		})
	}
}

func TestPgTaskClaimCombinedProbeRejectsUnsafeWireIdentity(t *testing.T) {
	for _, tc := range []struct {
		name      string
		pidDelta  int
		isolation string
	}{{"backend_switch", 1000, "read committed"}, {"wrong_isolation", 0, "repeatable read"}} {
		t.Run(tc.name, func(t *testing.T) {
			taskClaimWireResource(t, "synthetic-pg.example")
			fixture, pool := newPgPoolWireFixture(t, nil,
				func(context.Context, pgxpool.ShouldPingParams) bool { return false },
				func(fixture *pgPoolWireFixture, _ *pgxpool.Config) {
					fixture.queryRows = func(backend int, query string) ([]pgproto3.FieldDescription, [][][]byte) {
						if strings.Contains(query, "pg_try_advisory") {
							return combinedClaimWireRows(backend+tc.pidDelta, query, tc.isolation, false, false)
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
			Raise(ValidatePgTaskClaimTransaction(ctx, conn, tx))
			if session, queue, err := TryPgTaskClaimSessionAndQueueOwnership(ctx, tx, 19, NewPgOwnershipKey("claim-wire", NewId())); err == nil || session || queue {
				t.Fatal("unsafe claim identity was accepted", session, queue, err)
			}
			requireCombinedClaimExchangeCount(t, fixture, 1)
		})
	}
}

func TestPgTaskClaimCombinedProbeLostReplyDoesNotReplay(t *testing.T) {
	taskClaimWireResource(t, "synthetic-pg.example")
	fixture, pool := newPgPoolWireFixture(t,
		func(_ int, query string) bool { return !strings.Contains(query, "pg_try_advisory") },
		func(context.Context, pgxpool.ShouldPingParams) bool { return false },
		func(fixture *pgPoolWireFixture, _ *pgxpool.Config) { fixture.queryRows = taskClaimWireRows })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	conn := RaisePgResult(pool.open().Acquire(ctx))
	defer conn.Release()
	tx := RaisePgResult(conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: TxReadCommitted}))
	defer rollbackTaskClaimWire(ctx, tx)
	Raise(ValidatePgTaskClaimTransaction(ctx, conn, tx))
	if session, queue, err := TryPgTaskClaimSessionAndQueueOwnership(ctx, tx, 23, NewPgOwnershipKey("claim-wire", NewId())); err == nil || session || queue {
		t.Fatal("lost claim reply was acknowledged", session, queue, err)
	}
	requireCombinedClaimExchangeCount(t, fixture, 1)
}

func TestPgTaskClaimCombinedProbeRejectsWrappedTransaction(t *testing.T) {
	fixture, pool := newPgPoolWireFixture(t, nil,
		func(context.Context, pgxpool.ShouldPingParams) bool { return false })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	conn := RaisePgResult(pool.open().Acquire(ctx))
	defer conn.Release()
	tx := RaisePgResult(conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: TxReadCommitted}))
	defer rollbackTaskClaimWire(ctx, tx)
	if session, queue, err := TryPgTaskClaimSessionAndQueueOwnership(ctx, &postCommitPgTx{PgTx: tx}, 29, NewPgOwnershipKey("claim-wire", NewId())); err == nil || session || queue {
		t.Fatal("wrapped financial transaction borrowed raw claim authority", session, queue, err)
	}
	requireCombinedClaimExchangeCount(t, fixture, 0)
}
