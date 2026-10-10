package server

import (
	"context"
	"errors"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Counts physical transport disposal independently of pgxpool resource stats.
// The underlying transport is the existing in-memory pgPoolWireFixture.
type startupCleanupCountedConn struct {
	net.Conn
	once sync.Once
	live *atomic.Int32
}

type recordingStartupCleanup struct {
	done         chan struct{}
	closeContext chan context.Context
	closeCalls   atomic.Int32
	blockClose   bool
}

func (self *recordingStartupCleanup) Close(ctx context.Context) error {
	self.closeCalls.Add(1)
	self.closeContext <- ctx
	if self.blockClose {
		<-ctx.Done()
	}
	return errors.New("secondary cleanup failure")
}

func (self *recordingStartupCleanup) CleanupDone() chan struct{} { return self.done }

func TestPgStartupCleanupDetachesAndJoins(t *testing.T) {
	type key struct{}
	caller, cancelCaller := context.WithCancel(context.WithValue(context.Background(), key{}, "retained"))
	cancelCaller()
	conn := &recordingStartupCleanup{done: make(chan struct{}), closeContext: make(chan context.Context, 1)}
	returned := make(chan struct{})
	go func() {
		cleanupFailedPgStartup(caller, conn, time.Second)
		close(returned)
	}()
	var cleanupCtx context.Context
	select {
	case cleanupCtx = <-conn.closeContext:
	case <-time.After(time.Second):
		t.Fatal("cleanup did not call Close")
	}
	if cleanupCtx.Err() != nil || cleanupCtx.Value(key{}) != "retained" {
		t.Fatal("cleanup inherited caller cancellation or lost context values")
	}
	deadline, ok := cleanupCtx.Deadline()
	if remaining := time.Until(deadline); !ok || remaining <= 0 || remaining > time.Second {
		t.Fatalf("cleanup deadline=%v exists=%t", remaining, ok)
	}
	select {
	case <-returned:
		t.Fatal("Close return was mistaken for completed cleanup")
	default:
	}
	close(conn.done)
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("completed cleanup did not release its constructor")
	}
	if conn.closeCalls.Load() != 1 || !errors.Is(cleanupCtx.Err(), context.Canceled) {
		t.Fatal("cleanup did not close once and release its deadline timer")
	}
}

func TestPgStartupCleanupHasOneFiniteBudget(t *testing.T) {
	for _, blockClose := range []bool{false, true} {
		conn := &recordingStartupCleanup{
			done: make(chan struct{}), closeContext: make(chan context.Context, 1), blockClose: blockClose,
		}
		returned := make(chan struct{})
		go func() {
			cleanupFailedPgStartup(context.Background(), conn, 10*time.Millisecond)
			close(returned)
		}()
		select {
		case <-returned:
		case <-time.After(time.Second):
			t.Fatalf("cleanup ignored its finite budget (Close blocks=%t)", blockClose)
		}
		ctx := <-conn.closeContext
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) || conn.closeCalls.Load() != 1 {
			t.Fatal("Close and CleanupDone did not share one deadline")
		}
	}
}

func TestPgStartupCleanupAlreadyComplete(t *testing.T) {
	conn := &recordingStartupCleanup{done: make(chan struct{}), closeContext: make(chan context.Context, 1)}
	close(conn.done)
	cleanupFailedPgStartup(context.Background(), conn, time.Second)
	ctx := <-conn.closeContext
	if conn.closeCalls.Load() != 1 || !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatal("completed cleanup did not return directly and release its timer")
	}
}

func (self *startupCleanupCountedConn) Close() error {
	err := self.Conn.Close()
	self.once.Do(func() { self.live.Add(-1) })
	return err
}

// The actual public pgxpool constructor owns an initial Ping. A transport read
// deadline makes that validation fail; a barrier holds pgx's separate cancel
// dial so its original transport cannot finish asynchronous cleanup yet. No
// socket, listener, external database, or new protocol implementation is used.
func TestPgAfterConnectFailureRetainsCapacityUntilCleanup(t *testing.T) {
	for _, canceledCaller := range []bool{false, true} {
		name := "validation_error"
		if canceledCaller {
			name = "abandoned_acquire"
		}
		t.Run(name, func(t *testing.T) {
			pingStarted := make(chan struct{})
			cancelStarted := make(chan struct{})
			replacementStarted := make(chan struct{})
			releaseQuery, releaseCancel := make(chan struct{}), make(chan struct{})
			var queryOnce, cancelOnce, cancelStartedOnce sync.Once
			releaseCleanup := func() {
				queryOnce.Do(func() { close(releaseQuery) })
				cancelOnce.Do(func() { close(releaseCancel) })
			}
			defer releaseCleanup()
			fixture := &pgPoolWireFixture{query: func(index int, query string) bool {
				if index == 1 && query == "-- ping" {
					close(pingStarted)
					<-releaseQuery
					return false
				}
				return true
			}}
			config, err := pgxpool.ParseConfig("host=synthetic-pg.example user=synthetic dbname=synthetic sslmode=disable connect_timeout=5")
			if err != nil {
				t.Fatal(err)
			}
			config.MinConns, config.MaxConns = 0, 1
			config.HealthCheckPeriod = time.Hour
			config.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
			config.ConnConfig.LookupFunc = func(context.Context, string) ([]string, error) {
				return []string{"192.0.2.1"}, nil
			}
			var live, peak, dialCount atomic.Int32
			config.ConnConfig.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
				// CancelRequest targets the original transport's RemoteAddr.
				// For net.Pipe this is "pipe"; pool startup still uses "tcp".
				if network == "pipe" {
					cancelStartedOnce.Do(func() { close(cancelStarted) })
					select {
					case <-releaseCancel:
						return nil, net.ErrClosed
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}
				conn, err := fixture.dial(ctx, network, address)
				if err != nil {
					return nil, err
				}
				current := live.Add(1)
				for old := peak.Load(); old < current && !peak.CompareAndSwap(old, current); old = peak.Load() {
				}
				if dialCount.Add(1) == 2 {
					close(replacementStarted)
				}
				return &startupCleanupCountedConn{Conn: conn, live: &live}, nil
			}
			startupMetrics := configurePgPoolLiveness(config)
			configurePgPoolWriteTracking(config)
			config.ShouldPing = func(context.Context, pgxpool.ShouldPingParams) bool { return false }
			validate := config.AfterConnect
			created := make(chan *pgconn.PgConn, 1)
			validationReturned := make(chan error, 1)
			var validations atomic.Int32
			config.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
				first := validations.Add(1) == 1
				if first {
					created <- conn.PgConn()
				}
				err := validate(ctx, conn)
				if first {
					validationReturned <- err
				}
				return err
			}
			pool, err := pgxpool.NewWithConfig(context.Background(), config)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				releaseCleanup()
				fixture.stateLock.Lock()
				fixture.closed = true
				connections := append([]net.Conn(nil), fixture.connections...)
				fixture.stateLock.Unlock()
				for _, conn := range connections {
					_ = conn.Close()
				}
				pool.Close()
				fixture.workers.Wait()
			})
			acquire := func(ctx context.Context) <-chan error {
				done := make(chan error, 1)
				go func() {
					conn, err := pool.Acquire(ctx)
					if conn != nil {
						conn.Release()
					}
					done <- err
				}()
				return done
			}
			await := func(ch <-chan struct{}, name string) {
				t.Helper()
				select {
				case <-ch:
				case <-time.After(2 * time.Second):
					t.Fatalf("did not reach %s", name)
				}
			}
			caller, cancelCaller := context.WithCancel(context.Background())
			defer cancelCaller()
			firstAcquire := acquire(caller)
			await(pingStarted, "initial validation")
			first := <-created
			if canceledCaller {
				cancelCaller()
				select {
				case err := <-firstAcquire:
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("abandoned Acquire returned %v", err)
					}
				case <-time.After(time.Second):
					t.Fatal("canceled caller waited for detached construction")
				}
			}
			if err := first.Conn().SetReadDeadline(time.Now()); err != nil {
				t.Fatal(err)
			}
			await(cancelStarted, "asynchronous cleanup")
			secondCtx, cancelSecond := context.WithCancel(context.Background())
			defer cancelSecond()
			secondAcquire := acquire(secondCtx)
			select {
			case <-replacementStarted:
				t.Fatalf("replacement started before failed-constructor cleanup: live=%d peak=%d pool_total=%d max=1", live.Load(), peak.Load(), pool.Stat().TotalConns())
			case <-time.After(100 * time.Millisecond):
			}
			select {
			case err := <-validationReturned:
				t.Fatalf("validation returned before CleanupDone: %v", err)
			default:
			}
			if pool.Stat().ConstructingConns() != 1 || pool.Stat().TotalConns() != 1 || live.Load() != 1 {
				t.Fatalf("cleanup lost ownership: constructing=%d total=%d live=%d", pool.Stat().ConstructingConns(), pool.Stat().TotalConns(), live.Load())
			}
			select {
			case <-first.CleanupDone():
				t.Fatal("test failed to hold actual pgx cleanup")
			default:
			}
			cancelSecond()
			select {
			case err := <-secondAcquire:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("waiting replacement returned %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("waiting replacement ignored cancellation")
			}
			releaseCleanup()
			await(first.CleanupDone(), "physical connection cleanup")
			var validationErr error
			select {
			case validationErr = <-validationReturned:
				if !errors.Is(validationErr, os.ErrDeadlineExceeded) {
					t.Fatalf("validation error changed during cleanup: %v", validationErr)
				}
			case <-time.After(time.Second):
				t.Fatal("constructor did not return after cleanup")
			}
			if !canceledCaller {
				select {
				case err := <-firstAcquire:
					if err != validationErr {
						t.Fatalf("Acquire error=%v; want exact validation error=%v", err, validationErr)
					}
				case <-time.After(time.Second):
					t.Fatal("Acquire did not return validation error")
				}
			}
			phases := startupMetrics.snapshot()
			if phases[pgPoolInitialPing].active != 0 || phases[pgPoolInitialPing].completed != [pgPoolStartupOutcomeCount]uint64{pgPoolStartupDeadline: 1} ||
				phases[pgPoolFailedStartupCleanup].active != 0 || phases[pgPoolFailedStartupCleanup].completed != [pgPoolStartupOutcomeCount]uint64{pgPoolStartupOk: 1} {
				t.Fatal("real pgx timeout or physical cleanup join was misclassified", phases)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			conn, err := pool.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Release()
			if _, err := conn.Exec(ctx, "SELECT 1"); err != nil {
				t.Fatal(err)
			}
			if dialCount.Load() != 2 || peak.Load() != 1 || live.Load() != 1 {
				t.Fatalf("recovery ownership: dials=%d peak=%d live=%d; want 2,1,1", dialCount.Load(), peak.Load(), live.Load())
			}
		})
	}
}
