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

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A borrowed connection can become closed before pgx finishes its detached
// cleanup. The existing in-memory fixture controls cleanup using DialFunc;
// no database, listener, external address or custom protocol scenario is used.
func TestDbBorrowedFailureRetainsCapacityUntilCleanup(t *testing.T) {
	for _, canceledCaller := range []bool{false, true} {
		name := "read_timeout"
		if canceledCaller {
			name = "canceled_caller"
		}
		t.Run(name, func(t *testing.T) {
			queryStarted, cleanupStarted := make(chan struct{}), make(chan struct{})
			releaseQuery, releaseCleanup := make(chan struct{}), make(chan struct{})
			replacementStarted := make(chan struct{})
			var queryOnce, cleanupOnce, startedOnce sync.Once
			unblock := func() {
				queryOnce.Do(func() { close(releaseQuery) })
				cleanupOnce.Do(func() { close(releaseCleanup) })
			}
			var live, peak, dialCount atomic.Int32
			_, pool := newPgPoolWireFixture(t, func(index int, query string) bool {
				if index == 1 && query == "SELECT 1" {
					close(queryStarted)
					<-releaseQuery
					return false
				}
				return true
			}, func(context.Context, pgxpool.ShouldPingParams) bool { return false }, func(fixture *pgPoolWireFixture, config *pgxpool.Config) {
				config.HealthCheckPeriod = time.Hour
				dial := config.ConnConfig.DialFunc
				config.ConnConfig.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
					if network == "pipe" {
						startedOnce.Do(func() { close(cleanupStarted) })
						select {
						case <-releaseCleanup:
							return nil, net.ErrClosed
						case <-ctx.Done():
							return nil, ctx.Err()
						}
					}
					conn, err := dial(ctx, network, address)
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
			})
			caller, cancelCaller := context.WithCancel(context.Background())
			second, cancelSecond := context.WithCancel(context.Background())
			firstDone, secondDone := make(chan struct{}), make(chan struct{})
			var secondStarted bool
			t.Cleanup(func() {
				cancelCaller()
				cancelSecond()
				unblock()
				select {
				case <-firstDone:
				case <-time.After(2 * time.Second):
					t.Error("first callback worker did not join")
				}
				if secondStarted {
					select {
					case <-secondDone:
					case <-time.After(2 * time.Second):
						t.Error("second Acquire worker did not join")
					}
				}
			})
			await := func(ch <-chan struct{}, label string) {
				t.Helper()
				select {
				case <-ch:
				case <-time.After(2 * time.Second):
					t.Fatalf("did not reach %s", label)
				}
			}
			created := make(chan *pgconn.PgConn, 1)
			var firstError error
			var recovered any
			var callbacks atomic.Int32
			go func() {
				defer close(firstDone)
				recovered = captureDbErrorPanic(func() {
					dbWithPool(caller, pool, func(conn PgConn) {
						callbacks.Add(1)
						created <- conn.Conn().PgConn()
						_, firstError = conn.Exec(caller, "SELECT 1")
						Raise(firstError)
					}, OptNoRetry())
				})
			}()
			await(queryStarted, "borrowed callback query")
			first := <-created
			if canceledCaller {
				cancelCaller()
			} else if err := first.Conn().SetReadDeadline(time.Now()); err != nil {
				t.Fatal(err)
			}
			await(cleanupStarted, "asynchronous cleanup")
			await(firstDone, "callback outcome without waiting for detached cleanup")
			if canceledCaller {
				if recovered != firstError || !errors.Is(firstError, context.Canceled) {
					t.Fatalf("canceled callback outcome changed: recovered=%v first=%v", recovered, firstError)
				}
			} else if recovered != firstError || !errors.Is(firstError, os.ErrDeadlineExceeded) {
				t.Fatalf("callback outcome changed: recovered=%v first=%v", recovered, firstError)
			}
			if callbacks.Load() != 1 {
				t.Fatal("failed callback was replayed")
			}
			select {
			case <-first.CleanupDone():
				t.Fatal("fixture failed to hold physical cleanup")
			default:
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
			select {
			case <-replacementStarted:
				t.Fatalf("replacement started before borrowed cleanup: live=%d peak=%d pool_total=%d max=1", live.Load(), peak.Load(), pool.open().Stat().TotalConns())
			case <-time.After(100 * time.Millisecond):
			}
			if pool.open().Stat().TotalConns() != 1 || live.Load() != 1 {
				t.Fatalf("borrowed cleanup lost accounting: pool_total=%d live=%d", pool.open().Stat().TotalConns(), live.Load())
			}
			cancelSecond()
			await(secondDone, "canceled replacement Acquire")
			if !errors.Is(secondError, context.Canceled) {
				t.Fatalf("replacement Acquire outcome=%v; want canceled", secondError)
			}
			unblock()
			await(first.CleanupDone(), "physical cleanup completion")
			recovery, cancelRecovery := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancelRecovery()
			conn, err := pool.open().Acquire(recovery)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Release()
			if _, err := conn.Exec(recovery, "SELECT 1"); err != nil {
				t.Fatal(err)
			}
			if dialCount.Load() != 2 || peak.Load() != 1 || live.Load() != 1 {
				t.Fatalf("recovery dials=%d peak=%d live=%d; want 2,1,1", dialCount.Load(), peak.Load(), live.Load())
			}
		})
	}
}

// A classified connection error must discard even a physically open session;
// an ordinary callback error must continue to release its healthy session.
func TestDbBorrowedCleanupPreservesErrorAndReuse(t *testing.T) {
	for _, badConnection := range []bool{false, true} {
		name := "ordinary_error_reuses"
		var expected error = errors.New("ordinary callback error")
		if badConnection {
			name = "connection_error_discards"
			expected = &pgconn.PgError{Code: "08006", Message: "fixture connection failure"}
		}
		t.Run(name, func(t *testing.T) {
			_, pool := newPgPoolWireFixture(t, nil, func(context.Context, pgxpool.ShouldPingParams) bool { return false })
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			var first *pgconn.PgConn
			callbacks := 0
			recovered := captureDbErrorPanic(func() {
				dbWithPool(ctx, pool, func(conn PgConn) {
					callbacks++
					first = conn.Conn().PgConn()
					RaisePgResult(conn.Exec(ctx, "SELECT 1"))
					panic(expected)
				}, OptNoRetry())
			})
			if recovered != expected || callbacks != 1 {
				t.Fatalf("callback outcome=%v count=%d; want exact original error and one callback", recovered, callbacks)
			}
			conn, err := pool.open().Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Release()
			if same := first == conn.Conn().PgConn(); same == badConnection {
				t.Fatalf("connection reused=%t after classified connection error=%t", same, badConnection)
			}
			select {
			case <-first.CleanupDone():
				if !badConnection {
					t.Fatal("ordinary callback error disposed a healthy connection")
				}
			default:
				if badConnection {
					t.Fatal("replacement began before classified connection disposal")
				}
			}
		})
	}
}
