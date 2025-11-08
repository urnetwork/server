package server

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"runtime"
	"runtime/debug"
	// "strconv"
	mathrand "math/rand"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	// "github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/golang/glog"
)

/*
`Db` runs in read-only mode
`Tx` runs in read-write mode by default, which can be changed with `pgx.TxOptions`
*/

// note all times in the db should be `timestamp` UTC. Do not use `timestamp with time zone`. See `NowUtc`

var DbContextDoneError = errors.New("Done")

// type aliases to simplify user code
type PgConn = *pgxpool.Conn
type PgTx = pgx.Tx
type PgResult = pgx.Rows
type PgTag = pgconn.CommandTag
type PgNamedArgs = pgx.NamedArgs
type PgBatch = *pgx.Batch
type PgBatchResults = pgx.BatchResults

const TxSerializable = pgx.Serializable
const TxReadCommitted = pgx.ReadCommitted

var safePool = &safePgPool{
	ctx: context.Background(),
}

func pool() *pgxpool.Pool {
	return safePool.open()
}

// resets the connection pool
// call this after changes to the env
func PgReset() {
	safePool.reset()
}

type safePgPool struct {
	ctx   context.Context
	mutex sync.Mutex
	pool  *pgxpool.Pool
}

func (self *safePgPool) open() *pgxpool.Pool {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if self.pool == nil {
		// Logger().Printf("Db init\n")

		dbKeys := Vault.RequireSimpleResource("pg.yml")
		dbConfigKeys := Config.RequireSimpleResource("db.yml")

		minConnections := dbConfigKeys.RequireInt("min_connections")
		maxConnections := dbConfigKeys.RequireInt("max_connections")
		healthCheckPeriod := "1s"
		connectionMaxLifetime := "1h"
		connectionMaxLifetimeJitter := "15m"
		connectionMaxIdleTime := "5m"
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
			"connect_timeout":               "300",
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
			dbKeys.RequireString("password"),
			dbKeys.RequireString("authority"),
			dbKeys.RequireString("db"),
			optionsString,
		)
		// Logger().Printf("Db url %s\n", postgresUrl)
		config, err := pgxpool.ParseConfig(postgresUrl)
		if err != nil {
			panic(fmt.Sprintf("Unable to parse url: %s", err))
		}
		config.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
			// use `Id` instead of the default UUID type
			pgxRegisterIdType(conn.TypeMap())
			return nil
		}

		self.pool, err = pgxpool.NewWithConfig(self.ctx, config)
		if err != nil {
			panic(fmt.Sprintf("Unable to connect to database: %s", err))
		}
	}
	return self.pool
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
	rerunOnTransientError bool
	retryMinTimeout       time.Duration
	retryMaxTimeout       time.Duration
	endRetryTimeout       time.Duration
	// debugRetryTimeout time.Duration
}

func (self *DbRetryOptions) retryTimeout() time.Duration {
	return self.retryMinTimeout + time.Duration(
		mathrand.Int63n(int64(self.retryMaxTimeout-self.retryMinTimeout)),
	)
}

// this is the default for `Db` and `Tx`
func OptRetryDefault() DbRetryOptions {
	return DbRetryOptions{
		rerunOnCommitError:     true,
		rerunOnConnectionError: true,
		rerunOnTransientError:  true,
		retryMinTimeout:        20 * time.Millisecond,
		retryMaxTimeout:        2000 * time.Millisecond,
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

// transient errors can be resolved by either
// - changing the parameters of the query to avoid constraint conflicts
// - chaning the timing of the query to avoid rollbacks
// https://www.postgresql.org/docs/current/mvcc-serialization-failure-handling.html
// https://www.postgresql.org/docs/current/errcodes-appendix.html
func isTransientError(err error) bool {
	switch v := err.(type) {
	case *pgconn.PgError:
		if pgerrcode.IsIntegrityConstraintViolation(v.Code) {
			return true
		}
		if pgerrcode.IsTransactionRollback(v.Code) {
			return true
		}
		// fmt.Printf("[db]intransient error = %d\n", v.Code)
		return false
	case *PgRetry:
		return true
	default:
		return false
	}
}

func isConnectionError(err error) bool {
	switch v := err.(type) {
	case *pgconn.PgError:
		if pgerrcode.IsConnectionException(v.Code) {
			// try a new connection
			return true
		}
		return false
	default:
		switch err.Error() {
		// pgconn.connLockError
		// https://github.com/jackc/pgconn/blob/master/errors.go
		case "conn closed":
			// try a new connection
			return true
		default:
			return false
		}
	}
}

func Db(ctx context.Context, callback func(PgConn), options ...any) {
	c := func() {
		db(ctx, callback, options...)
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

func db(ctx context.Context, callback func(PgConn), options ...any) {
	retryOptions := OptRetryDefault()
	rwOptions := OptReadOnly()
	// debugOptions := OptNoDebug()
	for _, option := range options {
		switch v := option.(type) {
		case DbRetryOptions:
			retryOptions = v
		case DbReadWriteOptions:
			rwOptions = v
			// case DbDebugOptions:
			// 	debugOptions = v
		}
	}

	retryEndTime := NowUtc().Add(retryOptions.endRetryTimeout)
	// retryDebugTime := NowUtc().Add(retryOptions.debugRetryTimeout)
	for {
		var pgErr error
		conn, connErr := pool().Acquire(ctx)
		if connErr != nil {
			if retryOptions.rerunOnConnectionError {
				select {
				case <-ctx.Done():
					panic(DbContextDoneError)
				case <-time.After(retryOptions.retryTimeout()):
				}
			}
			panic(connErr)
		}

		connErr = conn.Ping(ctx)
		if connErr != nil {
			// take the bad connection out of the pool
			pgxConn := conn.Hijack()
			pgxConn.Close(ctx)
			conn = nil

			if retryOptions.rerunOnConnectionError {
				select {
				case <-ctx.Done():
					panic(DbContextDoneError)
				case <-time.After(retryOptions.retryTimeout()):
				}
			}
			panic(connErr)
		}

		func() {
			defer func() {
				if err := recover(); err != nil {
					switch v := err.(type) {
					case error:
						if isTransientError(v) && retryOptions.rerunOnTransientError {
							pgErr = v
						} else if isConnectionError(v) && retryOptions.rerunOnConnectionError {
							connErr = v
						} else {
							panic(v)
						}
					default:
						panic(v)
					}
				}
			}()
			defer func() {
				if connErr != nil {
					// take the bad connection out of the pool
					pgxConn := conn.Hijack()
					pgxConn.Close(ctx)
					conn = nil
				} else {
					conn.Release()
				}
			}()
			// defer Logger().Printf("DB CLOSE\n")
			if !rwOptions.readOnly {
				// the default is read only, escalate to rw
				RaisePgResult(conn.Exec(ctx, "SET default_transaction_read_only=off"))
			}
			callback(conn)
		}()

		if pgErr != nil {
			if isTransientError(pgErr) && retryOptions.rerunOnTransientError {
				select {
				case <-ctx.Done():
					panic(DbContextDoneError)
				case <-time.After(retryOptions.retryTimeout()):
				}
				if retryEndTime.Before(NowUtc()) {
					panic(pgErr)
				}
				if glog.V(2) {
					glog.Infof("[db]transient error, retry: %s\n", ErrorJson(pgErr, debug.Stack()))
				} else {
					glog.Infof("[db]transient error, retry = %v\n", pgErr)
				}
				continue
			}
			panic(pgErr)
		}
		if connErr != nil {
			if retryOptions.rerunOnConnectionError {
				select {
				case <-ctx.Done():
					panic(DbContextDoneError)
				case <-time.After(retryOptions.retryTimeout()):
				}
				continue
			}
			panic(connErr)
		}

		return
	}
}

func Tx(ctx context.Context, callback func(PgTx), options ...any) {
	c := func() {
		tx(ctx, callback, options...)
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

func tx(ctx context.Context, callback func(PgTx), options ...any) {
	retryOptions := OptRetryDefault()
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
	for {
		var pgErr error
		var commitErr error
		db(ctx, func(conn PgConn) {
			tx, err := conn.BeginTx(ctx, txOptions)
			if err != nil {
				panic(err)
			}
			// if debugOptions.txCommitSeparately {
			// 	tx = newDebugTx(tx, conn, txOptions)
			// }
			defer func() {
				if err := recover(); err != nil {
					if rollbackErr := tx.Rollback(ctx); rollbackErr != nil {
						panic(rollbackErr)
					}
					panic(err)
				}
			}()
			func() {
				defer func() {
					if err := recover(); err != nil {
						switch v := err.(type) {
						case error:
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
				callback(tx)
			}()
			if pgErr == nil {
				// Logger().Printf("Db commit\n")
				commitErr = tx.Commit(ctx)
			} else {
				if rollbackErr := tx.Rollback(ctx); rollbackErr != nil {
					panic(rollbackErr)
				}
			}
		}, options...)

		if pgErr != nil {
			if isTransientError(pgErr) && retryOptions.rerunOnTransientError {
				select {
				case <-ctx.Done():
					panic(DbContextDoneError)
				case <-time.After(retryOptions.retryTimeout()):
				}
				if retryEndTime.Before(NowUtc()) {
					panic(pgErr)
				}
				if glog.V(2) {
					glog.Infof("[db]transient error, retry: %s\n", ErrorJson(pgErr, debug.Stack()))
				} else {
					glog.Infof("[db]transient error, retry = %v\n", pgErr)
				}
				continue
			}
			panic(pgErr)
		}
		if commitErr != nil {
			if retryOptions.rerunOnCommitError {
				select {
				case <-ctx.Done():
					panic(DbContextDoneError)
				case <-time.After(retryOptions.retryTimeout()):
				}
				if retryEndTime.Before(NowUtc()) {
					panic(commitErr)
				}
				if glog.V(2) {
					glog.Infof("[db]commit error, retry: %s\n", ErrorJson(commitErr, debug.Stack()))
				} else {
					glog.Infof("[db]commit error, retry = %v\n", commitErr)
				}
				continue
			}
			panic(commitErr)
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

func WithPgResult(r PgResult, err error, callback any) {
	Raise(err)
	defer r.Close()
	switch v := callback.(type) {
	case func():
		v()
	case func(PgResult):
		v(r)
	default:
		panic(errors.New(fmt.Sprintf("Unknown callback: %s", callback)))
	}
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
