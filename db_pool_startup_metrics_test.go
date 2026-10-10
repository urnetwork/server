package server

// Startup observations follow explicit wire and cleanup barriers. No external
// database, listener, clock delay or error-text classification owns the proof.

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
)

// Acquisition cancellation does not hide its detached constructor's Ping.
func TestPgPoolStartupMetricsHeldInitialPing(t *testing.T) {
	for _, cancelAcquire := range []bool{false, true} {
		pingStarted, releasePing := make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		release := func() { releaseOnce.Do(func() { close(releasePing) }) }
		fixture, pool := newPgPoolWireFixture(t, func(_ int, query string) bool {
			if query == "-- ping" {
				close(pingStarted)
				<-releasePing
			}
			return true
		}, neverPingTestPool)
		t.Cleanup(release)
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		acquired := make(chan error, 1)
		go func() {
			conn, err := pool.open().Acquire(ctx)
			if conn != nil {
				conn.Release()
			}
			acquired <- err
		}()
		awaitPgStartupMetricBarrier(t, pingStarted)
		if cancelAcquire {
			cancel()
			if err := awaitPgStartupMetricResult(t, acquired); !errors.Is(err, context.Canceled) {
				t.Fatal("Acquire cancellation changed", err)
			}
		}
		snapshot, ok := pool.metricSnapshot()
		if !ok || snapshot.constructingConnections != 1 || snapshot.startup[pgPoolInitialPing].active != 1 ||
			snapshot.startup[pgPoolInitialPing].durationSeconds != 0 || snapshot.startup[pgPoolInitialPing].completed != [pgPoolStartupOutcomeCount]uint64{} ||
			snapshot.startup[pgPoolFailedStartupCleanup] != (pgPoolStartupPhaseSnapshot{}) {
			t.Fatal("held initial Ping was not observed independently of its caller", cancelAcquire, snapshot.startup)
		}
		release()
		if !cancelAcquire {
			if err := awaitPgStartupMetricResult(t, acquired); err != nil {
				t.Fatal(err)
			}
		}
		// This admission joins the detached constructor in either caller case.
		dbWithPool(t.Context(), pool, func(conn PgConn) {
			RaisePgResult(conn.Exec(t.Context(), "SELECT 1"))
		}, OptNoRetry())
		snapshot, _ = pool.metricSnapshot()
		phase := snapshot.startup[pgPoolInitialPing]
		if phase.active != 0 || phase.completed != [pgPoolStartupOutcomeCount]uint64{pgPoolStartupOk: 1} || phase.durationSeconds < 0 ||
			snapshot.startup[pgPoolFailedStartupCleanup] != (pgPoolStartupPhaseSnapshot{}) {
			t.Fatal("successful constructor phases are incorrect", snapshot.startup)
		}
		fixture.stateLock.Lock()
		dials, pings := fixture.dialCount, fixture.pingCount
		fixture.stateLock.Unlock()
		if dials != 1 || pings != 1 {
			t.Fatal("observation added a connection or Ping", dials, pings)
		}
	}
}

// A failed phase retains its actual pgx error while disposal completes.
func TestPgPoolStartupMetricsFailedPingResultPreserved(t *testing.T) {
	_, pool := newPgPoolWireFixture(t, nil, neverPingTestPool, func(fixture *pgPoolWireFixture, _ *pgxpool.Config) {
		fixture.queryError = func(_ int, query string) *pgproto3.ErrorResponse {
			if query == "-- ping" {
				return &pgproto3.ErrorResponse{Severity: "ERROR", Code: "XX000", Message: "synthetic validation failure"}
			}
			return nil
		}
	})
	conn, err := pool.open().Acquire(t.Context())
	if conn != nil {
		conn.Release()
		t.Fatal("failed initial Ping admitted a connection")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "XX000" || pgErr.Message != "synthetic validation failure" {
		t.Fatal("phase metrics changed the original validation error", err)
	}
	snapshot, _ := pool.metricSnapshot()
	if snapshot.startup[pgPoolInitialPing].active != 0 || snapshot.startup[pgPoolInitialPing].completed != [pgPoolStartupOutcomeCount]uint64{pgPoolStartupOther: 1} ||
		snapshot.startup[pgPoolFailedStartupCleanup].active != 0 || snapshot.startup[pgPoolFailedStartupCleanup].completed != [pgPoolStartupOutcomeCount]uint64{pgPoolStartupOk: 1} {
		t.Fatal("failed validation or joined cleanup outcome is missing", snapshot.startup)
	}
}

// Close returning, including its secondary error, is not physical cleanup.
func TestPgPoolStartupMetricsCleanupJoins(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	metrics := &pgPoolStartupMetrics{}
	conn := &recordingStartupCleanup{done: make(chan struct{}), closeContext: make(chan context.Context, 1)}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(conn.done) }) }
	t.Cleanup(release)
	returned := make(chan error, 1)
	go func() { returned <- metrics.cleanupFailedStartup(ctx, conn, PgStartupCleanupTimeout) }()
	var cleanupCtx context.Context
	select {
	case cleanupCtx = <-conn.closeContext:
	case <-time.After(2 * time.Second):
		t.Fatal("cleanup did not enter Close")
	}
	if cleanupCtx.Err() != nil {
		t.Fatal("observation inherited Acquire cancellation")
	}
	phase := metrics.snapshot()[pgPoolFailedStartupCleanup]
	if phase.active != 1 || phase.completed != [pgPoolStartupOutcomeCount]uint64{} || phase.durationSeconds != 0 {
		t.Fatal("Close return falsely completed cleanup", phase)
	}
	release()
	if err := awaitPgStartupMetricResult(t, returned); err != nil {
		t.Fatal("physical join did not win over secondary Close failure", err)
	}
	phase = metrics.snapshot()[pgPoolFailedStartupCleanup]
	if phase.active != 0 || phase.completed != [pgPoolStartupOutcomeCount]uint64{pgPoolStartupOk: 1} || conn.closeCalls.Load() != 1 {
		t.Fatal("cleanup join was not counted exactly once", phase)
	}
}

// An already-expired cleanup budget is deterministic and cannot claim a join.
func TestPgPoolStartupMetricsCleanupDeadlineIsNotJoin(t *testing.T) {
	metrics := &pgPoolStartupMetrics{}
	conn := &recordingStartupCleanup{done: make(chan struct{}), closeContext: make(chan context.Context, 1)}
	err := metrics.cleanupFailedStartup(t.Context(), conn, 0)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("cleanup budget outcome changed", err)
	}
	phase := metrics.snapshot()[pgPoolFailedStartupCleanup]
	if phase.active != 0 || phase.completed != [pgPoolStartupOutcomeCount]uint64{pgPoolStartupDeadline: 1} {
		t.Fatal("expired cleanup was mislabeled as a physical join", phase)
	}
	select {
	case <-conn.done:
		t.Fatal("observation disposed of the synthetic connection")
	default:
	}
	close(conn.done)
	if metrics.snapshot()[pgPoolFailedStartupCleanup] != phase {
		t.Fatal("later physical completion rewrote the recorded phase outcome")
	}
}

// Labels and durations stay finite and typed even for arbitrary error text.
func TestPgPoolStartupMetricsFixedOutcomesAndDurations(t *testing.T) {
	metrics := &pgPoolStartupMetrics{}
	outcomes := []error{nil, context.DeadlineExceeded, context.Canceled, errors.New("synthetic identity must not become a label")}
	for phase := pgPoolInitialPing; phase < pgPoolStartupPhaseCount; phase++ {
		for range outcomes {
			metrics.begin(phase)
		}
		if metrics.snapshot()[phase].active != int64(len(outcomes)) {
			t.Fatal("simultaneous phase observations were lost")
		}
		for _, err := range outcomes {
			metrics.finish(phase, time.Second, err)
		}
	}
	// A socket deadline shares the fixed context-deadline class.
	metrics.begin(pgPoolInitialPing)
	metrics.finish(pgPoolInitialPing, 500*time.Millisecond, os.ErrDeadlineExceeded)
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(newPgPoolMetricsCollector(map[string]pgPoolMetricsSource{
		"default": pgPoolMetricsFixtureSource{ready: true, snapshot: pgPoolMetricSnapshot{startup: metrics.snapshot()}},
	}))
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	observed := 0
	for _, family := range families {
		name := family.GetName()
		if name != "urnetwork_pg_pool_startup_phases_active" && name != "urnetwork_pg_pool_startup_phases_completed_total" && name != "urnetwork_pg_pool_startup_phase_duration_seconds_total" {
			continue
		}
		for _, metric := range family.Metric {
			observed++
			labels := map[string]string{}
			for _, label := range metric.Label {
				labels[label.GetName()] = label.GetValue()
			}
			phase := labels["phase"]
			if labels["pool"] != "default" || (phase != "initial_ping" && phase != "failed_startup_cleanup") {
				t.Fatal("startup identity escaped fixed labels", labels)
			}
			want, labelCount := float64(0), 2
			if name == "urnetwork_pg_pool_startup_phases_completed_total" {
				labelCount, want = 3, 1
				outcome := labels["outcome"]
				if outcome != "ok" && outcome != "deadline" && outcome != "canceled" && outcome != "other" {
					t.Fatal("unbounded startup outcome", labels)
				}
				if phase == "initial_ping" && outcome == "deadline" {
					want = 2
				}
			} else if name == "urnetwork_pg_pool_startup_phase_duration_seconds_total" {
				want = 4
				if phase == "initial_ping" {
					want = 4.5
				}
			}
			got := metric.GetGauge().GetValue()
			if metric.Counter != nil {
				got = metric.GetCounter().GetValue()
			}
			if len(labels) != labelCount || got != want {
				t.Fatal("startup metric value or label shape changed", name, labels, got, want)
			}
		}
	}
	if observed != 12 {
		t.Fatal("startup metric series coverage incomplete", observed)
	}
}

// Each configured pool captures a new observer; prior phase counts cannot leak.
func TestPgPoolStartupMetricsSeparateGenerations(t *testing.T) {
	_, first := newPgPoolWireFixture(t, nil, neverPingTestPool)
	dbWithPool(t.Context(), first, func(PgConn) {}, OptNoRetry())
	_, second := newPgPoolWireFixture(t, nil, neverPingTestPool)
	if first.startupMetrics == second.startupMetrics || second.startupMetrics.snapshot() != [pgPoolStartupPhaseCount]pgPoolStartupPhaseSnapshot{} {
		t.Fatal("new pool inherited prior generation's startup observations")
	}
	dbWithPool(t.Context(), second, func(PgConn) {}, OptNoRetry())
	for _, pool := range []*safePgPool{first, second} {
		if pool.startupMetrics.snapshot()[pgPoolInitialPing].completed != [pgPoolStartupOutcomeCount]uint64{pgPoolStartupOk: 1} {
			t.Fatal("pool generations share a startup completion counter")
		}
	}
	first.close()
	if _, ok := first.metricSnapshot(); ok {
		t.Fatal("closed pool still emits startup series")
	}
}

// Deadlines bound failed tests; completion channels establish every ordering.
func awaitPgStartupMetricBarrier(t *testing.T, barrier <-chan struct{}) {
	t.Helper()
	select {
	case <-barrier:
	case <-time.After(2 * time.Second):
		t.Fatal("startup fixture did not reach its explicit barrier")
	}
}

// A buffered return channel joins the exact owned fixture operation.
func awaitPgStartupMetricResult(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("startup fixture operation did not join")
		return nil
	}
}
