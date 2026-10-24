// Pins the transaction_timeout exemption's ordering and failure handling, on a
// fake session and on the pgx wire protocol, and what `Db` and `Tx` do with a
// session the bound ended (25P04). No database is contacted; PostgreSQL's own
// behavior is pinned by db_transaction_timeout_db_test.go.
package server

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A session that records what the exemption does to it. Not safe for
// concurrent use.
type fakeTimeoutSession struct {
	isDirect bool
	isIdle   bool
	isExempt bool
	// the error each statement returns
	execErrs map[string]error
	// statements, callback marks and closes, in order
	events []string
	// the context each statement ran on
	execCtxs []context.Context
	closed   bool
}

func newFakeTimeoutSession() *fakeTimeoutSession {
	return &fakeTimeoutSession{isDirect: true, isIdle: true, execErrs: map[string]error{}}
}

func (self *fakeTimeoutSession) direct() bool { return self.isDirect }
func (self *fakeTimeoutSession) idle() bool   { return self.isIdle && !self.closed }
func (self *fakeTimeoutSession) exempt() bool { return self.isExempt }

func (self *fakeTimeoutSession) setExempt(exempt bool) {
	self.isExempt = exempt
}

func (self *fakeTimeoutSession) exec(ctx context.Context, sql string) error {
	self.events = append(self.events, sql)
	self.execCtxs = append(self.execCtxs, ctx)
	if err := ctx.Err(); err != nil {
		return err
	}
	return self.execErrs[sql]
}

func (self *fakeTimeoutSession) close(ctx context.Context) {
	self.events = append(self.events, "close")
	self.closed = true
}

func requireTimeoutEvents(t *testing.T, session *fakeTimeoutSession, want ...string) {
	t.Helper()
	if !slices.Equal(session.events, want) {
		t.Fatalf("events = %q, want %q", session.events, want)
	}
}

// The bound is lifted around the callback only, and restored on the same
// session, which stays open for its pool.
func TestWithoutTransactionTimeoutRestoresAfterCallback(t *testing.T) {
	session := newFakeTimeoutSession()
	withoutTransactionTimeout(context.Background(), session, func() {
		if !session.isExempt {
			t.Error("callback ran without the exemption mark")
		}
		session.events = append(session.events, "callback")
	})
	requireTimeoutEvents(t, session, "SET transaction_timeout = 0", "callback", "RESET transaction_timeout")
	if session.isExempt || session.closed {
		t.Fatalf("after the callback exempt=%t closed=%t, want both false", session.isExempt, session.closed)
	}
}

// A panicking callback still restores its session, and its panic reaches the
// caller unchanged.
func TestWithoutTransactionTimeoutRestoresAfterPanic(t *testing.T) {
	session := newFakeTimeoutSession()
	callbackErr := errors.New("synthetic callback failure")
	recovered := captureDbErrorPanic(func() {
		withoutTransactionTimeout(context.Background(), session, func() {
			session.events = append(session.events, "callback")
			panic(callbackErr)
		})
	})
	if recovered != callbackErr {
		t.Fatalf("recovered %v, want the callback's own panic", recovered)
	}
	requireTimeoutEvents(t, session, "SET transaction_timeout = 0", "callback", "RESET transaction_timeout")
	if session.isExempt || session.closed {
		t.Fatalf("after the panic exempt=%t closed=%t, want both false", session.isExempt, session.closed)
	}
}

// A caller that stops during the callback still gets its session restored: the
// RESET runs on a live context with its own bound.
func TestWithoutTransactionTimeoutRestoresOnDetachedContext(t *testing.T) {
	session := newFakeTimeoutSession()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	withoutTransactionTimeout(ctx, session, func() {
		cancel()
	})
	requireTimeoutEvents(t, session, "SET transaction_timeout = 0", "RESET transaction_timeout")
	restoreCtx := session.execCtxs[1]
	deadline, bounded := restoreCtx.Deadline()
	if !bounded || pgTransactionTimeoutRestoreTimeout < time.Until(deadline) {
		t.Fatalf("restore deadline bounded=%t in %s, want within %s", bounded, time.Until(deadline), pgTransactionTimeoutRestoreTimeout)
	}
	if session.closed {
		t.Fatal("a restored session was closed")
	}
}

// A session whose RESET fails still carries the exemption, so it is closed
// rather than returned to its pool.
func TestWithoutTransactionTimeoutClosesSessionWhenResetFails(t *testing.T) {
	session := newFakeTimeoutSession()
	session.execErrs["RESET transaction_timeout"] = errors.New("synthetic broken connection")
	withoutTransactionTimeout(context.Background(), session, func() {})
	requireTimeoutEvents(t, session, "SET transaction_timeout = 0", "RESET transaction_timeout", "close")
}

// A callback that leaves a transaction open is not restored inside it: the
// RESET could not re-arm that transaction's timer, and a rollback would undo
// it. The session is closed instead.
func TestWithoutTransactionTimeoutClosesSessionLeftInTransaction(t *testing.T) {
	session := newFakeTimeoutSession()
	withoutTransactionTimeout(context.Background(), session, func() {
		session.isIdle = false
	})
	requireTimeoutEvents(t, session, "SET transaction_timeout = 0", "close")
}

// A session of the main pool goes through the transaction pooler, where a
// session SET would reach other clients: nothing runs on it.
func TestWithoutTransactionTimeoutRefusesPooledSession(t *testing.T) {
	session := newFakeTimeoutSession()
	session.isDirect = false
	called := false
	recovered := captureDbErrorPanic(func() {
		withoutTransactionTimeout(context.Background(), session, func() { called = true })
	})
	if recovered != errTransactionTimeoutPooledSession || called || len(session.events) != 0 || session.isExempt {
		t.Fatalf("recovered=%v called=%t events=%q exempt=%t, want a refusal before any statement", recovered, called, session.events, session.isExempt)
	}
}

// Inside a transaction the exemption could not be bounded to the callback,
// so it is refused before any statement.
func TestWithoutTransactionTimeoutRefusesSessionInTransaction(t *testing.T) {
	session := newFakeTimeoutSession()
	session.isIdle = false
	called := false
	recovered := captureDbErrorPanic(func() {
		withoutTransactionTimeout(context.Background(), session, func() { called = true })
	})
	if recovered != errTransactionTimeoutInTransaction || called || len(session.events) != 0 || session.isExempt {
		t.Fatalf("recovered=%v called=%t events=%q exempt=%t, want a refusal before any statement", recovered, called, session.events, session.isExempt)
	}
}

// A nested call keeps the outer exemption: its own end does not restore the
// bound under the rest of the outer callback.
func TestWithoutTransactionTimeoutNestedCallKeepsOuterExemption(t *testing.T) {
	session := newFakeTimeoutSession()
	withoutTransactionTimeout(context.Background(), session, func() {
		withoutTransactionTimeout(context.Background(), session, func() {
			session.events = append(session.events, "inner")
		})
		if !session.isExempt {
			t.Error("the inner call ended the outer exemption")
		}
		session.events = append(session.events, "outer")
	})
	requireTimeoutEvents(t, session, "SET transaction_timeout = 0", "inner", "outer", "RESET transaction_timeout")
}

// A failed SET skips the callback and surfaces its own error; the session is
// still restored.
func TestWithoutTransactionTimeoutSetFailureSkipsCallback(t *testing.T) {
	session := newFakeTimeoutSession()
	setErr := errors.New("synthetic set failure")
	session.execErrs["SET transaction_timeout = 0"] = setErr
	called := false
	recovered := captureDbErrorPanic(func() {
		withoutTransactionTimeout(context.Background(), session, func() { called = true })
	})
	if recovered != setErr || called {
		t.Fatalf("recovered=%v called=%t, want the SET error without the callback", recovered, called)
	}
	requireTimeoutEvents(t, session, "SET transaction_timeout = 0", "RESET transaction_timeout")
}

// A wire pool like newPgPoolWireFixture's, configured in production order:
// the direct-session mark wraps the liveness AfterConnect.
func newTransactionTimeoutWireFixture(t testing.TB, direct bool, queryError func(int, string) *pgproto3.ErrorResponse) (*pgPoolWireFixture, *safePgPool) {
	t.Helper()
	fixture := &pgPoolWireFixture{queryError: queryError}
	config, err := pgxpool.ParseConfig("host=synthetic-pg.example user=synthetic dbname=synthetic sslmode=disable connect_timeout=5")
	if err != nil {
		t.Fatal(err)
	}
	config.MinConns, config.MaxConns = 0, 1
	config.ConnConfig.DialFunc = fixture.dial
	config.ConnConfig.LookupFunc = func(ctx context.Context, host string) ([]string, error) {
		return []string{"192.0.2.1"}, nil
	}
	config.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	startupMetrics := configurePgPoolLiveness(config)
	configurePgPoolWriteTracking(config)
	configurePgPoolStatementErrors(config)
	configurePgPoolDirectSession(config, direct)
	config.ShouldPing = neverPingTestPool
	pgPool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	pool := &safePgPool{pool: pgPool, startupMetrics: startupMetrics}
	t.Cleanup(func() {
		fixture.stateLock.Lock()
		fixture.closed = true
		connections := fixture.connections
		fixture.stateLock.Unlock()
		for _, conn := range connections {
			_ = conn.Close()
		}
		pool.close()
		fixture.workers.Wait()
	})
	return fixture, pool
}

// The statements the fixture's sessions received after their startup pings.
func transactionTimeoutWireQueries(fixture *pgPoolWireFixture) ([]string, int) {
	fixture.stateLock.Lock()
	defer fixture.stateLock.Unlock()
	queries := []string{}
	for _, query := range fixture.queries {
		if query != "-- ping" {
			queries = append(queries, query)
		}
	}
	return queries, fixture.dialCount
}

// Only a pool configured as direct marks its sessions.
func TestConfigurePgPoolDirectSessionMarksSessions(t *testing.T) {
	for _, direct := range []bool{true, false} {
		_, pool := newTransactionTimeoutWireFixture(t, direct, nil)
		marked := false
		dbWithPool(context.Background(), pool, func(conn PgConn) {
			marked, _ = conn.Conn().PgConn().CustomData()[pgDirectSessionKey].(bool)
		}, OptNoRetry())
		if marked != direct {
			t.Errorf("direct=%t pool marked its session %t", direct, marked)
		}
	}
}

// The mark follows the liveness hook, so a failed startup Ping still fails
// the connection before any session is marked.
func TestConfigurePgPoolDirectSessionKeepsStartupFailure(t *testing.T) {
	config, err := pgxpool.ParseConfig("host=synthetic-pg.example user=synthetic dbname=synthetic sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	startupErr := errors.New("synthetic startup failure")
	config.AfterConnect = func(context.Context, *pgx.Conn) error { return startupErr }
	configurePgPoolDirectSession(config, true)
	if err := config.AfterConnect(context.Background(), nil); err != startupErr {
		t.Fatalf("AfterConnect = %v, want the liveness failure", err)
	}
}

// On a real pgx session the SET, the callback's statement and the RESET run in
// order on one connection, which the pool then reuses.
func TestWithoutTransactionTimeoutWireRestoresPooledSession(t *testing.T) {
	fixture, pool := newTransactionTimeoutWireFixture(t, true, nil)
	ctx := context.Background()
	dbWithPool(ctx, pool, func(conn PgConn) {
		WithoutTransactionTimeout(ctx, conn, func() {
			RaisePgResult(conn.Exec(ctx, "SELECT 1"))
		})
	}, OptNoRetry())
	dbWithPool(ctx, pool, func(conn PgConn) {
		RaisePgResult(conn.Exec(ctx, "SELECT 2"))
	}, OptNoRetry())
	queries, dials := transactionTimeoutWireQueries(fixture)
	want := []string{"SET transaction_timeout = 0", "SELECT 1", "RESET transaction_timeout", "SELECT 2"}
	if !slices.Equal(queries, want) || dials != 1 {
		t.Fatalf("queries=%q dials=%d, want %q on one connection", queries, dials, want)
	}
}

// A session whose RESET fails is destroyed: the next callback dials a new one.
func TestWithoutTransactionTimeoutWireDestroysUnrestoredSession(t *testing.T) {
	fixture, pool := newTransactionTimeoutWireFixture(t, true, func(connectionIndex int, query string) *pgproto3.ErrorResponse {
		if query == "RESET transaction_timeout" {
			return &pgproto3.ErrorResponse{Severity: "ERROR", Code: "XX000", Message: "synthetic reset failure"}
		}
		return nil
	})
	ctx := context.Background()
	dbWithPool(ctx, pool, func(conn PgConn) {
		WithoutTransactionTimeout(ctx, conn, func() {})
		if !conn.Conn().IsClosed() {
			t.Error("an unrestored session stayed open")
		}
	}, OptNoRetry())
	dbWithPool(ctx, pool, func(conn PgConn) {
		RaisePgResult(conn.Exec(ctx, "SELECT 1"))
	}, OptNoRetry())
	if _, dials := transactionTimeoutWireQueries(fixture); dials != 2 {
		t.Fatalf("dials=%d, want the unrestored session replaced", dials)
	}
}

// A callback that leaves its transaction open gets no RESET inside it; the
// session is destroyed instead.
func TestWithoutTransactionTimeoutWireDestroysSessionLeftInTransaction(t *testing.T) {
	fixture, pool := newTransactionTimeoutWireFixture(t, true, nil)
	ctx := context.Background()
	dbWithPool(ctx, pool, func(conn PgConn) {
		WithoutTransactionTimeout(ctx, conn, func() {
			RaisePgResult(conn.Exec(ctx, "begin"))
		})
	}, OptNoRetry())
	dbWithPool(ctx, pool, func(conn PgConn) {
		RaisePgResult(conn.Exec(ctx, "SELECT 1"))
	}, OptNoRetry())
	queries, dials := transactionTimeoutWireQueries(fixture)
	want := []string{"SET transaction_timeout = 0", "begin", "SELECT 1"}
	if !slices.Equal(queries, want) || dials != 2 {
		t.Fatalf("queries=%q dials=%d, want %q with the open-transaction session replaced", queries, dials, want)
	}
}

// A main pool session is refused before any statement reaches the pooler.
func TestWithoutTransactionTimeoutWireRefusesPooledSession(t *testing.T) {
	fixture, pool := newTransactionTimeoutWireFixture(t, false, nil)
	ctx := context.Background()
	recovered := captureDbErrorPanic(func() {
		dbWithPool(ctx, pool, func(conn PgConn) {
			WithoutTransactionTimeout(ctx, conn, func() {
				RaisePgResult(conn.Exec(ctx, "SELECT 1"))
			})
		}, OptNoRetry())
	})
	queries, _ := transactionTimeoutWireQueries(fixture)
	if recovered != errTransactionTimeoutPooledSession || len(queries) != 0 {
		t.Fatalf("recovered=%v queries=%q, want a refusal before any statement", recovered, queries)
	}
}

// PostgreSQL ends a session whose transaction runs past transaction_timeout
// with this error, then closes it.
func syntheticTransactionTimeoutError() *pgproto3.ErrorResponse {
	return &pgproto3.ErrorResponse{
		Severity:            "FATAL",
		SeverityUnlocalized: "FATAL",
		Code:                pgTransactionTimeoutCode,
		Message:             "terminating connection due to transaction timeout",
	}
}

// The bound's failure as the caller sees it, and how many attempts ran.
func requireTransactionTimeoutFailure(t *testing.T, recovered any, attempts int) {
	t.Helper()
	err, ok := recovered.(error)
	var pgErr *pgconn.PgError
	if !ok || !errors.As(err, &pgErr) || pgErr.Code != pgTransactionTimeoutCode {
		t.Fatalf("recovered %v, want the 25P04 cause", recovered)
	}
	if attempts != 1 {
		t.Fatalf("attempts=%d, want one: the bound ended the work and a rerun meets it again", attempts)
	}
}

// A transaction the bound ends fails at once with its cause under the
// default retry options, and its dead session is replaced.
func TestTransactionTimeoutWireStatementFailsFastWithoutRerun(t *testing.T) {
	fixture, pool := newTransactionTimeoutWireFixture(t, false, func(connectionIndex int, query string) *pgproto3.ErrorResponse {
		if query == "SELECT pg_sleep(1)" {
			return syntheticTransactionTimeoutError()
		}
		return nil
	})
	ctx := context.Background()
	attempts := 0
	recovered := captureDbErrorPanic(func() {
		txWithPool(ctx, pool, func(tx PgTx) {
			attempts += 1
			RaisePgResult(tx.Exec(ctx, "SELECT pg_sleep(1)"))
		})
	})
	requireTransactionTimeoutFailure(t, recovered, attempts)
	dbWithPool(ctx, pool, func(conn PgConn) {
		RaisePgResult(conn.Exec(ctx, "SELECT 1"))
	}, OptNoRetry())
	if _, dials := transactionTimeoutWireQueries(fixture); dials != 2 {
		t.Fatalf("dials=%d, want the ended session replaced", dials)
	}
}

// The bound can end a transaction at its commit; that commit did not happen,
// and it is not rerun either.
func TestTransactionTimeoutWireCommitFailsFastWithoutRerun(t *testing.T) {
	_, pool := newTransactionTimeoutWireFixture(t, false, func(connectionIndex int, query string) *pgproto3.ErrorResponse {
		if query == "commit" {
			return syntheticTransactionTimeoutError()
		}
		return nil
	})
	ctx := context.Background()
	attempts := 0
	recovered := captureDbErrorPanic(func() {
		txWithPool(ctx, pool, func(tx PgTx) {
			attempts += 1
			RaisePgResult(tx.Exec(ctx, "SELECT 1"))
		})
	})
	requireTransactionTimeoutFailure(t, recovered, attempts)
}

// A single autocommit statement runs in an implicit transaction the bound
// also covers.
func TestTransactionTimeoutWireAutocommitStatementFailsFastWithoutRerun(t *testing.T) {
	fixture, pool := newTransactionTimeoutWireFixture(t, true, func(connectionIndex int, query string) *pgproto3.ErrorResponse {
		if query == "SELECT pg_sleep(1)" {
			return syntheticTransactionTimeoutError()
		}
		return nil
	})
	ctx := context.Background()
	attempts := 0
	recovered := captureDbErrorPanic(func() {
		dbWithPool(ctx, pool, func(conn PgConn) {
			attempts += 1
			RaisePgResult(conn.Exec(ctx, "SELECT pg_sleep(1)"))
		})
	})
	requireTransactionTimeoutFailure(t, recovered, attempts)
	dbWithPool(ctx, pool, func(conn PgConn) {
		RaisePgResult(conn.Exec(ctx, "SELECT 1"))
	}, OptNoRetry())
	if _, dials := transactionTimeoutWireQueries(fixture); dials != 2 {
		t.Fatalf("dials=%d, want the ended session replaced", dials)
	}
}

// The taxonomy never reruns a session the bound ended, at a statement or at
// commit, and does not take it for a lost connection that could be retried.
func TestPgRetryTaxonomyNeverRerunsTransactionTimeout(t *testing.T) {
	for _, err := range []error{
		&pgconn.PgError{Severity: "FATAL", Code: pgTransactionTimeoutCode},
		&txAbortedError{statementErr: &pgconn.PgError{Severity: "FATAL", Code: pgTransactionTimeoutCode}, err: pgx.ErrTxCommitRollback},
	} {
		if class := pgRetryClassOf(err); class != pgRetryNever {
			t.Errorf("%v class=%d, want never", err, class)
		}
		if (&callbackRetryEvidence{}).CanRerun(err) {
			t.Errorf("%v can rerun", err)
		}
		if canRetryCommitError(err) {
			t.Errorf("%v retries its commit", err)
		}
		if isConnectionError(err) {
			t.Errorf("%v is classified as a retryable connection error", err)
		}
	}
}

// A statement recorder standing in for a transaction.
type recordingTransactionTimeoutTx struct {
	statements []string
}

func (self *recordingTransactionTimeoutTx) Exec(_ context.Context, sql string, _ ...any) (PgTag, error) {
	self.statements = append(self.statements, sql)
	return PgTag{}, nil
}

// Zero or less lifts the bound; a positive bound disarms the running timer and
// arms a new one in whole milliseconds, at least one.
func TestSetTransactionTimeoutInTxStatements(t *testing.T) {
	for _, c := range []struct {
		bound time.Duration
		want  string
	}{
		{bound: 0, want: "SET LOCAL transaction_timeout = 0"},
		{bound: -time.Second, want: "SET LOCAL transaction_timeout = 0"},
		{bound: 2 * time.Hour, want: "SET LOCAL transaction_timeout = 0; SET LOCAL transaction_timeout = 7200000"},
		{bound: 1500 * time.Microsecond, want: "SET LOCAL transaction_timeout = 0; SET LOCAL transaction_timeout = 1"},
		{bound: time.Microsecond, want: "SET LOCAL transaction_timeout = 0; SET LOCAL transaction_timeout = 1"},
	} {
		recorder := &recordingTransactionTimeoutTx{}
		SetTransactionTimeoutInTx(context.Background(), recorder, c.bound)
		if len(recorder.statements) != 1 || recorder.statements[0] != c.want {
			t.Errorf("bound %s: statements %q, want %q", c.bound, recorder.statements, c.want)
		}
	}
	if liftTransactionTimeoutInTxSql != "SET LOCAL transaction_timeout = 0" {
		t.Fatal("the lift statement changed")
	}
}

// The monitor names the unpublished migration by its id, so its version is
// found wherever a merge places it.
func TestCodeMigrationVersionFindsTheTransactionTimeoutMigration(t *testing.T) {
	version := CodeMigrationVersion(ApplicationTransactionTimeoutMigrationId)
	if version < 1 || len(migrations) < version {
		t.Fatalf("version %d of %d migrations", version, len(migrations))
	}
	migration, ok := migrations[version-1].(*CodeMigration)
	if !ok || migration.id != ApplicationTransactionTimeoutMigrationId {
		t.Fatalf("version %d is %T, not the transaction timeout migration", version, migrations[version-1])
	}
	if CodeMigrationVersion("synthetic_missing_code_migration") != 0 {
		t.Fatal("a missing id found a version")
	}
}
