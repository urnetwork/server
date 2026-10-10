// Exact pool and pgx completion controls use only the existing in-memory wire fixture.
package server

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Observe only the synthetic control statements, not startup pings.
type queryScopeTestTracer struct {
	stateLock sync.Mutex
	ends      []pgx.TraceQueryEndData
}

// Each call shadows earlier instrumentation context without retaining arguments.
func (self *queryScopeTestTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, self, data.SQL == "SELECT synthetic_rows" || data.SQL == "SELECT synthetic_denied")
}

// Callback publication follows actual driver result completion.
func (self *queryScopeTestTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if matched, _ := ctx.Value(self).(bool); matched {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.ends = append(self.ends, data)
	}
}

// Every wait has a fresh rescue budget; ordering itself is channel-driven.
func queryScopeTestJoin(t testing.TB, done <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("query scope owner did not join")
	}
}

// A closed real pool must reject acquisition without opening any connection.
func queryScopeTestClosed(t testing.TB, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := pool.Acquire(ctx)
	if err == nil {
		conn.Release()
		t.Fatal("retired query scope pool remained usable")
	}
}

// A generated name is necessary but both pool resources must name that same fixture.
func TestPgQueryScopeRequiresDisposableFixture(t *testing.T) {
	first := "test_1_00000000000000000000000000000001"
	second := "test_1_00000000000000000000000000000002"
	if err := testPgQueryScopeFixture("local", [2]string{first, first}); err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct {
		env   string
		names [2]string
	}{
		{env: "main", names: [2]string{first, first}},
		{env: "local", names: [2]string{"synthetic_application", "synthetic_application"}},
		{env: "local", names: [2]string{first, second}},
	} {
		if testPgQueryScopeFixture(check.env, check.names) == nil {
			t.Fatal("nonfixture query scope accepted")
		}
	}
}

// A real driver's partial-read Close drains to the full returned-row command tag.
func TestPgQueryScopeObservesDriverCompletionAndErrors(t *testing.T) {
	var fixture *pgPoolWireFixture
	var normal *safePgPool
	previous := &queryScopeTestTracer{}
	fixture, normal = newPgPoolWireFixture(t, func(index int, query string) bool {
		if query != "SELECT synthetic_rows" {
			return true
		}
		fixture.stateLock.Lock()
		peer := fixture.connections[2*(index-1)+1]
		fixture.stateLock.Unlock()
		backend := pgproto3.NewBackend(peer, peer)
		backend.Send(&pgproto3.RowDescription{Fields: []pgproto3.FieldDescription{{Name: []byte("value"), DataTypeOID: 23, DataTypeSize: 4, TypeModifier: -1}}})
		backend.Send(&pgproto3.DataRow{Values: [][]byte{[]byte("1")}})
		backend.Send(&pgproto3.DataRow{Values: [][]byte{[]byte("2")}})
		backend.Send(&pgproto3.CommandComplete{CommandTag: []byte("SELECT 2")})
		backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
		_ = backend.Flush()
		return false
	}, nil, func(fixture *pgPoolWireFixture, config *pgxpool.Config) {
		config.ConnConfig.Tracer = previous
		fixture.queryError = func(_ int, query string) *pgproto3.ErrorResponse {
			if query == "SELECT synthetic_denied" {
				return &pgproto3.ErrorResponse{Severity: "ERROR", Code: "42501", Message: "synthetic restricted diagnostic"}
			}
			return nil
		}
	})
	_, maintenance := newPgPoolWireFixture(t, nil, nil)
	priorNormal, priorMaintenance := normal.open(), maintenance.open()
	observer := &queryScopeTestTracer{}
	scope, err := newTestPgQueryScope(t.Context(), observer, [2]*safePgPool{normal, maintenance}, pgxpool.NewWithConfig, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := scope.Close(); err != nil {
			t.Error(err)
		}
	}()
	for _, owner := range scope.poolOwners {
		original, observed := owner.prior.Config(), owner.traced.Config()
		if observed.MaxConns != original.MaxConns || observed.MinConns != original.MinConns || observed.PingTimeout != original.PingTimeout || observed.AfterConnect == nil || observed.ConnConfig.DialFunc == nil || observed.ConnConfig.OnPgError == nil {
			t.Fatal("query scope changed pool policy or lost existing hooks")
		}
		if _, ok := observed.ConnConfig.Tracer.(pgx.BatchTracer); !ok {
			t.Fatal("composed tracer lost optional interfaces")
		}
	}
	conn, err := normal.open().Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	func() {
		defer conn.Release()
		rows, err := conn.Query(t.Context(), "SELECT synthetic_rows")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		if !rows.Next() {
			t.Fatal("synthetic result omitted first row")
		}
		rows.Close()
		if rows.Err() != nil {
			t.Fatal(rows.Err())
		}
		if err := conn.Conn().Close(t.Context()); err != nil {
			t.Fatal(err)
		}
	}()
	conn, err = normal.open().Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	func() { defer conn.Release(); _, err = conn.Exec(t.Context(), "SELECT synthetic_denied") }()
	var denied *pgconn.PgError
	if !errors.As(err, &denied) || denied.Code != "42501" {
		t.Fatal("synthetic restricted-setting refusal was not returned")
	}
	if err := scope.Close(); err != nil {
		t.Fatal(err)
	}
	if normal.open() != priorNormal || maintenance.open() != priorMaintenance {
		t.Fatal("both prior pool identities were not restored")
	}
	for _, tracer := range []*queryScopeTestTracer{observer, previous} {
		if len(tracer.ends) != 2 || tracer.ends[0].CommandTag.RowsAffected() != 2 || tracer.ends[0].Err != nil || !errors.As(tracer.ends[1].Err, &denied) {
			t.Fatal("driver row count/error or existing tracer was lost")
		}
	}
}

// Partial construction closes every produced pool without publishing either identity.
func TestPgQueryScopeConstructionFailureAndGoexitRollback(t *testing.T) {
	for _, arm := range []string{"error", "pool-and-error", "goexit"} {
		func() {
			_, normal := newPgPoolWireFixture(t, nil, nil)
			_, maintenance := newPgPoolWireFixture(t, nil, nil)
			priors := [2]*pgxpool.Pool{normal.open(), maintenance.open()}
			clones := []*pgxpool.Pool{}
			done := make(chan struct{})
			defer func() {
				queryScopeTestJoin(t, done)
				// Backup ownership is independent of the constructor under test.
				normal.mutex.Lock()
				normal.pool = priors[0]
				normal.mutex.Unlock()
				maintenance.mutex.Lock()
				maintenance.pool = priors[1]
				maintenance.mutex.Unlock()
				for _, pool := range clones {
					pool.Close()
				}
			}()
			go func() {
				defer close(done)
				_, _ = newTestPgQueryScope(t.Context(), &queryScopeTestTracer{}, [2]*safePgPool{normal, maintenance}, func(ctx context.Context, config *pgxpool.Config) (*pgxpool.Pool, error) {
					if len(clones) == 1 && arm == "goexit" {
						runtime.Goexit()
					}
					if len(clones) == 1 && arm == "error" {
						return nil, errors.New("synthetic second constructor failure")
					}
					pool, err := pgxpool.NewWithConfig(ctx, config)
					if err != nil {
						return nil, err
					}
					clones = append(clones, pool)
					if len(clones) == 2 {
						return pool, errors.New("synthetic constructed-pool failure")
					}
					return pool, nil
				}, "synthetic")
			}()
			queryScopeTestJoin(t, done)
			if normal.open() != priors[0] || maintenance.open() != priors[1] || len(clones) == 0 {
				t.Fatal("partial construction changed prior ownership")
			}
			for _, pool := range clones {
				queryScopeTestClosed(t, pool)
			}
		}()
	}
}

// A borrower admitted while clones construct is rejected before publication.
func TestPgQueryScopeRejectsAcquiredAndNestedScopes(t *testing.T) {
	_, normal := newPgPoolWireFixture(t, nil, nil)
	_, maintenance := newPgPoolWireFixture(t, nil, nil)
	priors := [2]*pgxpool.Pool{normal.open(), maintenance.open()}
	var held *pgxpool.Conn
	defer func() {
		if held != nil {
			held.Release()
		}
	}()
	clones := []*pgxpool.Pool{}
	defer func() {
		// Keep a broken construction guard from leaking its test-owned clones.
		normal.mutex.Lock()
		normal.pool = priors[0]
		normal.mutex.Unlock()
		maintenance.mutex.Lock()
		maintenance.pool = priors[1]
		maintenance.mutex.Unlock()
		for _, pool := range clones {
			pool.Close()
		}
	}()
	_, err := newTestPgQueryScope(t.Context(), &queryScopeTestTracer{}, [2]*safePgPool{normal, maintenance}, func(ctx context.Context, config *pgxpool.Config) (*pgxpool.Pool, error) {
		pool, err := pgxpool.NewWithConfig(ctx, config)
		if err != nil {
			return nil, err
		}
		clones = append(clones, pool)
		if len(clones) == 2 {
			held, err = priors[0].Acquire(ctx)
		}
		return pool, err
	}, "synthetic")
	if err == nil || held == nil || normal.open() != priors[0] || maintenance.open() != priors[1] {
		t.Fatal("construction-time borrowed connection was displaced")
	}
	for _, pool := range clones {
		queryScopeTestClosed(t, pool)
	}
	held.Release()
	held = nil
	scope, err := newTestPgQueryScope(t.Context(), &queryScopeTestTracer{}, [2]*safePgPool{normal, maintenance}, pgxpool.NewWithConfig, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	defer scope.Close()
	if _, err := newTestPgQueryScope(t.Context(), &queryScopeTestTracer{}, [2]*safePgPool{normal, maintenance}, pgxpool.NewWithConfig, "synthetic"); err == nil {
		t.Fatal("nested scope accepted")
	}
	for _, targets := range [][2]*safePgPool{{normal, nil}, {normal, normal}} {
		if _, err := newTestPgQueryScope(t.Context(), &queryScopeTestTracer{}, targets, pgxpool.NewWithConfig, "synthetic"); err == nil {
			t.Fatal("invalid scope targets accepted")
		}
	}
}

// The actual held destructor proves restore-before-close; repeated calls return
// the same result. Concurrent waiter joining itself is sync.Once's contract.
func TestPgQueryScopeRestoresBeforeCloseAndRepeatsCleanup(t *testing.T) {
	_, normal := newPgPoolWireFixture(t, nil, nil)
	_, maintenance := newPgPoolWireFixture(t, nil, nil)
	priors := [2]*pgxpool.Pool{normal.open(), maintenance.open()}
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	constructorCalls := 0
	scope, err := newTestPgQueryScope(t.Context(), &queryScopeTestTracer{}, [2]*safePgPool{normal, maintenance}, func(ctx context.Context, config *pgxpool.Config) (*pgxpool.Pool, error) {
		constructorCalls++
		if constructorCalls == 1 {
			config.BeforeClose = func(*pgx.Conn) {
				if normal.open() != priors[0] || maintenance.open() != priors[1] {
					t.Error("clone close preceded complete identity restoration")
				}
				close(entered)
				<-release
			}
		}
		return pgxpool.NewWithConfig(ctx, config)
	}, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		releaseOnce.Do(func() { close(release) })
		if err := scope.Close(); err != nil {
			t.Error(err)
		}
	}()
	conn, err := normal.open().Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	conn.Release()
	first := make(chan struct{})
	var second chan struct{}
	var firstErr, secondErr error
	defer func() {
		releaseOnce.Do(func() { close(release) })
		queryScopeTestJoin(t, first)
		if second != nil {
			queryScopeTestJoin(t, second)
		}
	}()
	go func() { defer close(first); firstErr = scope.Close() }()
	queryScopeTestJoin(t, entered)
	second = make(chan struct{})
	go func() { defer close(second); secondErr = scope.Close() }()
	releaseOnce.Do(func() { close(release) })
	queryScopeTestJoin(t, first)
	queryScopeTestJoin(t, second)
	if firstErr != nil || secondErr != nil || scope.Close() != nil {
		t.Fatal("repeat close lost idempotent joined cleanup")
	}
}

// Stale pool configuration is rejected even when resource-name syntax is valid.
func TestPgQueryScopeRejectsStalePoolDatabase(t *testing.T) {
	_, normal := newPgPoolWireFixture(t, nil, nil)
	_, maintenance := newPgPoolWireFixture(t, nil, nil)
	priorNormal, priorMaintenance := normal.open(), maintenance.open()
	constructed := false
	_, err := newTestPgQueryScope(t.Context(), &queryScopeTestTracer{}, [2]*safePgPool{normal, maintenance}, func(ctx context.Context, config *pgxpool.Config) (*pgxpool.Pool, error) {
		constructed = true
		return pgxpool.NewWithConfig(ctx, config)
	}, "test_1_00000000000000000000000000000001")
	if err == nil || constructed || normal.open() != priorNormal || maintenance.open() != priorMaintenance {
		t.Fatal("stale application database escaped the exact fixture guard")
	}
}

// Fatal's Goexit mechanism must run in-frame cleanup before its owner returns.
func TestPgQueryScopeCallbackGoexitRestoresPools(t *testing.T) {
	_, normal := newPgPoolWireFixture(t, nil, nil)
	_, maintenance := newPgPoolWireFixture(t, nil, nil)
	priors := [2]*pgxpool.Pool{normal.open(), maintenance.open()}
	done := make(chan struct{})
	var scope *TestPgQueryScope
	var createErr, closeErr error
	defer queryScopeTestJoin(t, done)
	go func() {
		defer close(done)
		scope, createErr = newTestPgQueryScope(t.Context(), &queryScopeTestTracer{}, [2]*safePgPool{normal, maintenance}, pgxpool.NewWithConfig, "synthetic")
		if createErr != nil {
			return
		}
		defer func() { closeErr = scope.Close() }()
		runtime.Goexit()
	}()
	queryScopeTestJoin(t, done)
	if createErr != nil || closeErr != nil || scope == nil || normal.open() != priors[0] || maintenance.open() != priors[1] {
		t.Fatal("callback Goexit did not restore both pools before join")
	}
	for _, owner := range scope.poolOwners {
		queryScopeTestClosed(t, owner.traced)
	}
}

// External restoration and replacement remain explicit failures, not silent success.
func TestPgQueryScopeReportsIdentityDrift(t *testing.T) {
	for _, restorePrior := range []bool{false, true} {
		func() {
			_, normal := newPgPoolWireFixture(t, nil, nil)
			_, maintenance := newPgPoolWireFixture(t, nil, nil)
			prior := normal.open()
			scope, err := newTestPgQueryScope(t.Context(), &queryScopeTestTracer{}, [2]*safePgPool{normal, maintenance}, pgxpool.NewWithConfig, "synthetic")
			if err != nil {
				t.Fatal(err)
			}
			defer scope.Close()
			current := prior
			if !restorePrior {
				current, err = pgxpool.NewWithConfig(t.Context(), prior.Config())
				if err != nil {
					t.Fatal(err)
				}
			}
			normal.mutex.Lock()
			normal.pool = current
			normal.mutex.Unlock()
			if scope.Close() == nil || scope.Close() == nil || normal.open() != current {
				t.Fatal("pool identity drift was erased or overwritten")
			}
			queryScopeTestClosed(t, scope.poolOwners[0].traced)
			if !restorePrior {
				queryScopeTestClosed(t, prior)
			}
		}()
	}
}
