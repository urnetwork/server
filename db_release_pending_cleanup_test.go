// A private in-memory transport holds actual pgx physical Close after the
// ordinary transaction/release returns. No database or extra cleanup owner runs.
package server

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The underlying transport closes first; only its return acknowledgement is
// held. CleanupDone therefore cannot close until the explicit release.
type dbReleaseHeldCloseConn struct {
	net.Conn
	once    sync.Once
	entered chan struct{}
	release <-chan struct{}
}

func (self *dbReleaseHeldCloseConn) Close() error {
	err := self.Conn.Close()
	self.once.Do(func() { close(self.entered); <-self.release })
	return err
}

// Puddle checks Done once before TryAcquire; the semaphore reads it a second
// time only after that exact capacity acquisition failed. This is a pinned
// private waiting barrier, not a queue-length or scheduler-timing guess.
type dbReleaseAcquireContext struct {
	context.Context
	entered chan struct{}
	calls   atomic.Int32
}

func (self *dbReleaseAcquireContext) Done() <-chan struct{} {
	if self.calls.Add(1) == 2 {
		close(self.entered)
	}
	return self.Context.Done()
}

// A returned destroy receipt may overlap pending physical cleanup, while the
// exact original pool continues holding capacity and refuses replacement.
func TestDbReleaseObservationTransfersPendingCleanupWithoutReleasingCapacity(t *testing.T) {
	watchdog, stopWatchdog := context.WithTimeout(t.Context(), 5*time.Second)
	defer stopWatchdog()
	queryEntered, releaseQuery := make(chan struct{}), make(chan struct{})
	closeEntered, releaseClose := make(chan struct{}), make(chan struct{})
	var queryOnce, closeOnce sync.Once
	unblockQuery := func() { queryOnce.Do(func() { close(releaseQuery) }) }
	unblockClose := func() { closeOnce.Do(func() { close(releaseClose) }) }
	var dials atomic.Int32
	_, pool := newPgPoolWireFixture(t, func(index int, sql string) bool {
		if index == 1 && sql == "SELECT 1" {
			close(queryEntered)
			<-releaseQuery
			return false
		}
		return true
	}, neverPingTestPool, func(_ *pgPoolWireFixture, config *pgxpool.Config) {
		dial := config.ConnConfig.DialFunc
		config.ConnConfig.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
			if network == "pipe" {
				return nil, net.ErrClosed
			}
			conn, err := dial(ctx, network, address)
			if err == nil && dials.Add(1) == 1 {
				return &dbReleaseHeldCloseConn{Conn: conn, entered: closeEntered, release: releaseClose}, nil
			}
			return conn, err
		}
	})
	caller, cancelCaller := context.WithCancel(watchdog)
	secondParent, cancelSecond := context.WithCancel(watchdog)
	second := &dbReleaseAcquireContext{Context: secondParent, entered: make(chan struct{})}
	firstDone, secondDone := make(chan struct{}), make(chan struct{})
	secondStarted := false
	t.Cleanup(func() {
		cancelCaller()
		cancelSecond()
		unblockQuery()
		unblockClose()
		select {
		case <-firstDone:
		case <-watchdog.Done():
			t.Error("first release owner did not join")
		}
		if secondStarted {
			select {
			case <-secondDone:
			case <-watchdog.Done():
				t.Error("second capacity waiter did not join")
			}
		}
	})
	physical := make(chan *pgconn.PgConn, 1)
	observation := NewDbTxObservation()
	var failure any
	go func() {
		defer close(firstDone)
		failure = captureDbErrorPanic(func() {
			txWithPool(caller, pool, func(tx PgTx) {
				physical <- tx.Conn().PgConn()
				RaisePgResult(tx.Exec(caller, "SELECT 1"))
			}, OptNoRetry(), txStatementStopTimeout(time.Millisecond), observation)
		})
	}()
	await := func(channel <-chan struct{}, label string) {
		t.Helper()
		select {
		case <-channel:
		case <-watchdog.Done():
			t.Fatal("private release fixture did not reach " + label)
		}
	}
	await(queryEntered, "actual statement entry")
	connection := <-physical
	cancelCaller()
	await(firstDone, "returned transaction/release")
	snapshot := observation.Snapshot()
	if failure == nil || snapshot.CommitAttempts != 0 || snapshot.RollbackAcknowledged != 0 || snapshot.ReleaseAttempts != 1 || snapshot.ReleaseReturned != 1 ||
		snapshot.LastRelease.Disposition != DbTxReleaseDestroyTransferred || snapshot.LastRelease.CleanupComplete {
		t.Fatal("pending destruction did not produce its exact returned transfer", failure, snapshot)
	}
	unblockQuery()
	await(closeEntered, "held actual transport close")
	select {
	case <-connection.CleanupDone():
		t.Fatal("physical cleanup completed through the held Close")
	default:
	}
	if stat := pool.open().Stat(); stat.TotalConns() != 1 || stat.AcquiredConns() != 1 || stat.MaxConns() != 1 {
		t.Fatal("destroy transfer lost the exact pool's capacity ownership", stat.TotalConns(), stat.AcquiredConns(), stat.MaxConns())
	}
	var secondError error
	secondStarted = true
	go func() {
		defer close(secondDone)
		conn, err := pool.open().Acquire(second)
		secondError = err
		if conn != nil {
			conn.Release()
		}
	}()
	await(second.entered, "actual pool semaphore wait")
	cancelSecond()
	await(secondDone, "canceled capacity waiter")
	if !errors.Is(secondError, context.Canceled) || dials.Load() != 1 {
		t.Fatal("pending destructor admitted a replacement or lost cancellation", secondError, dials.Load())
	}
	unblockClose()
	await(connection.CleanupDone(), "actual physical completion")
	conn, err := pool.open().Acquire(watchdog)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(watchdog, "SELECT 2"); err != nil || dials.Load() != 2 {
		t.Fatal("capacity did not recover after owned cleanup completed", err, dials.Load())
	}
}
