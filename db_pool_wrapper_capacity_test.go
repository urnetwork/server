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
	"github.com/prometheus/client_golang/prometheus"
)

// Extend the existing borrowed-cleanup wire fixture to an entire 16-slot pool.
// The real Db and Tx wrappers return their canceled outcomes before physical
// cleanup completes. AcquiredConns must then include those destroying resources,
// and an unrelated operation must time out before entering its callback. This
// does not simulate a PostgreSQL backend or measure a Main cleanup duration.
func TestPgPoolWrapperLifecycleFullCancellation(t *testing.T) {
	for _, transaction := range []bool{false, true} {
		name := "db"
		if transaction {
			name = "tx"
		}
		t.Run(name, func(t *testing.T) {
			const capacity = 16
			const query = "SELECT 17 /* synthetic whole-pool cancellation */"
			queryEntered := make(chan struct{}, capacity)
			cleanupEntered := make(chan struct{}, capacity)
			releaseQueries, releaseCleanup := make(chan struct{}), make(chan struct{})
			var queriesOnce, cleanupOnce sync.Once
			unblockQueries := func() { queriesOnce.Do(func() { close(releaseQueries) }) }
			unblockCleanup := func() { cleanupOnce.Do(func() { close(releaseCleanup) }) }
			var live, peak, dials atomic.Int32
			_, pool := newPgPoolWireFixture(t, func(index int, got string) bool {
				if index <= capacity && got == query {
					queryEntered <- struct{}{}
					<-releaseQueries
					return false
				}
				return true
			}, func(context.Context, pgxpool.ShouldPingParams) bool { return false }, func(_ *pgPoolWireFixture, config *pgxpool.Config) {
				config.MaxConns = capacity
				config.HealthCheckPeriod = time.Hour
				dial := config.ConnConfig.DialFunc
				config.ConnConfig.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
					if network == "pipe" {
						cleanupEntered <- struct{}{}
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
					dials.Add(1)
					current := live.Add(1)
					for old := peak.Load(); old < current && !peak.CompareAndSwap(old, current); old = peak.Load() {
					}
					return &startupCleanupCountedConn{Conn: conn, live: &live}, nil
				}
			})
			run := func(ctx context.Context, borrowed func(*pgconn.PgConn)) {
				if transaction {
					txWithPool(ctx, pool, func(tx PgTx) {
						borrowed(tx.Conn().PgConn())
						RaisePgResult(tx.Exec(ctx, query))
					}, OptNoRetry())
				} else {
					dbWithPool(ctx, pool, func(conn PgConn) {
						borrowed(conn.Conn().PgConn())
						RaisePgResult(conn.Exec(ctx, query))
					}, OptNoRetry())
				}
			}
			caller, cancelCaller := context.WithCancel(t.Context())
			created := make(chan *pgconn.PgConn, capacity)
			outcomes := make(chan any, capacity)
			joined := make(chan struct{})
			var workers sync.WaitGroup
			var callbacks atomic.Int32
			t.Cleanup(func() {
				cancelCaller()
				unblockQueries()
				unblockCleanup()
				select {
				case <-joined:
				case <-time.After(3 * time.Second):
					t.Error("whole-pool callback workers did not join during cleanup")
				}
			})
			for range capacity {
				workers.Add(1)
				go func() {
					defer workers.Done()
					outcomes <- captureDbErrorPanic(func() {
						run(caller, func(conn *pgconn.PgConn) {
							callbacks.Add(1)
							// Preserve the physical handle before the wrapper releases
							// its pooled Conn. Never use a pooled Conn after Release.
							created <- conn
						})
					})
				}()
			}
			go func() { workers.Wait(); close(joined) }()
			await := func(ch <-chan struct{}, label string) {
				t.Helper()
				select {
				case <-ch:
				case <-time.After(3 * time.Second):
					t.Fatalf("did not reach %s", label)
				}
			}
			for range capacity {
				await(queryEntered, "all admitted query barriers")
			}
			originals := make([]*pgconn.PgConn, 0, capacity)
			for range capacity {
				originals = append(originals, <-created)
			}
			assertPgPoolWrapperLifecycleMetrics(t, pool, capacity, 0, 0)
			cancelCaller()
			for range capacity {
				await(cleanupEntered, "all detached cancellation transports")
			}
			await(joined, "all application outcomes before physical cleanup")
			for range capacity {
				err, ok := (<-outcomes).(error)
				if !ok || !errors.Is(err, context.Canceled) {
					t.Fatal("canceled operation lost its caller cause")
				}
			}
			// Server-side fixture handlers no longer block. The separately held
			// cancellation transport is sufficient to retain client cleanup.
			unblockQueries()
			for _, conn := range originals {
				select {
				case <-conn.CleanupDone():
					t.Fatal("physical cleanup completed while cancellation transport was held")
				default:
				}
			}
			stat := pool.open().Stat()
			if stat.AcquiredConns() != capacity || stat.IdleConns() != 0 || stat.ConstructingConns() != 0 || stat.TotalConns() != capacity || stat.MaxConns() != capacity || callbacks.Load() != capacity || dials.Load() != capacity || live.Load() != capacity {
				t.Fatalf("returned callbacks lost retained capacity: acquired=%d idle=%d constructing=%d total=%d callbacks=%d dials=%d live=%d", stat.AcquiredConns(), stat.IdleConns(), stat.ConstructingConns(), stat.TotalConns(), callbacks.Load(), dials.Load(), live.Load())
			}
			assertPgPoolWrapperLifecycleMetrics(t, pool, 0, 0, capacity)
			sibling, cancelSibling := context.WithTimeout(t.Context(), 250*time.Millisecond)
			defer cancelSibling()
			enteredSibling := false
			err, ok := captureDbErrorPanic(func() {
				run(sibling, func(*pgconn.PgConn) { enteredSibling = true })
			}).(error)
			if !ok || !errors.Is(err, context.DeadlineExceeded) || enteredSibling {
				t.Fatal("full cleanup pool did not stop the sibling at acquisition")
			}
			after := pool.open().Stat()
			if after.AcquiredConns() != capacity || after.TotalConns() != capacity || after.IdleConns() != 0 || after.AcquireCount() != stat.AcquireCount() || after.CanceledAcquireCount() != stat.CanceledAcquireCount()+1 || dials.Load() != capacity || peak.Load() != capacity {
				t.Fatal("failed sibling acquisition escaped retained physical capacity")
			}
			unblockCleanup()
			for _, conn := range originals {
				await(conn.CleanupDone(), "each original physical cleanup")
			}
			recovery, cancelRecovery := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancelRecovery()
			replacements := make([]*pgxpool.Conn, 0, capacity)
			defer func() {
				for _, conn := range replacements {
					conn.Release()
				}
			}()
			for range capacity {
				conn, err := pool.open().Acquire(recovery)
				if err != nil {
					t.Fatal("completed cleanup did not restore every pool slot")
				}
				replacements = append(replacements, conn)
				if _, err := conn.Exec(recovery, "SELECT 1"); err != nil {
					t.Fatal("replacement connection did not execute a healthy query")
				}
			}
			if pool.open().Stat().AcquiredConns() != capacity || dials.Load() != 2*capacity || peak.Load() != capacity || live.Load() != capacity {
				t.Fatal("recovery changed the cap or overlapped old physical resources")
			}
			for _, conn := range replacements {
				conn.Release()
			}
			if recovered := captureDbErrorPanic(func() { run(recovery, func(*pgconn.PgConn) {}) }); recovered != nil {
				t.Fatal("healthy operation through the same wrapper did not recover")
			}
			after = pool.open().Stat()
			if after.AcquiredConns() != 0 || after.IdleConns() != capacity || after.TotalConns() != capacity || after.NewConnsCount() != 2*capacity {
				t.Fatal("healthy recovery did not release all replacement capacity")
			}
			assertPgPoolWrapperLifecycleMetrics(t, pool, 0, 0, 0)
			t.Logf("wrapper=%s callbacks_returned=%d retained_acquired=%d sibling_entered=false recovered_idle=%d physical_peak=%d", name, callbacks.Load(), stat.AcquiredConns(), after.IdleConns(), peak.Load())
		})
	}
}

// The exact same metric assertions compile on the baseline producer. Missing
// families fail at the public metric boundary rather than at a new Go symbol.
func assertPgPoolWrapperLifecycleMetrics(t *testing.T, pool *safePgPool, owned, releasing, pending float64) {
	t.Helper()
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(newPgPoolMetricsCollector(map[string]pgPoolMetricsSource{"default": pool}))
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	assertPgPoolMetricValue(t, families, "urnetwork_pg_pool_wrapper_connections", "state", "owned", owned)
	assertPgPoolMetricValue(t, families, "urnetwork_pg_pool_wrapper_connections", "state", "releasing", releasing)
	assertPgPoolMetricValue(t, families, "urnetwork_pg_pool_wrapper_connections", "state", "cleanup_pending", pending)
	assertPgPoolMetricValue(t, families, "urnetwork_pg_pool_wrapper_cleanup_tracking_dropped_total", "pool", "default", 0)
}
