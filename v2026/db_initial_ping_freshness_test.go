package server

// Real pgx/Puddle and the existing in-memory protocol peer run in a Go fake
// clock bubble. Slow startup and the one-second idle boundary are exact;
// no wall-clock sleep, external pooler, or replacement idle policy is used.

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

// Holds a caller at the real pgx idle-Ping budget boundary until its actual
// deadline expires. Puddle's constructor delegates cancellation to its own
// context, so this hook cannot expire its independent initial Ping.
type pgExpireAtIdlePingBudget struct {
	context.Context
	once    sync.Once
	entered atomic.Bool
}

func (self *pgExpireAtIdlePingBudget) Deadline() (time.Time, bool) {
	deadline, ok := self.Context.Deadline()
	self.once.Do(func() {
		self.entered.Store(true)
		if ok {
			synctest.Sleep(time.Until(deadline))
		}
	})
	return deadline, ok
}

func pgFreshnessFixtureConfig(_ *pgPoolWireFixture, config *pgxpool.Config) {
	config.HealthCheckPeriod = time.Hour
}

// A slow backend-capacity wait before the initial EmptyQuery response must
// not turn that successful validation into an immediately redundant query.
func TestPgSlowConstructorReusesInitialPing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture, pool := newPgPoolWireFixture(t, func(_ int, query string) bool {
			if query == "-- ping" {
				synctest.Sleep(2 * time.Second)
			}
			return true
		}, nil, pgFreshnessFixtureConfig)
		conn, err := pool.open().Acquire(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		conn.Release()
		synctest.Wait()
		if fixture.pingCount != 1 || fixture.dialCount != 1 {
			t.Fatalf("slow constructor repeated its completed initial Ping: pings=%d dials=%d", fixture.pingCount, fixture.dialCount)
		}
		for range 3 {
			conn, err := pool.open().Acquire(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := conn.Exec(t.Context(), "SELECT 1"); err != nil {
				conn.Release()
				t.Fatal(err)
			}
			conn.Release()
		}
		synctest.Wait()
		if fixture.pingCount != 1 || fixture.dialCount != 1 || len(fixture.queries) != 4 {
			t.Fatal("fresh reuse added a preamble or replacement", fixture.pingCount, fixture.dialCount, len(fixture.queries))
		}
	})
}

// The old default policy destroys a healthy freshly validated socket when
// its redundant Ping meets an already-expired caller before writing bytes.
// With no redundant Ping, the exact same socket is acquired and reused.
func TestPgSlowConstructorAvoidsExpiredRedundantPing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var live atomic.Int32
		fixture, pool := newPgPoolWireFixture(t, func(_ int, query string) bool {
			if query == "-- ping" {
				synctest.Sleep(2 * time.Second)
			}
			return true
		}, nil, pgFreshnessFixtureConfig, func(_ *pgPoolWireFixture, config *pgxpool.Config) {
			dial := config.ConnConfig.DialFunc
			config.ConnConfig.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
				conn, err := dial(ctx, network, address)
				if err == nil && network == "tcp" {
					live.Add(1)
					conn = &startupCleanupCountedConn{Conn: conn, live: &live}
				}
				return conn, err
			}
		})
		caller, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		defer cancel()
		ctx := &pgExpireAtIdlePingBudget{Context: caller}
		conn, err := pool.open().Acquire(ctx)
		synctest.Wait()
		if err != nil || conn == nil {
			t.Fatalf("redundant idle Ping discarded fresh validation before callback: deadline=%t boundary=%t live=%d pings=%d dials=%d", errors.Is(err, context.DeadlineExceeded), ctx.entered.Load(), live.Load(), fixture.pingCount, fixture.dialCount)
		}
		first := conn.Conn().PgConn()
		if ctx.entered.Load() || fixture.pingCount != 1 || live.Load() != 1 {
			conn.Release()
			t.Fatal("fresh admission entered a second Ping or lost its physical socket")
		}
		// The caller can still expire before its first application statement.
		// Observe pgx's own pre-write refusal without any extra wire work.
		before := snapshotPgWrites(first.Conn())
		synctest.Sleep(time.Second)
		_, err = conn.Exec(ctx, "SELECT 991")
		if !errors.Is(err, context.DeadlineExceeded) || !pgconn.SafeToRetry(err) || !before.unchanged() || first.IsClosed() {
			conn.Release()
			t.Fatal("expired caller did not preserve pgx's exact pre-write boundary", err)
		}
		conn.Release()
		conn, err = pool.open().Acquire(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if conn.Conn().PgConn() != first {
			conn.Release()
			t.Fatal("healthy first socket was replaced after the caller expired")
		}
		conn.Release()
		synctest.Wait()
		if fixture.pingCount != 1 || fixture.dialCount != 1 || live.Load() != 1 {
			t.Fatal("pre-write refusal manufactured a validation or physical replacement")
		}
	})
}

// Once actually idle for more than a second, a healthy connection still
// receives pgx's bounded liveness Ping and is then reused without redialing.
func TestPgInitialPingFreshnessPreservesHealthyIdleCheck(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture, pool := newPgPoolWireFixture(t, nil, nil, pgFreshnessFixtureConfig)
		conn, err := pool.open().Acquire(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		first := conn.Conn().PgConn()
		conn.Release()
		synctest.Sleep(time.Second + time.Nanosecond)
		conn, err = pool.open().Acquire(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if conn.Conn().PgConn() != first {
			conn.Release()
			t.Fatal("healthy idle connection was replaced")
		}
		conn.Release()
		synctest.Wait()
		if fixture.pingCount != 2 || fixture.dialCount != 1 {
			t.Fatal("true idle health check was skipped or redialed", fixture.pingCount, fixture.dialCount)
		}
	})
}

// A genuinely idle failed socket must still be destroyed and fully cleaned
// before the max-one pool admits its independently validated replacement.
func TestPgInitialPingFreshnessPreservesFailedIdleCheck(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pings := 0
		fixture, pool := newPgPoolWireFixture(t, func(index int, query string) bool {
			if index == 1 && query == "-- ping" {
				pings++
				return pings != 2
			}
			return true
		}, nil, pgFreshnessFixtureConfig)
		conn, err := pool.open().Acquire(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		first := conn.Conn().PgConn()
		conn.Release()
		synctest.Sleep(time.Second + time.Nanosecond)
		conn, err = pool.open().Acquire(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if conn.Conn().PgConn() == first || !first.IsClosed() {
			conn.Release()
			t.Fatal("idle failure reused the old connection")
		}
		select {
		case <-first.CleanupDone():
		default:
			conn.Release()
			t.Fatal("replacement preceded old physical cleanup")
		}
		conn.Release()
		synctest.Wait()
		if fixture.pingCount != 3 || fixture.dialCount != 2 {
			t.Fatal("idle replacement changed validation ownership", fixture.pingCount, fixture.dialCount)
		}
	})
}

// A caller may leave while a transaction pooler has no backend for its
// initial Ping. The detached successful constructor is usable by the next
// caller immediately, but its freshness must still expire while it sits idle.
func TestPgInitialPingFreshnessExpiresAfterAbandonedAcquire(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pingStarted, capacity := make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		release := func() { releaseOnce.Do(func() { close(capacity) }) }
		fixture, pool := newPgPoolWireFixture(t, func(index int, query string) bool {
			if index == 1 && query == "-- ping" {
				select {
				case <-pingStarted:
				default:
					close(pingStarted)
					<-capacity
				}
			}
			return true
		}, nil, pgFreshnessFixtureConfig)
		t.Cleanup(release)
		caller, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		result := make(chan error, 1)
		go func() {
			conn, err := pool.open().Acquire(caller)
			if conn != nil {
				conn.Release()
			}
			result <- err
		}()
		<-pingStarted
		synctest.Sleep(2 * time.Second)
		if err := <-result; !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("short caller did not leave its detached constructor", err)
		}
		snapshot, _ := pool.metricSnapshot()
		if snapshot.constructingConnections != 1 || snapshot.startup[pgPoolInitialPing].active != 1 || snapshot.newConnections != 1 || snapshot.startup[pgPoolFailedStartupCleanup].active != 0 {
			t.Fatal("backend-capacity wait was not held in initial Ping", snapshot)
		}
		release()
		synctest.Wait()
		conn, err := pool.open().Acquire(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		conn.Release()
		synctest.Wait()
		if fixture.pingCount != 1 || fixture.dialCount != 1 {
			t.Fatal("abandoned slow constructor received a redundant Ping", fixture.pingCount, fixture.dialCount)
		}
		synctest.Sleep(time.Second + time.Nanosecond)
		conn, err = pool.open().Acquire(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		conn.Release()
		synctest.Wait()
		if fixture.pingCount != 2 || fixture.dialCount != 1 {
			t.Fatal("abandoned constructor's freshness failed to expire", fixture.pingCount, fixture.dialCount)
		}
	})
}

// The new phase gauges discriminate login/startup from a queued initial
// backend query. Neither has yet admitted a database callback.
func TestPgPoolStartupMetricsSeparateLoginAndBackendWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		loginStarted, loginReady := make(chan struct{}), make(chan struct{})
		pingStarted, backendReady := make(chan struct{}), make(chan struct{})
		var loginOnce, backendOnce sync.Once
		releaseLogin := func() { loginOnce.Do(func() { close(loginReady) }) }
		releaseBackend := func() { backendOnce.Do(func() { close(backendReady) }) }
		fixture, pool := newPgPoolWireFixture(t, func(_ int, query string) bool {
			if query == "-- ping" {
				select {
				case <-pingStarted:
				default:
					close(pingStarted)
					<-backendReady
				}
			}
			return true
		}, nil, pgFreshnessFixtureConfig, func(fixture *pgPoolWireFixture, _ *pgxpool.Config) {
			fixture.beforeStartup = func(_ int) { close(loginStarted); <-loginReady }
		})
		t.Cleanup(releaseBackend)
		t.Cleanup(releaseLogin)
		result := make(chan error, 1)
		go func() {
			conn, err := pool.open().Acquire(t.Context())
			if conn != nil {
				conn.Release()
			}
			result <- err
		}()
		<-loginStarted
		synctest.Wait()
		snapshot, _ := pool.metricSnapshot()
		if snapshot.constructingConnections != 1 || snapshot.newConnections != 1 || snapshot.startup != ([pgPoolStartupPhaseCount]pgPoolStartupPhaseSnapshot{}) {
			t.Fatal("login wait was incorrectly labeled as Ping or cleanup", snapshot)
		}
		synctest.Sleep(2 * time.Second)
		releaseLogin()
		<-pingStarted
		synctest.Wait()
		snapshot, _ = pool.metricSnapshot()
		if snapshot.constructingConnections != 1 || snapshot.startup[pgPoolInitialPing].active != 1 || snapshot.startup[pgPoolFailedStartupCleanup].active != 0 {
			t.Fatal("initial backend query wait was not separately observed", snapshot)
		}
		releaseBackend()
		if err := <-result; err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if fixture.pingCount != 1 || fixture.dialCount != 1 {
			t.Fatal("slow login manufactured a redundant validation", fixture.pingCount, fixture.dialCount)
		}
	})
}

// A missing or malformed freshness record is never permission to bypass the
// original idle boundary. This uses an actual validated pgx connection.
func TestPgInitialPingFreshnessUnknownDefaultsToIdleCheck(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, pool := newPgPoolWireFixture(t, nil, nil, pgFreshnessFixtureConfig)
		conn, err := pool.open().Acquire(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Release()
		policy := pool.open().Config().ShouldPing
		if policy == nil {
			t.Fatal("production freshness policy is absent")
		}
		data := conn.Conn().PgConn().CustomData()
		for key := range data {
			data[key] = "synthetic malformed record"
		}
		for _, clearRecord := range []bool{false, true} {
			if clearRecord {
				clear(data)
			}
			if !policy(t.Context(), pgxpool.ShouldPingParams{Conn: conn.Conn(), IdleDuration: 2 * time.Second}) ||
				policy(t.Context(), pgxpool.ShouldPingParams{Conn: conn.Conn(), IdleDuration: time.Second}) {
				t.Fatal("unknown freshness did not preserve the original idle boundary")
			}
		}
	})
}
