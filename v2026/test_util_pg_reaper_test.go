// Database cleanup must distinguish abandoned fixtures from old live owners.
package server

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// Keep the fixture young to real sweepers; a synthetic future cutoff makes it
// eligible only to the test's own reaper. Cleanup names only this database.
func newTestPgDbReaperFixture(ctx context.Context) (string, int64, func()) {
	created := NowUtc()
	datname := fmt.Sprintf("test_%d_%x", created.UnixMilli(), NewId().Bytes())
	Db(ctx, func(conn PgConn) {
		_, err := conn.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %s`, datname))
		Raise(err)
	}, OptReadWrite())
	return datname, created.Add(testPgDbOrphanAge).UnixMilli(), func() {
		Db(ctx, func(conn PgConn) {
			_, err := conn.Exec(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %s`, datname))
			Raise(err)
		}, OptReadWrite())
	}
}

// Force the discovery/use race: the candidate is already known when its live
// owner connects. The old forced drop destroyed that owner and its database.
func TestTestPgDbReaperPreservesLiveOldDatabase(t *testing.T) {
	(&TestEnv{}).Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		datname, cutoffMillis, cleanup := newTestPgDbReaperFixture(ctx)
		defer cleanup()
		owner := acquireTestPgDbLease(ctx, datname)
		defer closePgConnection(ctx, owner)
		if dropOrphanedTestPgDb(ctx, datname, cutoffMillis) {
			t.Fatal("reaper dropped an old database with a live owner")
		}
		if err := owner.Ping(ctx); err != nil {
			t.Fatalf("reaper disconnected the live fixture owner: %v", err)
		}
	})
}

// The fixture lease is independent of pools: resetting every application pool
// does not make its database look abandoned, and releasing it permits cleanup.
func TestTestPgDbReaperReapsReleasedDatabase(t *testing.T) {
	(&TestEnv{}).Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		datname, cutoffMillis, cleanup := newTestPgDbReaperFixture(ctx)
		defer cleanup()
		owner := acquireTestPgDbLease(ctx, datname)
		defer closePgConnection(ctx, owner)
		PgReset()
		if err := owner.Ping(ctx); err != nil {
			t.Fatalf("pool reset closed the fixture owner: %v", err)
		}
		closePgConnection(ctx, owner)
		if !dropOrphanedTestPgDb(ctx, datname, cutoffMillis) {
			t.Fatal("released orphan database was not reaped")
		}
		Db(ctx, func(conn PgConn) {
			var exists bool
			Raise(conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, datname).Scan(&exists))
			if exists {
				t.Fatal("reaper reported success without removing its database")
			}
		})
	})
}

// No fixture is eligible to an ordinary age sweep during the create-to-lease
// window; only its synthetic future cutoff admits it to the test operation.
func TestTestPgDbReaperFixtureIsYoungToRealSweepers(t *testing.T) {
	(&TestEnv{}).Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		datname, cutoffMillis, cleanup := newTestPgDbReaperFixture(ctx)
		defer cleanup()
		createdMillis, ok := parseTestPgDbName(datname)
		if !ok || createdMillis < NowUtc().Add(-testPgDbOrphanAge).UnixMilli() || cutoffMillis <= createdMillis {
			t.Fatalf("fixture is exposed to real sweepers or missing its synthetic cutoff: created=%d cutoff=%d", createdMillis, cutoffMillis)
		}
	})
}

// Force a sweep to win after all fixture connections close but before its own
// teardown drop. Both owners must complete without a spurious missing-db error.
func TestTestPgDbTeardownToleratesReaperAfterLeaseRelease(t *testing.T) {
	var reaperWon atomic.Bool
	testEnv := &TestEnv{
		beforePgDbDropForTest: func(ctx context.Context, datname string) {
			ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			if dropOrphanedTestPgDb(ctx, datname, NowUtc().Add(testPgDbOrphanAge).UnixMilli()) {
				reaperWon.Store(true)
			} else {
				t.Error("forced sweep did not acquire the released fixture database")
			}
		},
	}
	testEnv.Run(t, func(testing.TB) {})
	if !reaperWon.Load() {
		t.Error("test never exercised the lease-release/drop race")
	}
}

// The real setup path, not just the lease helper, must retain ownership through
// a pool reset. Teardown closes the lease before dropping its own database.
func TestTestPgDbLeaseCoversFixtureLifetime(t *testing.T) {
	(&TestEnv{}).Run(t, func(t testing.TB) {
		PgReset()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		Db(ctx, func(conn PgConn) {
			var owners int
			Raise(conn.QueryRow(ctx, `
				SELECT count(*) FROM pg_stat_activity
				WHERE datname = current_database()
				AND application_name = 'urnetwork-test-database-owner'
			`).Scan(&owners))
			if owners != 1 {
				t.Fatalf("live fixture has %d independent database owners, want 1", owners)
			}
		})
	})
}

// Invalid identifiers and candidates at/after the cutoff never reach SQL.
func TestTestPgDbReaperRejectsUnsafeOrYoungCandidates(t *testing.T) {
	for _, datname := range []string{
		"app_database",
		"test_1_0011",
		"test_-1_00112233445566778899aabbccddeeff",
		"test_+1_00112233445566778899aabbccddeeff",
		"test_01_00112233445566778899aabbccddeeff",
		"test_100_00112233445566778899aabbccddeeff",
		"test_101_00112233445566778899aabbccddeeff",
	} {
		if dropOrphanedTestPgDb(context.Background(), datname, 100) {
			t.Errorf("reaped unsafe or young candidate %q", datname)
		}
	}
}
