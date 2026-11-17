// An otherwise protocol-idle release may still expire into pgxpool-owned
// destruction. The private wire peer and virtual clock prove that distinction.
package server

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ReleaseIdle is not a promise of physical reinsertion or joined cleanup.
// Acknowledged rollback releases locks while the same pool retains capacity
// throughout lifetime-triggered destruction of the borrowed connection.
func TestDbReleaseIdleObservationRetainsLifetimeDestructorCustody(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		closeEntered, releaseClose := make(chan struct{}), make(chan struct{})
		var closeOnce sync.Once
		unblockClose := func() { closeOnce.Do(func() { close(releaseClose) }) }
		var dials atomic.Int32
		_, pool := newPgPoolWireFixture(t, nil, neverPingTestPool, func(_ *pgPoolWireFixture, config *pgxpool.Config) {
			config.MaxConnLifetime = time.Second
			config.MaxConnLifetimeJitter = 0
			config.HealthCheckPeriod = time.Hour
			dial := config.ConnConfig.DialFunc
			config.ConnConfig.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
				conn, err := dial(ctx, network, address)
				if err == nil && dials.Add(1) == 1 {
					return &dbReleaseHeldCloseConn{Conn: conn, entered: closeEntered, release: releaseClose}, nil
				}
				return conn, err
			}
		})
		t.Cleanup(unblockClose)
		var physical *pgconn.PgConn
		observation := NewDbTxObservation()
		cause := errors.New("synthetic rolled-back read-only callback")
		failure := captureDbErrorPanic(func() {
			txWithPool(t.Context(), pool, func(tx PgTx) {
				physical = tx.Conn().PgConn()
				// Only the fake clock advances while this actual borrow is held.
				synctest.Sleep(2 * time.Second)
				panic(cause)
			}, OptNoRetry(), observation)
		})
		synctest.Wait()
		select {
		case <-closeEntered:
		default:
			t.Fatal("expired idle release did not enter actual physical destruction")
		}
		snapshot := observation.Snapshot()
		if failure != cause || snapshot.AcquireSucceeded != 1 || snapshot.RollbackAcknowledged != 1 || snapshot.CommitAttempts != 0 ||
			snapshot.ReleaseAttempts != 1 || snapshot.ReleaseReturned != 1 || snapshot.LastRelease.Disposition != DbTxReleaseIdle || snapshot.LastRelease.CleanupComplete {
			t.Fatal("idle eligibility was confused with actual reuse or joined destruction", failure, snapshot)
		}
		select {
		case <-physical.CleanupDone():
			t.Fatal("physical cleanup crossed the held actual transport Close")
		default:
		}
		if stat := pool.open().Stat(); stat.MaxLifetimeDestroyCount() != 1 || stat.TotalConns() != 1 || stat.AcquiredConns() != 1 || dials.Load() != 1 {
			t.Fatal("lifetime destruction lost the exact pool's retained capacity", stat.MaxLifetimeDestroyCount(), stat.TotalConns(), stat.AcquiredConns(), dials.Load())
		}
		unblockClose()
		<-physical.CleanupDone()
		synctest.Wait()
		conn, err := pool.open().Acquire(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Release()
		if _, err := conn.Exec(t.Context(), "SELECT 2"); err != nil || dials.Load() != 2 || snapshot.LastRelease.CleanupComplete {
			t.Fatal("fresh capacity did not recover or old snapshot acquired a false cleanup join", err, dials.Load())
		}
	})
}
