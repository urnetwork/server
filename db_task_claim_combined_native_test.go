// Real independent backends prove the two lock lifetimes and CASE short circuit.
package server

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

func combinedClaimNativeTestEnv(t *testing.T, run func(testing.TB, context.Context, PgConn, PgConn, PgConn)) {
	ownedTransactionTestEnv(t, func(t testing.TB, ctx context.Context) {
		pop := Config.PushSimpleResource(MaintenancePgConfigResourceName, []byte("min_connections: 0\nmax_connections: 3\n"))
		defer pop()
		safeMaintenancePool.reset()
		defer safeMaintenancePool.reset()
		connections := make([]PgConn, 0, 3)
		for range 3 {
			conn := RaisePgResult(AcquireMaintenanceDbConn(ctx))
			defer conn.Release()
			defer releaseOwnedTransactionTestKeys(ctx, conn)
			connections = append(connections, conn)
		}
		run(t, ctx, connections[0], connections[1], connections[2])
	})
}

// A third backend probes both key spaces and immediately releases anything
// acquired. This cannot be satisfied by a same-session reentrant try-lock.
func requireCombinedClaimNativeLocks(t testing.TB, ctx context.Context, probe PgConn, sessionKey int64, key PgOwnershipKey, sessionHeld, queueHeld bool) {
	t.Helper()
	var sessionFree, queueFree bool
	Raise(probe.QueryRow(ctx, `SELECT pg_try_advisory_lock($1::bigint)`, sessionKey).Scan(&sessionFree))
	if sessionFree {
		RaisePgResult(probe.Exec(ctx, `SELECT pg_advisory_unlock($1::bigint)`, sessionKey))
	}
	Raise(probe.QueryRow(ctx, `SELECT pg_try_advisory_lock($1::integer,$2::integer)`, key.first, key.second).Scan(&queueFree))
	if queueFree {
		RaisePgResult(probe.Exec(ctx, `SELECT pg_advisory_unlock($1::integer,$2::integer)`, key.first, key.second))
	}
	if sessionFree == sessionHeld || queueFree == queueHeld {
		t.Fatalf("independent claim ownership differs: session held=%t want=%t, queue held=%t want=%t", !sessionFree, sessionHeld, !queueFree, queueHeld)
	}
}

func TestPgTaskClaimCombinedNativeHeldSessionSkipsQueue(t *testing.T) {
	combinedClaimNativeTestEnv(t, func(t testing.TB, ctx context.Context, owner, claim, probe PgConn) {
		const sessionKey int64 = 174103
		key := NewPgOwnershipKey("claim-combined-native", NewId())
		RaisePgResult(owner.Exec(ctx, `SELECT pg_advisory_lock($1::bigint)`, sessionKey))
		tx := RaisePgResult(claim.BeginTx(ctx, pgx.TxOptions{IsoLevel: TxReadCommitted}))
		defer rollbackTaskClaimWire(ctx, tx)
		Raise(ValidatePgTaskClaimTransaction(ctx, claim, tx))
		session, queue, err := TryPgTaskClaimSessionAndQueueOwnership(ctx, tx, sessionKey, key)
		if err != nil || session || queue {
			t.Fatal("held session did not refuse the complete claim pair", session, queue, err)
		}
		requireCombinedClaimNativeLocks(t, ctx, probe, sessionKey, key, true, false)
		Raise(tx.Commit(ctx))
		RaisePgResult(owner.Exec(ctx, `SELECT pg_advisory_unlock($1::bigint)`, sessionKey))
		requireCombinedClaimNativeLocks(t, ctx, probe, sessionKey, key, false, false)
	})
}

func TestPgTaskClaimCombinedNativeHeldQueueRetainsOneSession(t *testing.T) {
	combinedClaimNativeTestEnv(t, func(t testing.TB, ctx context.Context, owner, claim, probe PgConn) {
		const sessionKey int64 = 174107
		key := NewPgOwnershipKey("claim-combined-native", NewId())
		RaisePgResult(owner.Exec(ctx, `SELECT pg_advisory_lock($1::integer,$2::integer)`, key.first, key.second))
		tx := RaisePgResult(claim.BeginTx(ctx, pgx.TxOptions{IsoLevel: TxReadCommitted}))
		defer rollbackTaskClaimWire(ctx, tx)
		Raise(ValidatePgTaskClaimTransaction(ctx, claim, tx))
		session, queue, err := TryPgTaskClaimSessionAndQueueOwnership(ctx, tx, sessionKey, key)
		if err != nil || !session || queue {
			t.Fatal("queue refusal lost retained session ownership", session, queue, err)
		}
		requireCombinedClaimNativeLocks(t, ctx, probe, sessionKey, key, true, true)
		Raise(tx.Rollback(ctx))
		requireCombinedClaimNativeLocks(t, ctx, probe, sessionKey, key, true, true)
		var unlocked bool
		Raise(claim.QueryRow(ctx, `SELECT pg_advisory_unlock($1::bigint)`, sessionKey).Scan(&unlocked))
		if !unlocked {
			t.Fatal("claim queue refusal did not retain its session guard")
		}
		// One acknowledged retirement must suffice: a duplicated volatile
		// session attempt would leave its second reentrant lock held here.
		requireCombinedClaimNativeLocks(t, ctx, probe, sessionKey, key, false, true)
	})
}

func TestPgTaskClaimCombinedNativeEndReleasesOnlyQueue(t *testing.T) {
	for _, commit := range []bool{true, false} {
		name := "rollback"
		if commit {
			name = "commit"
		}
		t.Run(name, func(t *testing.T) {
			combinedClaimNativeTestEnv(t, func(t testing.TB, ctx context.Context, _, claim, probe PgConn) {
				const sessionKey int64 = 174109
				key := NewPgOwnershipKey("claim-combined-native", NewId())
				tx := RaisePgResult(claim.BeginTx(ctx, pgx.TxOptions{IsoLevel: TxReadCommitted}))
				defer rollbackTaskClaimWire(ctx, tx)
				Raise(ValidatePgTaskClaimTransaction(ctx, claim, tx))
				session, queue, err := TryPgTaskClaimSessionAndQueueOwnership(ctx, tx, sessionKey, key)
				if err != nil || !session || !queue {
					t.Fatal("healthy claim pair was refused", session, queue, err)
				}
				requirePgOwnershipHeld(t, ctx, claim.Conn().PgConn().PID(), key)
				requireCombinedClaimNativeLocks(t, ctx, probe, sessionKey, key, true, true)
				if commit {
					Raise(tx.Commit(ctx))
				} else {
					Raise(tx.Rollback(ctx))
				}
				requireCombinedClaimNativeLocks(t, ctx, probe, sessionKey, key, true, false)
				var unlocked bool
				Raise(claim.QueryRow(ctx, `SELECT pg_advisory_unlock($1::bigint)`, sessionKey).Scan(&unlocked))
				if !unlocked {
					t.Fatal("transaction end retired the live task session")
				}
				requireCombinedClaimNativeLocks(t, ctx, probe, sessionKey, key, false, false)
			})
		})
	}
}

func TestPgTaskClaimCombinedNativeWrongIsolationTakesNoLocks(t *testing.T) {
	combinedClaimNativeTestEnv(t, func(t testing.TB, ctx context.Context, _, claim, probe PgConn) {
		const sessionKey int64 = 174121
		key := NewPgOwnershipKey("claim-combined-native", NewId())
		tx := RaisePgResult(claim.BeginTx(ctx, pgx.TxOptions{IsoLevel: TxRepeatableRead}))
		defer rollbackTaskClaimWire(ctx, tx)
		session, queue, err := TryPgTaskClaimSessionAndQueueOwnership(ctx, tx, sessionKey, key)
		if err == nil || session || queue {
			t.Fatal("wrong isolation admitted a claim owner", session, queue, err)
		}
		requireCombinedClaimNativeLocks(t, ctx, probe, sessionKey, key, false, false)
	})
}
