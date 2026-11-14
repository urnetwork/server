// Nonwaiting admission shares direct-session custody and never classifies an
// unknown database outcome as a harmless busy key.
package server

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A busy key in the second SQL chunk releases the first chunk and checkout.
// Two maintenance slots suffice for the held owner and independent progress.
func TestTryOwnedTxBusyReleasesPartialKeysAndCheckout(t *testing.T) {
	ownedTransactionTestEnv(t, func(t testing.TB, ctx context.Context) {
		pop := Config.PushSimpleResource(MaintenancePgConfigResourceName, []byte("min_connections: 0\nmax_connections: 2\n"))
		defer pop()
		safeMaintenancePool.reset()
		defer safeMaintenancePool.reset()
		keys := make([]PgOwnershipKey, pgOwnershipQueryLimit+3)
		for index := range keys {
			keys[index] = NewPgOwnershipKey("synthetic-try-owned", NewId())
		}
		keys = normalizePgOwnershipKeys(keys)
		held := keys[len(keys)-1]
		ready, release := make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		resume := func() { releaseOnce.Do(func() { close(release) }) }
		holder := startOwnedTransactionTest(func() {
			OwnedTx(ctx, []PgOwnershipKey{held}, func(PgTx) {
				close(ready)
				select {
				case <-release:
				case <-ctx.Done():
					Raise(ctx.Err())
				}
			}, TxReadCommitted, OptNoRetry())
		})
		defer func() {
			resume()
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			holder.join(t, cleanup)
		}()
		ownedTransactionTestAwait(t, ctx, ready)
		var events []PgOwnershipEvent
		var refusedAcquired int32
		observed := Testing_WithPgOwnershipObservation(ctx, func(event PgOwnershipEvent) {
			events = append(events, event)
			if event.Kind == PgOwnershipRefused {
				refusedAcquired = safeMaintenancePool.open().Stat().AcquiredConns()
			}
		})
		calls := 0
		if TryOwnedTx(observed, append(slices.Clone(keys), keys[0]), func(PgTx) { calls++ }, TxReadCommitted, OptNoRetry()) ||
			calls != 0 || len(events) != 1 || events[0].Kind != PgOwnershipRefused || events[0].TransactionScoped || refusedAcquired != 1 {
			t.Fatal("known busy admission waited, entered business or retained its checkout", calls, refusedAcquired, events)
		}
		MaintenanceDb(ctx, func(conn PgConn) {
			var remaining int
			Raise(conn.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE pid=$1 AND locktype='advisory'`, events[0].BackendPid).Scan(&remaining))
			if remaining != 0 {
				t.Fatal("later key refusal leaked earlier partial ownership", remaining)
			}
		}, OptNoRetry())
		if !TryOwnedTx(ctx, keys[:len(keys)-1], func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, `INSERT INTO owned_tx_effect VALUES(1,23)`))
		}, TxReadCommitted, OptNoRetry()) {
			t.Fatal("held late key blocked independently released earlier keys")
		}
		resume()
		if err := holder.join(t, ctx); err != nil {
			t.Fatal("held owner did not finish", err)
		}
		events = nil
		var releasedAcquired int32
		observed = Testing_WithPgOwnershipObservation(ctx, func(event PgOwnershipEvent) {
			events = append(events, event)
			if event.Kind == PgOwnershipReleased {
				releasedAcquired = safeMaintenancePool.open().Stat().AcquiredConns()
			}
		})
		if !TryOwnedTx(observed, keys, func(tx PgTx) {
			calls++
			if !TxOwnsKeys(tx, keys) {
				panic(errors.New("admitted direct transaction lost its complete key set"))
			}
			RaisePgResult(tx.Exec(ctx, `INSERT INTO owned_tx_effect VALUES(2,37)`))
		}, TxReadCommitted, OptNoRetry()) || calls != 1 || len(events) != 2 ||
			events[0].Kind != PgOwnershipAdmitted || events[1].Kind != PgOwnershipReleased || releasedAcquired != 0 {
			t.Fatal("released owner did not execute once or observed completion before checkout return", calls, releasedAcquired, events)
		}
		MaintenanceDb(ctx, func(conn PgConn) {
			var exact bool
			Raise(conn.QueryRow(ctx, `SELECT count(*)=2 AND sum(amount)=60 FROM owned_tx_effect`).Scan(&exact))
			if !exact {
				t.Fatal("busy admission or independent progress changed exact committed effects")
			}
		}, OptNoRetry())
	})
}

// Lost probe, refusal-cleanup and commit replies are actual pgx wire failures.
// None may become a false return that lets a scanner forget ambiguous work.
func TestTryOwnedTxUnknownOutcomesRemainErrors(t *testing.T) {
	for _, phase := range []string{"probe", "refusal_cleanup", "commit"} {
		fixture, pool := newPgPoolWireFixture(t, func(_ int, query string) bool {
			switch phase {
			case "probe":
				return !strings.HasPrefix(query, "SELECT pg_backend_pid(),")
			case "refusal_cleanup":
				return query != "SELECT pg_advisory_unlock_all()"
			default:
				return query != "commit"
			}
		}, func(context.Context, pgxpool.ShouldPingParams) bool { return false },
			func(fixture *pgPoolWireFixture, _ *pgxpool.Config) {
				fixture.queryRows = func(backend int, query string) ([]pgproto3.FieldDescription, [][][]byte) {
					fields, rows := ownedTransactionWireRows(backend, query)
					if phase == "refusal_cleanup" && strings.HasPrefix(query, "SELECT pg_backend_pid(),") {
						rows[0][1] = []byte("f")
					}
					return fields, rows
				}
			})
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		calls, posts := 0, 0
		returned := false
		got := captureDbErrorPanic(func() {
			ownedTxWithResourcePolicy(ctx, []PgOwnershipKey{NewPgOwnershipKey("synthetic-try-owned-wire", NewId())},
				ownedTransactionWireResource(), pool.open().Acquire, false, func(tx PgTx) {
					calls++
					RaisePgResult(tx.Exec(ctx, `INSERT INTO synthetic_effect VALUES(1)`))
					AddTxPostCommit(tx, "must-not-publish", func() any { posts++; return nil })
				}, OptRetryDefault())
			returned = true
		})
		cancel()
		wantedCalls := 0
		if phase == "commit" {
			wantedCalls = 1
		}
		if got == nil || returned || calls != wantedCalls || posts != 0 {
			t.Fatal("unknown direct-owner outcome became busy/success or replayed", phase, got, returned, calls, posts)
		}
		fixture.stateLock.Lock()
		commits := 0
		for _, query := range fixture.queries {
			if query == "commit" {
				commits++
			}
		}
		fixture.stateLock.Unlock()
		if commits != wantedCalls {
			t.Fatal("unknown reply changed the number of business transactions", phase, commits)
		}
	}
}

// A canceled acquire remains the original error and executes no business work.
func TestTryOwnedTxAcquisitionErrorDoesNotBecomeBusy(t *testing.T) {
	calls := 0
	got := captureDbErrorPanic(func() {
		ownedTxWithResourcePolicy(t.Context(), []PgOwnershipKey{NewPgOwnershipKey("synthetic-try-owned-acquire", NewId())},
			ownedTransactionWireResource(), func(context.Context) (PgConn, error) { return nil, context.Canceled }, false,
			func(PgTx) { calls++ })
	})
	if got != context.Canceled || calls != 0 {
		t.Fatal("failed acquire became a known busy result", got, calls)
	}
}
