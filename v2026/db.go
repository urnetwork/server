package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"runtime"
	"runtime/debug"
	// "strconv"
	mathrand "math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	// "github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/urnetwork/glog/v2026"
)

/*
uses the shared transaction pool across all services:
- `Db` runs in raw connection mode (no transaction)
- `Tx` runs in read-write mode by default, which can be changed with `pgx.TxOptions`
uses a private connection pool local to the current service:
- `MaintenanceDb`
- `MaintenanceTx`
*/

// note all times in the db should be `timestamp` UTC. Do not use `timestamp with time zone`. See `NowUtc`

var DbContextDoneError = errors.New("Done")

// Bounds PostgreSQL startup per resolved address. Pool constructors outlive a
// canceled Acquire, so a backend outage must not retain each attempt for the
// much longer request or retry horizon.
const PgConnectTimeout = 5 * time.Second

// Bounds validation of an established PostgreSQL connection. Protocol reads
// need a shorter budget than dialing so a stale socket cannot consume the
// caller's remaining work horizon.
const PgPingTimeout = 5 * time.Second

// Bounds best-effort disposal of an unusable PostgreSQL connection.
const PgCloseTimeout = 5 * time.Second

// Matches pgxpool's normal destructor budget. Failed AfterConnect hooks do not
// run that destructor, so they must join pgx's asynchronous socket cleanup
// before returning the constructor reservation to the pool.
const PgStartupCleanupTimeout = 15 * time.Second

// PgCommitTimeout bounds the commit round trip, which runs on a context
// detached from the caller (see the commit in `txWithPool`).
const PgCommitTimeout = 30 * time.Second

// PgRollbackTimeout bounds transaction cleanup after the caller's context is
// canceled. Rollback is best-effort when another error already owns the
// outcome, but it still needs a live context so PostgreSQL can release the
// transaction promptly instead of waiting for a broken connection to close.
const PgRollbackTimeout = 30 * time.Second

// type aliases to simplify user code
type PgConn = *pgxpool.Conn
type PgTx = pgx.Tx
type PgResult = pgx.Rows
type PgTag = pgconn.CommandTag
type PgNamedArgs = pgx.NamedArgs
type PgBatch = *pgx.Batch
type PgBatchResults = pgx.BatchResults

// PgCanQuery is satisfied by PgConn, PgTx, and any other value that can execute queries.
// It lets helper functions accept either a raw connection or a transaction uniformly.
type PgCanQuery interface {
	Query(ctx context.Context, sql string, args ...any) (PgResult, error)
}

// TxReadBeforeBegin performs an authoritative read on the transaction's owned
// connection before the transaction takes its first snapshot. It runs for every
// attempt, including a connection retry, and must not write or publish effects.
// This avoids a second pool acquisition without carrying an earlier snapshot
// into a transaction that must observe a later credential or ownership change.
type TxReadBeforeBegin func(PgCanQuery)

const TxSerializable = pgx.Serializable
const TxRepeatableRead = pgx.RepeatableRead
const TxReadCommitted = pgx.ReadCommitted

var safePool = &safePgPool{
	ctx:                context.Background(),
	vaultResourceName:  DefaultPgVaultResourceName,
	configResourceName: DefaultPgConfigResourceName,
}

// func pool() *pgxpool.Pool {
// 	return safePool.open()
// }

var safeMaintenancePool = &safePgPool{
	ctx:                context.Background(),
	vaultResourceName:  MaintenancePgVaultResourceName,
	configResourceName: MaintenancePgConfigResourceName,
}

// func maintenancePool() *pgxpool.Pool {
// 	return safeMaintenancePool.open()
// }

// resets the connection pool
// call this after changes to the env
func PgReset() {
	safePool.reset()
	safeMaintenancePool.reset()
}

const DefaultPgVaultResourceName = "pg.yml"
const DefaultPgConfigResourceName = "db.yml"

const MaintenancePgVaultResourceName = "pg_maintenance.yml"
const MaintenancePgConfigResourceName = "db_maintenance.yml"

type safePgPool struct {
	vaultResourceName  string
	configResourceName string
	ctx                context.Context
	mutex              sync.Mutex
	pool               *pgxpool.Pool
	lifecycleLock      sync.Mutex
	lifecycle          *pgPoolWrapperLifecycle
	lifecycleDropped   atomic.Uint64
}

// resolveResources returns the connection (vault) and pool-sizing (config)
// resources for this pool. It honors the pool's OWN resource names — so the
// maintenance pool uses pg_maintenance.yml (direct Postgres, bypassing
// PgBouncer) and db_maintenance.yml (its own max_connections) — and falls back
// to the defaults (pg.yml / db.yml) when the pool-specific resource is absent
// or does not define the size. The test harness redirects both pool resources
// when a production-shaped profile supplies pg_maintenance.yml, and the
// fallback still tolerates that resource being absent. The main pool's names
// ARE the defaults, so its resolution is unchanged.
func (self *safePgPool) resolveResources() (vaultKeys *SimpleResource, configKeys *SimpleResource) {
	vaultKeys = Vault.RequireSimpleResource(DefaultPgVaultResourceName)
	if self.vaultResourceName != DefaultPgVaultResourceName {
		if r, err := Vault.SimpleResource(self.vaultResourceName); err == nil {
			vaultKeys = r
		}
	}
	configKeys = Config.RequireSimpleResource(DefaultPgConfigResourceName)
	if self.configResourceName != DefaultPgConfigResourceName {
		// use the pool-specific config only if it actually defines the size, so
		// an empty/partial db_maintenance.yml falls back to db.yml
		if r, err := Config.SimpleResource(self.configResourceName); err == nil && 0 < len(r.Int("max_connections")) {
			configKeys = r
		}
	}
	return
}

func (self *safePgPool) open() *pgxpool.Pool {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if self.pool == nil {
		// Logger().Printf("Db init\n")

		dbKeys, dbConfigKeys := self.resolveResources()

		minConnections := dbConfigKeys.RequireInt("min_connections")
		maxConnections := dbConfigKeys.RequireInt("max_connections")
		healthCheckPeriod := "1s"
		connectionMaxLifetime := "8h"
		connectionMaxLifetimeJitter := "15m"
		connectionMaxIdleTime := "60m"
		if healthCheckPeriods := dbConfigKeys.String("health_check_period"); 0 < len(healthCheckPeriods) {
			healthCheckPeriod = healthCheckPeriods[0]
		}
		if connectionMaxLifetimes := dbConfigKeys.String("conn_max_lifetime"); 0 < len(connectionMaxLifetimes) {
			connectionMaxLifetime = connectionMaxLifetimes[0]
		}
		if connectionMaxLifetimeJitters := dbConfigKeys.String("conn_max_lifetime_jitter"); 0 < len(connectionMaxLifetimeJitters) {
			connectionMaxLifetimeJitter = connectionMaxLifetimeJitters[0]
		}
		if connectionMaxIdleTimes := dbConfigKeys.String("conn_max_idle_time"); 0 < len(connectionMaxIdleTimes) {
			connectionMaxIdleTime = connectionMaxIdleTimes[0]
		}
		if service, err := Service(); err == nil && service != "" {
			if serviceMinConnections := dbConfigKeys.Int(service, "min_connections"); 0 < len(serviceMinConnections) {
				minConnections = serviceMinConnections[0]
			}
			if serviceMaxConnections := dbConfigKeys.Int(service, "max_connections"); 0 < len(serviceMaxConnections) {
				maxConnections = serviceMaxConnections[0]
			}
			if healthCheckPeriods := dbConfigKeys.String(service, "health_check_period"); 0 < len(healthCheckPeriods) {
				healthCheckPeriod = healthCheckPeriods[0]
			}
			if connectionMaxLifetimes := dbConfigKeys.String(service, "conn_max_lifetime"); 0 < len(connectionMaxLifetimes) {
				connectionMaxLifetime = connectionMaxLifetimes[0]
			}
			if connectionMaxLifetimeJitters := dbConfigKeys.String(service, "conn_max_lifetime_jitter"); 0 < len(connectionMaxLifetimeJitters) {
				connectionMaxLifetimeJitter = connectionMaxLifetimeJitters[0]
			}
			if connectionMaxIdleTimes := dbConfigKeys.String(service, "conn_max_idle_time"); 0 < len(connectionMaxIdleTimes) {
				connectionMaxIdleTime = connectionMaxIdleTimes[0]
			}
		}

		// see the Config struct for human understandable docs
		// https://github.com/jackc/pgx/blob/master/pgxpool/pool.go#L103
		// https://github.com/jackc/pgx/blob/master/pgconn/config.go#L445
		options := map[string]string{
			"sslmode":                       "disable",
			"connect_timeout":               fmt.Sprintf("%d", PgConnectTimeout/time.Second),
			"pool_max_conns":                fmt.Sprintf("%d", maxConnections),
			"pool_min_conns":                fmt.Sprintf("%d", minConnections),
			"pool_max_conn_lifetime":        connectionMaxLifetime,
			"pool_max_conn_lifetime_jitter": connectionMaxLifetimeJitter,
			"pool_max_conn_idle_time":       connectionMaxIdleTime,
			"pool_health_check_period":      healthCheckPeriod,
			// must use `Tx` to write, which sets `AccessMode: pgx.ReadWrite`
			// "default_transaction_read_only": "on",
			// "default_transaction_isolation": "read committed",
		}
		glog.Infof("[db]options = %s\n", options)
		optionsPairs := []string{}
		for key, value := range options {
			optionsPairs = append(optionsPairs, fmt.Sprintf("%s=%s", key, value))
		}
		optionsString := strings.Join(optionsPairs, "&")

		postgresUrl := fmt.Sprintf(
			"postgres://%s:%s@%s/%s?%s",
			dbKeys.RequireString("user"),
			localEvaluationCredential("EVALUATION_DB_PASSWORD", dbKeys.RequireString("password")),
			dbKeys.RequireString("authority"),
			dbKeys.RequireString("db"),
			optionsString,
		)
		// Logger().Printf("Db url %s\n", postgresUrl)
		config, err := pgxpool.ParseConfig(postgresUrl)
		if err != nil {
			panic(fmt.Sprintf("Unable to parse url: %s", err))
		}
		glog.Infof("[db]statement_tag = %s\n", processPgStatementTag())
		configurePgPoolLiveness(config)
		configurePgPoolWriteTracking(config)
		configurePgPoolStatementErrors(config)

		self.pool, err = pgxpool.NewWithConfig(self.ctx, config)
		if err != nil {
			panic(fmt.Sprintf("Unable to connect to database: %s", err))
		}
	}
	return self.pool
}

// Validate a new socket before first use, then let pgx validate connections
// idle for more than a second. A hot checkout needs no second round trip to
// the transaction pooler; failed callback connections still follow disposal
// and safe-retry classification in dbWithPool.
func configurePgPoolLiveness(config *pgxpool.Config) {
	if config.ConnConfig.ConnectTimeout <= 0 || PgConnectTimeout < config.ConnConfig.ConnectTimeout {
		config.ConnConfig.ConnectTimeout = PgConnectTimeout
	}
	// pgconn resolves names before applying ConnectTimeout. The pool supplies
	// a detached constructor context, so give each lookup its own finite bound.
	lookup := config.ConnConfig.LookupFunc
	config.ConnConfig.LookupFunc = func(ctx context.Context, host string) ([]string, error) {
		lookupCtx, cancel := context.WithTimeout(ctx, PgConnectTimeout)
		defer cancel()
		return lookup(lookupCtx, host)
	}
	config.PingTimeout = PgPingTimeout
	config.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		pgxRegisterIdType(conn.TypeMap())
		if err := pingPgConnection(ctx, conn); err != nil {
			cleanupFailedPgStartup(ctx, conn.PgConn(), PgStartupCleanupTimeout)
			return err
		}
		return nil
	}
}

// A failed Ping can mark a PgConn closed before its cancel/Terminate cleanup
// has disposed of the socket. Close alone then returns immediately. Retain the
// failed constructor until CleanupDone, subject to the same finite budget used
// by pgxpool's ordinary destructor. Acquire cancellation must not skip cleanup.
func cleanupFailedPgStartup(ctx context.Context, conn interface {
	Close(context.Context) error
	CleanupDone() chan struct{}
}, timeout time.Duration) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	_ = conn.Close(cleanupCtx)
	select {
	case <-conn.CleanupDone():
	case <-cleanupCtx.Done():
	}
}

func (self *safePgPool) close() {
	self.reset()
}

func (self *safePgPool) reset() {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	if self.pool != nil {
		self.pool.Close()
		self.pool = nil
	}
}

type DbRetryOptions struct {
	// rerun the entire callback on commit error
	rerunOnCommitError     bool
	rerunOnConnectionError bool
	// this only works if the conflict, e.g. an ID, is changed on each run
	// the BY coding style will generate the id in the callback, so this is generally considered safe
	// a unique or foreign key violation that a rerun repeats exactly ends the
	// reruns, and permanent errors are never rerun (see db_retry_taxonomy.go)
	rerunOnTransientError bool
	retryMinTimeout       time.Duration
	retryMaxTimeout       time.Duration
	endRetryTimeout       time.Duration
	// debugRetryTimeout time.Duration
}

func (self *DbRetryOptions) Backoff() *retryBackoff {
	return &retryBackoff{
		retryMinTimeout: self.retryMinTimeout,
		retryMaxTimeout: self.retryMaxTimeout,
	}
}

// exponential backoff with jitter
// each call to `NextRetryTimeout` advances the attempt, growing the backoff window
// from `retryMinTimeout` up to `retryMaxTimeout`
type retryBackoff struct {
	retryMinTimeout time.Duration
	retryMaxTimeout time.Duration
	attempt         int
}

func (self *retryBackoff) NextRetryTimeout() time.Duration {
	// cap the shift so the backoff does not overflow
	backoff := self.retryMaxTimeout
	if t := self.retryMinTimeout << uint(min(self.attempt, 16)); t < self.retryMaxTimeout {
		backoff = t
	}
	self.attempt += 1
	if backoff <= self.retryMinTimeout {
		return self.retryMinTimeout
	}
	return self.retryMinTimeout + time.Duration(
		mathrand.Int63n(int64(backoff-self.retryMinTimeout)),
	)
}

// this is the default for `Db` and `Tx`
func OptRetryDefault() DbRetryOptions {
	return DbRetryOptions{
		rerunOnCommitError:     true,
		rerunOnConnectionError: true,
		rerunOnTransientError:  true,
		retryMinTimeout:        100 * time.Millisecond,
		retryMaxTimeout:        5 * time.Second,
		endRetryTimeout:        60 * time.Second,
		// debugRetryTimeout: 90 * time.Second,
	}
}

func OptNoRetry() DbRetryOptions {
	return DbRetryOptions{
		rerunOnCommitError:     false,
		rerunOnConnectionError: false,
		rerunOnTransientError:  false,
	}
}

type DbReadWriteOptions struct {
	readOnly bool
}

func OptReadOnly() DbReadWriteOptions {
	return DbReadWriteOptions{
		readOnly: true,
	}
}

func OptReadWrite() DbReadWriteOptions {
	return DbReadWriteOptions{
		readOnly: false,
	}
}

/*
type DbDebugOptions struct {
	txCommitSeparately bool
}

func OptNoDebug() DbDebugOptions {
	return DbDebugOptions{
		txCommitSeparately: false,
	}
}

// it can be hard to know which `Exec` has issues in a large transaction
// use this to separate the `Exec`
func OptDebugTx() DbDebugOptions {
	return DbDebugOptions{
		txCommitSeparately: true,
	}
}
*/

type PgRetry struct {
}

func (self *PgRetry) Error() string {
	return "retry"
}

// Whether a rerun of the whole callback can resolve the error: a
// serialization failure or deadlock, an explicit `PgRetry`, or a unique or
// foreign key violation, which a rerun resolves unless it repeats (see
// db_retry_taxonomy.go). Every other error, including the other integrity
// violations and every data exception, is permanent.
// https://www.postgresql.org/docs/current/mvcc-serialization-failure-handling.html
// https://www.postgresql.org/docs/current/errcodes-appendix.html
func isTransientError(err error) bool {
	return pgRetryClassOf(err) != pgRetryNever
}

func isConnectionError(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgerrcode.IsConnectionException(pgErr.Code)
	}
	// pgx wraps this sentinel while cleaning its statement cache after a
	// connection dies; retain the connection retry through those wrappers.
	if errors.Is(err, pgconn.ErrConnClosed) {
		return true
	}
	// pgx protocol errors wrap the underlying socket failure. In particular,
	// pgproto3.writeError wraps net.OpError, so inspect the complete chain before
	// deciding whether the current pooled connection can be reused.
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	// Retain compatibility with adapters that return only the legacy status.
	return err.Error() == "conn closed"
}

// Classifies the current failure only. A callback replay additionally requires
// transport proof that none of its statements wrote bytes; pgx can mask a lost
// read reply as a SafeToRetry closed-connection error.
func canRetryConnectionError(err error) bool {
	// Read-side pgx timeouts are normalized into its private timeout wrapper.
	// Preserve their established fresh-connection retry even though that wrapper
	// cannot prove that no query bytes were sent.
	if pgconn.Timeout(err) {
		return true
	}
	var netErr net.Error
	if !errors.As(err, &netErr) {
		return true
	}
	var safeToRetry interface {
		SafeToRetry() bool
	}
	if errors.As(err, &safeToRetry) {
		return safeToRetry.SafeToRetry()
	}
	return true
}

// Recognizes the raw socket timeout pgx can return from its write path after
// its context watcher has already canceled the operation. Both facts are
// required so an unrelated application error racing cancellation stays loud.
func isDoneContextConnectionError(ctx context.Context, err error) bool {
	if ctx.Err() == nil {
		return false
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// maintenance connection
func MaintenanceDb(ctx context.Context, callback func(PgConn), options ...any) {
	c := func() {
		dbWithPool(ctx, safeMaintenancePool, callback, options...)
	}
	if glog.V(2) {
		pc, filename, line, _ := runtime.Caller(1)
		pcName := runtime.FuncForPC(pc).Name()
		parts := strings.Split(filename, "/")
		Trace(
			fmt.Sprintf("[db] %s %s:%d\n", pcName, parts[len(parts)-1], line),
			c,
		)
	} else {
		c()
	}
}

// AcquireMaintenanceDbConn reserves one direct-postgres session until the
// caller invokes Release (or Hijack+Close on error). It is intentionally
// separate from MaintenanceDb's callback API for the rare case where
// session-scoped state must remain alive across application work, such as a
// PostgreSQL advisory lock guarding a task claim. The maintenance pool bypasses
// transaction-pooled PgBouncer, where session locks would not be safe.
func AcquireMaintenanceDbConn(ctx context.Context) (PgConn, error) {
	checkPostgresAllowed(ctx)
	conn, err := safeMaintenancePool.open().Acquire(ctx)
	if err != nil {
		return nil, err
	}
	if err := pingPgConnection(ctx, conn); err != nil {
		discardPgConnection(ctx, conn)
		return nil, err
	}
	return conn, nil
}

func Db(ctx context.Context, callback func(PgConn), options ...any) {
	c := func() {
		dbWithPool(ctx, safePool, callback, options...)
	}
	if glog.V(2) {
		pc, filename, line, _ := runtime.Caller(1)
		pcName := runtime.FuncForPC(pc).Name()
		parts := strings.Split(filename, "/")
		Trace(
			fmt.Sprintf("[db] %s %s:%d\n", pcName, parts[len(parts)-1], line),
			c,
		)
	} else {
		c()
	}
}

// ReplicaDb runs a read-only query that tolerates replication delay, such as
// stats and analytics reads. Queries tagged with this can be offloaded to a
// read replica when one is attached; until then it uses the primary pool,
// identical to `Db`. Do not tag reads that must observe the caller's own
// writes (read-after-write within a request).
func ReplicaDb(ctx context.Context, callback func(PgConn), options ...any) {
	c := func() {
		dbWithPool(ctx, safePool, callback, options...)
	}
	if glog.V(2) {
		pc, filename, line, _ := runtime.Caller(1)
		pcName := runtime.FuncForPC(pc).Name()
		parts := strings.Split(filename, "/")
		Trace(
			fmt.Sprintf("[db] %s %s:%d\n", pcName, parts[len(parts)-1], line),
			c,
		)
	} else {
		c()
	}
}

// Validates a pooled connection within a finite protocol budget. A socket can
// stall after dialing, so connect_timeout alone does not bound this round trip.
func pingPgConnection(ctx context.Context, conn interface {
	Ping(context.Context) error
}) error {
	pingCtx, pingCancel := context.WithTimeout(ctx, PgPingTimeout)
	defer pingCancel()
	return conn.Ping(pingCtx)
}

// Disposes a bad connection with a detached finite cleanup budget. Its error
// stays secondary to the Ping or callback error that proved the connection bad.
func closePgConnection(ctx context.Context, conn interface {
	Close(context.Context) error
}) {
	closeCtx, closeCancel := context.WithTimeout(
		context.WithoutCancel(ctx),
		PgCloseTimeout,
	)
	defer closeCancel()
	_ = conn.Close(closeCtx)
}

// Close while the pool still owns this borrowed resource, then let Release
// destroy the closed connection. pgxpool's bounded destructor joins CleanupDone
// before returning capacity; Hijack would release capacity before that cleanup.
// The original callback outcome need not wait for asynchronous disposal.
func discardPgConnection(ctx context.Context, conn PgConn) {
	closePgConnection(ctx, conn.Conn())
	conn.Release()
}

func dbWithPool(ctx context.Context, pool *safePgPool, callback func(PgConn), options ...any) {
	checkPostgresAllowed(ctx)
	retryOptions := OptRetryDefault()
	rwOptions := OptReadOnly()
	var timing *DbTiming
	phase := dbPhaseObservation(options)
	var readObservation *DbReadObservation
	var ownedConnection *pgOwnedConnection
	var readBeforeBegin TxReadBeforeBegin
	// debugOptions := OptNoDebug()
	for _, option := range options {
		switch v := option.(type) {
		case DbRetryOptions:
			retryOptions = v
		case DbReadWriteOptions:
			rwOptions = v
		case *DbTiming:
			timing = v
		case *DbReadObservation:
			readObservation = v
		case *pgOwnedConnection:
			ownedConnection = v
		case TxReadBeforeBegin:
			readBeforeBegin = v
			// case DbDebugOptions:
			// 	debugOptions = v
		}
	}
	if ownedConnection != nil {
		phase.enter(DbOperationSessionSetup)
		ownedConnection.run(ctx, callback, rwOptions.readOnly)
		return
	}

	retryEndTime := NowUtc().Add(retryOptions.endRetryTimeout)
	// retryDebugTime := NowUtc().Add(retryOptions.debugRetryTimeout)
	backoff := retryOptions.Backoff()
	retryEvidence := &callbackRetryEvidence{}
	for {
		var pgErr error
		connectionContextDone := false
		callbackStarted := false
		callbackWrites := pgWriteSnapshot{}
		connectionRetrySafe := false
		phase.enter(DbOperationAcquire)
		acquireStarted := timing.start()
		pgPool := pool.open()
		readObservation.BeginAcquire()
		conn, connErr := pgPool.Acquire(ctx)
		readObservation.FinishAcquire(connErr == nil)
		timing.finish(DbTimingAcquire, acquireStarted)
		if connErr != nil {
			if retryOptions.rerunOnConnectionError {
				waitStarted := timing.start()
				select {
				case <-ctx.Done():
					timing.finish(DbTimingRetryWait, waitStarted)
					panic(dbContextDoneCause(ctx, connErr))
				case <-time.After(backoff.NextRetryTimeout()):
					timing.finish(DbTimingRetryWait, waitStarted)
					if retryEndTime.Before(NowUtc()) {
						panic(connErr)
					}
					continue
				}
			}
			panic(connErr)
		}
		lifecycle := pool.observeBorrow(pgPool)
		physical := conn.Conn().PgConn()
		cleanup := physical.CleanupDone()

		func() {
			// Cleanup must observe the classification below. Register it first so
			// the recovery defer runs before it during panic unwinding.
			defer func() {
				needsCleanup := connErr != nil || physical.IsClosed() || physical.IsBusy() || physical.TxStatus() != 'I'
				lifecycle.beginRelease()
				if connErr != nil {
					discardPgConnection(ctx, conn)
					conn = nil
				} else {
					conn.Release()
				}
				lifecycle.finishRelease(cleanup, needsCleanup)
			}()
			defer func() {
				if err := recover(); err != nil {
					switch v := err.(type) {
					case error:
						if isTransientError(v) && retryOptions.rerunOnTransientError {
							pgErr = v
						} else if isConnectionError(v) {
							connErr = v
							connectionContextDone = isDoneContextConnectionError(ctx, v)
							connectionRetrySafe = !callbackStarted || callbackWrites.unchanged()
						} else {
							panic(v)
						}
					default:
						panic(v)
					}
				}
			}()
			// defer Logger().Printf("DB CLOSE\n")
			phase.enter(DbOperationSessionSetup)
			if !rwOptions.readOnly {
				// the default is read only, escalate to rw
				RaisePgResult(conn.Exec(ctx, "SET default_transaction_read_only=off"))
			}
			if readBeforeBegin != nil {
				// Only this explicitly read-only preparation precedes the
				// callback's write/replay boundary. A failed BEGIN can still
				// retry safely, and every new connection reloads the read.
				readBeforeBegin(conn)
			}
			callbackWrites = snapshotPgWrites(conn.Conn().PgConn().Conn())
			callbackStarted = true
			phase.enter(DbOperationCallback)
			callback(conn)
		}()

		if pgErr != nil {
			if retryOptions.rerunOnTransientError && retryEvidence.CanRerun(pgErr) {
				waitStarted := timing.start()
				select {
				case <-ctx.Done():
					timing.finish(DbTimingRetryWait, waitStarted)
					panic(dbContextDoneCause(ctx, pgErr))
				case <-time.After(backoff.NextRetryTimeout()):
					timing.finish(DbTimingRetryWait, waitStarted)
					if retryEndTime.Before(NowUtc()) {
						recordRerunDecision(pgErr, rerunDecisionEndedBudget)
						panic(pgErr)
					}
					if glog.V(2) {
						glog.Infof("[db]transient error, retry: %s\n", ErrorJson(pgErr, debug.Stack()))
					} else if glog.V(1) {
						glog.Infof("[db]transient error, retry = %v\n", pgErr)
					}
					continue
				}
			}
			panic(pgErr)
		}
		if connErr != nil {
			if connectionContextDone {
				panic(dbContextDoneCause(ctx, connErr))
			}
			if retryOptions.rerunOnConnectionError && connectionRetrySafe && canRetryConnectionError(connErr) {
				waitStarted := timing.start()
				select {
				case <-ctx.Done():
					timing.finish(DbTimingRetryWait, waitStarted)
					panic(dbContextDoneCause(ctx, connErr))
				case <-time.After(backoff.NextRetryTimeout()):
					timing.finish(DbTimingRetryWait, waitStarted)
					if retryEndTime.Before(NowUtc()) {
						panic(connErr)
					}
					continue
				}
			}
			panic(connErr)
		}

		return
	}
}

func MaintenanceTx(ctx context.Context, callback func(PgTx), options ...any) {
	c := func() {
		txWithPool(ctx, safeMaintenancePool, callback, options...)
	}
	if glog.V(2) {
		pc, filename, line, _ := runtime.Caller(1)
		pcName := runtime.FuncForPC(pc).Name()
		parts := strings.Split(filename, "/")
		Trace(
			fmt.Sprintf("[tx] %s %s:%d\n", pcName, parts[len(parts)-1], line),
			c,
		)
	} else {
		c()
	}
}

func Tx(ctx context.Context, callback func(PgTx), options ...any) {
	c := func() {
		txWithPool(ctx, safePool, callback, options...)
	}
	if glog.V(2) {
		pc, filename, line, _ := runtime.Caller(1)
		pcName := runtime.FuncForPC(pc).Name()
		parts := strings.Split(filename, "/")
		Trace(
			fmt.Sprintf("[tx] %s %s:%d\n", pcName, parts[len(parts)-1], line),
			c,
		)
	} else {
		c()
	}
}

// rollbackTx preserves the error or panic that caused cleanup. pgx marks a
// transaction closed after a rollback attempt even when that attempt returns
// an error; retrying Rollback from an outer recovery then yields ErrTxClosed
// and can replace the owning failure. Use a detached, bounded context and
// deliberately leave the rollback result secondary to the existing outcome.
func rollbackTx(ctx context.Context, tx PgTx) {
	rollbackCtx, rollbackCancel := context.WithTimeout(
		context.WithoutCancel(ctx),
		PgRollbackTimeout,
	)
	defer rollbackCancel()
	_ = tx.Rollback(rollbackCtx)
}

func txWithPool(ctx context.Context, pool *safePgPool, callback func(PgTx), options ...any) {
	txWithConnection(ctx, func(body func(PgConn)) {
		dbWithPool(ctx, pool, body, options...)
	}, callback, options...)
}

func txWithConnection(ctx context.Context, use func(func(PgConn)), callback func(PgTx), options ...any) {
	retryOptions := OptRetryDefault()
	var timing *DbTiming
	phase := dbPhaseObservation(options)
	// by default use RepeatableRead isolation
	// https://www.postgresql.org/docs/current/transaction-iso.html
	txOptions := pgx.TxOptions{
		IsoLevel:       pgx.RepeatableRead,
		AccessMode:     pgx.ReadWrite,
		DeferrableMode: pgx.NotDeferrable,
	}
	// debugOptions := OptNoDebug()
	for _, option := range options {
		switch v := option.(type) {
		case DbRetryOptions:
			retryOptions = v
		case *DbTiming:
			timing = v
		case pgx.TxOptions:
			txOptions = v
		case pgx.TxIsoLevel:
			txOptions.IsoLevel = v
		case pgx.TxAccessMode:
			txOptions.AccessMode = v
		case pgx.TxDeferrableMode:
			txOptions.DeferrableMode = v
			// case DbDebugOptions:
			// 	debugOptions = v
		}
	}

	retryEndTime := NowUtc().Add(retryOptions.endRetryTimeout)
	// retryDebugTime := NowUtc().Add(retryOptions.debugRetryTimeout)
	backoff := retryOptions.Backoff()
	retryEvidence := &callbackRetryEvidence{}
	for {
		var pgErr error
		var commitErr error
		var commitPosts []PostFunction
		var committedAt time.Time
		use(func(conn PgConn) {
			// an earlier use of the pooled connection is not evidence about
			// this attempt
			pgStatementErrorRecorderOf(conn.Conn().PgConn()).Reset()
			phase.enter(DbOperationBegin)
			beginStarted := timing.start()
			rawTx, err := conn.BeginTx(ctx, txOptions)
			timing.finish(DbTimingBegin, beginStarted)
			if err != nil {
				panic(err)
			}
			tx := &postCommitPgTx{PgTx: rawTx,
				ownershipAllowed: txOptions.IsoLevel == pgx.ReadCommitted &&
					!retryOptions.rerunOnCommitError && !retryOptions.rerunOnTransientError && !retryOptions.rerunOnConnectionError}
			// if debugOptions.txCommitSeparately {
			// 	tx = newDebugTx(tx, conn, txOptions)
			// }
			defer func() {
				if err := recover(); err != nil {
					rollbackStarted := timing.start()
					rollbackTx(ctx, tx)
					timing.finish(DbTimingRollback, rollbackStarted)
					panic(err)
				}
			}()
			func() {
				defer func() {
					if err := recover(); err != nil {
						switch v := err.(type) {
						case error:
							// a statement refused in a transaction an earlier
							// statement aborted is classified by that statement
							v = withAbortingStatementError(conn.Conn().PgConn(), v, callback)
							if isTransientError(v) && retryOptions.rerunOnTransientError {
								pgErr = v
							} else {
								panic(v)
							}
						default:
							panic(v)
						}
					}
				}()
				phase.enter(DbOperationCallback)
				callback(tx)
			}()
			if pgErr == nil {
				// Logger().Printf("Db commit\n")
				// Commit on a context detached from the caller. The
				// transaction body has already succeeded, so abandoning the
				// commit round trip because the requester went away leaves an
				// AMBIGUOUS outcome: postgres commits while the client reports
				// an error, and every post-commit action is then skipped for
				// work that is durably written (observed: a contract's escrow
				// rows committed while its redis mirror increment was dropped,
				// leaving the net escrow counter permanently short). Waiting
				// for the answer costs at most PgCommitTimeout. A transport
				// failure can still leave an ambiguous outcome, which must
				// remain an error rather than replaying the transaction.
				commitCtx, commitCancel := context.WithTimeout(
					context.WithoutCancel(ctx),
					PgCommitTimeout,
				)
				phase.enter(DbOperationCommit)
				commitStarted := timing.start()
				commitErr = commitObservedTx(commitCtx, tx)
				if commitErr == nil {
					phase.enter(DbOperationAcknowledged)
					committedAt = time.Now()
					tx.committedAt = committedAt
					commitPosts = tx.posts
				}
				timing.finish(DbTimingCommit, commitStarted)
				commitCancel()
				if errors.Is(commitErr, pgx.ErrTxCommitRollback) {
					// the callback returned normally after one of its
					// statements failed; the recorded statement error is the
					// cause and decides whether a rerun can succeed
					commitErr = abortedCommitError(conn.Conn().PgConn(), commitErr, callback)
				}
			} else {
				rollbackStarted := timing.start()
				rollbackTx(ctx, tx)
				timing.finish(DbTimingRollback, rollbackStarted)
			}
		})

		if pgErr != nil {
			if retryOptions.rerunOnTransientError && retryEvidence.CanRerun(pgErr) {
				waitStarted := timing.start()
				select {
				case <-ctx.Done():
					timing.finish(DbTimingRetryWait, waitStarted)
					panic(dbContextDoneCause(ctx, pgErr))
				case <-time.After(backoff.NextRetryTimeout()):
					timing.finish(DbTimingRetryWait, waitStarted)
				}
				if retryEndTime.Before(NowUtc()) {
					recordRerunDecision(pgErr, rerunDecisionEndedBudget)
					panic(pgErr)
				}
				if glog.V(2) {
					glog.Infof("[db]transient error, retry: %s\n", ErrorJson(pgErr, debug.Stack()))
				} else if glog.V(1) {
					glog.Infof("[db]transient error, retry = %v\n", pgErr)
				}
				runTxRerunHook(ctx)
				continue
			}
			panic(pgErr)
		}
		if commitErr != nil {
			if retryOptions.rerunOnCommitError && canRetryCommitError(commitErr) && retryEvidence.CanRerun(commitErr) {
				waitStarted := timing.start()
				select {
				case <-ctx.Done():
					timing.finish(DbTimingRetryWait, waitStarted)
					panic(dbContextDoneCause(ctx, commitErr))
				case <-time.After(backoff.NextRetryTimeout()):
					timing.finish(DbTimingRetryWait, waitStarted)
				}
				if retryEndTime.Before(NowUtc()) {
					recordRerunDecision(commitErr, rerunDecisionEndedBudget)
					panic(commitErr)
				}
				if glog.V(2) {
					glog.Infof("[db]commit error, retry: %s\n", ErrorJson(commitErr, debug.Stack()))
				} else if glog.V(1) {
					glog.Infof("[db]commit error, retry = %v\n", commitErr)
				}
				runTxRerunHook(ctx)
				continue
			}
			panic(commitErr)
		}

		// A confirmed commit owns its bounded publications even if the request
		// canceled. The commit timestamp includes post queueing in that budget.
		if len(commitPosts) > 0 {
			phase.enter(DbOperationPostCommit)
			postCtx, postCancel := context.WithDeadline(context.WithoutCancel(ctx), committedAt.Add(TxPostCommitTimeout))
			for offset := 0; offset < len(commitPosts) && postCtx.Err() == nil; offset += 8 {
				RunPosts(postCtx, commitPosts[offset:min(offset+8, len(commitPosts))]...)
			}
			postCancel()
		}
		return
	}
}

/*
type debugTx struct {
	conn PgConn
	txOptions pgx.TxOptions
	PgTx
}

func newDebugTx(tx pgx.Tx, conn PgConn, txOptions pgx.TxOptions) pgx.Tx {
	return &debugTx{
		conn: conn,
		txOptions: txOptions,
		PgTx: tx,
	}
}

func (self *debugTx) commit(ctx context.Context) {
	commitErr := self.Commit(ctx)
	if commitErr != nil {
		panic(fmt.Errorf("[Tx debug] Commit error. (%w)", commitErr))
	}
	tx, txErr := self.conn.BeginTx(ctx, self.txOptions)
	if txErr != nil {
		panic(fmt.Errorf("[Tx debug] Create new transaction error. (%w)", txErr))
	}
	self.PgTx = tx
}

func (self *debugTx) Exec(ctx context.Context, sql string, arguments ...any) (commandTag pgconn.CommandTag, err error) {
	tempTableDropRe := regexp.MustCompile("(?si)^\\s*(CREATE TEMPORARY TABLE\\s*(\\S+).*)\\s+ON COMMIT DROP\\s*$")
	groups := tempTableDropRe.FindStringSubmatch(sql)
	if groups != nil {
		// remove `ON COMMIT DROP`
		sql = groups[1]
		Logger().Printf("[Tx debug] Removed `ON COMMIT DROP` from temp table %s\n", groups[2])
	}

	commandTag, err = self.PgTx.Exec(ctx, sql, arguments...)
	if err != nil {
		return
	}
	self.commit(ctx)
	return
}

// note the batch results need to be closed before commit
// func (self *debugTx) SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults {
// 	results := self.PgTx.SendBatch(ctx, b)
// 	self.commit(ctx)
// 	return results
// }
*/

// Preserve an existing failure while closing its rows. After a successful
// callback, Close must finish before Err is checked: draining a final reply
// can produce the first error after the callback has consumed its last row.
func WithPgResult(r PgResult, err error, callback any) {
	closed := false
	defer func() {
		if !closed && r != nil {
			// This path unwinds an earlier error, callback panic or Goexit.
			// Cleanup must not replace that primary outcome with its panic.
			func() {
				defer func() { _ = recover() }()
				r.Close()
			}()
		}
	}()
	Raise(err)
	switch v := callback.(type) {
	case func():
		v()
	case func(PgResult):
		v(r)
	default:
		panic(errors.New(fmt.Sprintf("Unknown callback: %s", callback)))
	}
	Raise(r.Err())
	closed = true
	r.Close()
	Raise(r.Err())
}

func RaisePgResult[T any](result T, err error) T {
	Raise(err)
	return result
}

func BatchInTx(ctx context.Context, tx PgTx, callback func(PgBatch), resultsCallbacks ...func(PgBatchResults)) {
	batch := &pgx.Batch{}
	callback(batch)
	results := tx.SendBatch(ctx, batch)
	for _, resultsCallback := range resultsCallbacks {
		resultsCallback(results)
	}
	err := results.Close()
	if err != nil {
		panic(err)
	}
}

type ComplexValue interface {
	// unpack a complex value into individual values
	Values() []any
}

// CreateTempTableInTxAllowDuplicates

// spec is `table_name(value_column_name type)`
func CreateTempTableInTx[T any](ctx context.Context, tx PgTx, spec string, values ...T) {
	tableSpec := parseTempTableSpec(spec)

	pgParts := []string{}
	for i, valueColumnName := range tableSpec.valueColumnNames {
		valuePgType := tableSpec.valuePgTypes[i]
		valuePart := fmt.Sprintf("%s %s NOT NULL", valueColumnName, valuePgType)
		pgParts = append(pgParts, valuePart)
	}

	pgPlaceholders := []string{}
	i := 1
	for range tableSpec.valueColumnNames {
		pgPlaceholders = append(pgPlaceholders, fmt.Sprintf("$%d", i))
		i += 1
	}

	RaisePgResult(tx.Exec(ctx, fmt.Sprintf(
		`
			CREATE TEMPORARY TABLE %s (
				%s,
				PRIMARY KEY (%s)
			)
			ON COMMIT DROP
		`,
		tableSpec.tableName,
		strings.Join(pgParts, ", "),
		strings.Join(tableSpec.valueColumnNames, ", "),
	)))
	BatchInTx(ctx, tx, func(batch PgBatch) {
		for _, value := range values {
			var pgValues = []any{}
			pgValues = expandValue(value, pgValues)
			if len(pgValues) != len(pgPlaceholders) {
				panic(fmt.Errorf("Expected %d values but found %d.", len(pgPlaceholders), len(pgValues)))
			}
			batch.Queue(
				fmt.Sprintf(
					`
						INSERT INTO %s (%s) VALUES (%s)
						ON CONFLICT DO NOTHING
					`,
					tableSpec.tableName,
					strings.Join(tableSpec.valueColumnNames, ", "),
					strings.Join(pgPlaceholders, ", "),
				),
				pgValues...,
			)
		}
	})
}

func CreateTempTableInTxAllowDuplicates[T any](ctx context.Context, tx PgTx, spec string, values ...T) {
	tableSpec := parseTempTableSpec(spec)

	pgParts := []string{}
	for i, valueColumnName := range tableSpec.valueColumnNames {
		valuePgType := tableSpec.valuePgTypes[i]
		valuePart := fmt.Sprintf("%s %s NOT NULL", valueColumnName, valuePgType)
		pgParts = append(pgParts, valuePart)
	}

	pgPlaceholders := []string{}
	i := 1
	for range tableSpec.valueColumnNames {
		pgPlaceholders = append(pgPlaceholders, fmt.Sprintf("$%d", i))
		i += 1
	}

	RaisePgResult(tx.Exec(ctx, fmt.Sprintf(
		`
			CREATE TEMPORARY TABLE %s (
				%s
			)
			ON COMMIT DROP
		`,
		tableSpec.tableName,
		strings.Join(pgParts, ", "),
	)))
	BatchInTx(ctx, tx, func(batch PgBatch) {
		for _, value := range values {
			pgValues := []any{}
			pgValues = expandValue(value, pgValues)
			if len(pgValues) != len(pgPlaceholders) {
				panic(fmt.Errorf("Expected %d values but found %d.", len(pgPlaceholders), len(pgValues)))
			}
			batch.Queue(
				fmt.Sprintf(
					`
						INSERT INTO %s (%s) VALUES (%s)
					`,
					tableSpec.tableName,
					strings.Join(tableSpec.valueColumnNames, ", "),
					strings.Join(pgPlaceholders, ", "),
				),
				pgValues...,
			)
		}
	})
}

// many to one join table
// spec is `table_name(key_column_name type[, ...] -> value_column_name type[, ...])`
func CreateTempJoinTableInTx[K comparable, V any](ctx context.Context, tx PgTx, spec string, values map[K]V) {
	tableSpec := parseTempJoinTableSpec(spec)

	pgParts := []string{}
	for i, keyColumnName := range tableSpec.keyColumnNames {
		keyPgType := tableSpec.keyPgTypes[i]
		keyPart := fmt.Sprintf("%s %s NOT NULL", keyColumnName, keyPgType)
		pgParts = append(pgParts, keyPart)
	}
	for i, valueColumnName := range tableSpec.valueColumnNames {
		valuePgType := tableSpec.valuePgTypes[i]
		nullable := "NOT NULL"
		if tableSpec.valueNullables[i] {
			nullable = "NULL"
		}
		valuePart := fmt.Sprintf("%s %s %s", valueColumnName, valuePgType, nullable)
		pgParts = append(pgParts, valuePart)
	}

	columnNames := []string{}
	columnNames = append(columnNames, tableSpec.keyColumnNames...)
	columnNames = append(columnNames, tableSpec.valueColumnNames...)
	pgPlaceholders := []string{}
	i := 1
	for range tableSpec.keyColumnNames {
		pgPlaceholders = append(pgPlaceholders, fmt.Sprintf("$%d", i))
		i += 1
	}
	for range tableSpec.valueColumnNames {
		pgPlaceholders = append(pgPlaceholders, fmt.Sprintf("$%d", i))
		i += 1
	}

	RaisePgResult(tx.Exec(ctx, fmt.Sprintf(
		`
			CREATE TEMPORARY TABLE %s (
				%s,
				PRIMARY KEY (%s)
			)
			ON COMMIT DROP
		`,
		tableSpec.tableName,
		strings.Join(pgParts, ", "),
		strings.Join(tableSpec.keyColumnNames, ", "),
	)))
	BatchInTx(ctx, tx, func(batch PgBatch) {
		for key, value := range values {
			pgValues := []any{}
			pgValues = expandValue(key, pgValues)
			pgValues = expandValue(value, pgValues)
			if len(pgValues) != len(pgPlaceholders) {
				panic(fmt.Errorf("Expected %d values but found %d.", len(pgPlaceholders), len(pgValues)))
			}
			batch.Queue(
				fmt.Sprintf(
					`
						INSERT INTO %s (%s) VALUES (%s)
						ON CONFLICT DO NOTHING
					`,
					tableSpec.tableName,
					strings.Join(columnNames, ", "),
					strings.Join(pgPlaceholders, ", "),
				),
				pgValues...,
			)
		}
	})
}

func expandValue[T any](value T, out []any) []any {
	if v, ok := any(value).(ComplexValue); ok {
		out = append(out, v.Values()...)
		// value may be a struct, `&value` will convert it to an interface type
	} else if v, ok := any(&value).(ComplexValue); ok {
		out = append(out, v.Values()...)
	} else {
		out = append(out, value)
	}
	return out
}

type TempTableSpec struct {
	tableName        string
	valueColumnNames []string
	valuePgTypes     []string
}

// spec is `table_name(value_column_name type)`
func parseTempTableSpec(spec string) *TempTableSpec {
	re := regexp.MustCompile("(?s)^\\s*(\\w+)\\s*\\((.*)\\)")
	groups := re.FindStringSubmatch(spec)
	if groups == nil {
		panic(errors.New(fmt.Sprintf("Bad spec: %s", spec)))
	}

	valueColumnNames, valuePgTypes, _ := parseSpec(groups[2])

	return &TempTableSpec{
		tableName:        groups[1],
		valueColumnNames: valueColumnNames,
		valuePgTypes:     valuePgTypes,
	}
}

type TempJoinTableSpec struct {
	tableName        string
	keyColumnNames   []string
	keyPgTypes       []string
	valueColumnNames []string
	valuePgTypes     []string
	valueNullables   []bool
}

// spec is `table_name(key_column_name type[, ...] -> value_column_name type[, ...])`
func parseTempJoinTableSpec(spec string) *TempJoinTableSpec {
	re := regexp.MustCompile("(?s)^\\s*(\\w+)\\s*\\((.*)\\s*->\\s*(.*)\\)")
	groups := re.FindStringSubmatch(spec)
	if groups == nil {
		panic(errors.New(fmt.Sprintf("Bad spec: %s", spec)))
	}

	keyColumnNames, keyPgTypes, _ := parseSpec(groups[2])
	valueColumnNames, valuePgTypes, valueNullables := parseSpec(groups[3])

	return &TempJoinTableSpec{
		tableName:        groups[1],
		keyColumnNames:   keyColumnNames,
		keyPgTypes:       keyPgTypes,
		valueColumnNames: valueColumnNames,
		valuePgTypes:     valuePgTypes,
		valueNullables:   valueNullables,
	}
}

func parseSpec(spec string) (columnNames []string, pgTypes []string, nullables []bool) {
	re := regexp.MustCompile("(?is)^\\s*(\\w+)\\s+([^,]+?)(\\s+NULL)?\\s*(?:,|$)")

	for {
		groups := re.FindStringSubmatch(spec)
		if groups == nil {
			break
		}
		columnNames = append(columnNames, strings.TrimSpace(groups[1]))
		pgTypes = append(pgTypes, strings.TrimSpace(groups[2]))
		nullables = append(nullables, strings.TrimSpace(groups[3]) != "")
		spec = spec[len(groups[0]):]
	}

	return
}
