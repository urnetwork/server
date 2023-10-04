package bringyour

import (
	"sync"
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
	"errors"
	"regexp"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	// "github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)


/*
`Db` runs in read-only mode
`Tx` runs in read-write mode by default, which can be changed with `pgx.TxOptions`
*/


// type aliases to simplify user code
type PgConn = *pgxpool.Conn
type PgTx = pgx.Tx
type PgResult = pgx.Rows
type PgNamedArgs = pgx.NamedArgs
type PgBatch = pgx.Batch
type PgBatchResults = pgx.BatchResults


const TxSerializable = pgx.Serializable


type safePgPool struct {
	ctx context.Context
	mutex sync.Mutex
	pool *pgxpool.Pool
}

func (self *safePgPool) open() *pgxpool.Pool {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if self.pool == nil {
		Logger().Printf("Db init\n")

		dbKeys := Vault.RequireSimpleResource("pg.yml")

		// see the Config struct for human understandable docs
		// https://github.com/jackc/pgx/blob/master/pgxpool/pool.go#L103
		// fixme use Config struct?
		options := map[string]string{
			"sslmode": "disable",
			"pool_max_conns": strconv.Itoa(32),
			"pool_min_conns": strconv.Itoa(4),
			"pool_max_conn_lifetime": "8h",
			"pool_max_conn_lifetime_jitter": "1h",
			"pool_max_conn_idle_time": "60s",
			"pool_health_check_period": "1h",
			// must use `Tx` to write, which sets `AccessMode: pgx.ReadWrite`
			"default_transaction_read_only": "on",
		}
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
		Logger().Printf("Db url %d %s\n", postgresUrl)
		config, err := pgxpool.ParseConfig(postgresUrl)
		if err != nil {
			panic(fmt.Sprintf("Unable to parse url: %s", err))
		}
		config.AfterConnect = func(ctx context.Context, conn *pgx.Conn)(error) {
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
	self.mutex.Lock()
	defer self.mutex.Unlock()

	if self.pool != nil {
		self.pool.Close()
		self.pool = nil
	}
}

var safePool *safePgPool = &safePgPool{
	ctx: context.Background(),
}

func pool() *pgxpool.Pool {
	return safePool.open()
}


type DbRetryOptions struct {
	// rerun the entire callback on commit error
	rerunOnCommitError bool
	rerunOnConnectionError bool
	// this only works if the conflict, e.g. an ID, is changed on each run
	// the BY coding style will generate the id in the callback, so this is generally considered safe
	rerunOnConstraintError bool
	retryTimeout time.Duration
}

// this is the default for `Db` and `Tx`
func OptRetryDefault() DbRetryOptions {
	return DbRetryOptions{
		rerunOnCommitError: true,
		rerunOnConnectionError: true,
		rerunOnConstraintError: true,
		retryTimeout: 200 * time.Millisecond,
	}
}

func OptNoRetry() DbRetryOptions {
	return DbRetryOptions{
		rerunOnCommitError: false,
		rerunOnConnectionError: false,
		rerunOnConstraintError: false,
	}
}


func Db(ctx context.Context, callback func(PgConn), options ...any) error {
	retryOptions := OptRetryDefault()
	for _, option := range options {
		switch v := option.(type) {
		case DbRetryOptions:
			retryOptions = v
		}
	}

	for {
		var pgErr *pgconn.PgError
		conn, connErr := pool().Acquire(ctx)
		if connErr != nil {
			if retryOptions.rerunOnConnectionError {
				select {
				case <- ctx.Done():
					return errors.New("Done")
				case <- time.After(retryOptions.retryTimeout):
				}
			}
			return connErr
		}

		connErr = conn.Ping(ctx)
		if connErr != nil {
			// take the bad connection out of the pool
			pgxConn := conn.Hijack()
			pgxConn.Close(ctx)
			conn = nil

			if retryOptions.rerunOnConnectionError {
				select {
				case <- ctx.Done():
					return errors.New("Done")
				case <- time.After(retryOptions.retryTimeout):
				}
			}
			return connErr
		}

		func() {
			defer func() {
				if err := recover(); err != nil {
					switch v := err.(type) {
					case pgconn.PgError:
						pgErr = &v
					default:
						panic(err)
					}
				}
			}()
			defer conn.Release()
			callback(conn)
		}()

		if pgErr != nil {
			if pgErr.ConstraintName != "" && retryOptions.rerunOnConstraintError {
				select {
				case <- ctx.Done():
					return errors.New("Done")
				case <- time.After(retryOptions.retryTimeout):
				}
				Logger().Printf("constraint error, retry (%s)", pgErr)
				continue
			}
			return pgErr
		}

		return nil
	}
}


func Tx(ctx context.Context, callback func(PgTx), options ...any) error {
	retryOptions := OptRetryDefault()
	// by default use RepeatableRead isolation
	// https://www.postgresql.org/docs/current/transaction-iso.html
	txOptions := pgx.TxOptions{
		IsoLevel: pgx.RepeatableRead,
		AccessMode: pgx.ReadWrite,
		DeferrableMode: pgx.NotDeferrable,
	}
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
		}
	}

	for {
		var commitErr error
		err := Db(ctx, func (conn PgConn) {
			tx, err := conn.BeginTx(ctx, txOptions)
			if err != nil {
				return
			}
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
						case pgconn.PgError:
							commitErr = &v
						default:
							panic(err)
						}
					}
				}()
				callback(tx)
			}()
			Logger().Printf("Db commit\n")
			commitErr = tx.Commit(ctx)
		}, options...)
		if err != nil {
			return err
		}
		if commitErr != nil {
			if retryOptions.rerunOnCommitError {
				select {
				case <- ctx.Done():
					return errors.New("Done")
				case <- time.After(retryOptions.retryTimeout):
				}
				Logger().Printf("commit")
				continue
			}
			return commitErr
		}
		return nil
	}
}


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


func BatchInTx(ctx context.Context, tx PgTx, callback func (*PgBatch), resultCallbacks ...func(PgBatchResults)) {
	batch := &pgx.Batch{}
	callback(batch)
	results := tx.SendBatch(ctx, batch)
	for _, resultCallback := range resultCallbacks {
		resultCallback(results)
	}
	results.Close()
}


// spec is `table_name(value_column_name)`
func CreateTempTableInTx[T any](ctx context.Context, tx PgTx, spec string, values ...T) {
	tableSpec := parseTempTableSpec(spec)
	var valueSqlType string
	for _, value := range values {
		switch v := any(value).(type) {
		case Id:
			valueSqlType = "uuid"
		default:
			panic(fmt.Errorf("Unknown SQL type for %v", v))
		}
		break
	}
	_, err := tx.Exec(ctx, fmt.Sprintf(
		`
			CREATE TEMPORARY TABLE %s (
				%s %s NOT NULL,
				PRIMARY KEY (%s)
			)
		`,
		tableSpec.tableName,
		tableSpec.valueColummName,
		valueSqlType,
		tableSpec.valueColummName,
	))
	Raise(err)
	BatchInTx(ctx, tx, func (batch *PgBatch) {
		for _, value := range values {
			batch.Queue(
				fmt.Sprintf(
					`
						INSERT INTO %s (%s) VALUES ($1)
						ON CONFLICT IGNORE
					`,
					tableSpec.tableName,
					tableSpec.valueColummName,
				),
				value,
			)
		}
	})
}


// many to one join table
// spec is `table_name(key_column_name, value_column_name)`
func CreateTempJoinTableInTx[K comparable, V any](ctx context.Context, tx PgTx, spec string, values map[K]V) {
	tableSpec := parseTempJoinTableSpec(spec)
	var keySqlType string
	var valueSqlType string
	for key, value := range values {
		switch v := any(key).(type) {
		case Id:
			keySqlType = "uuid"
		default:
			panic(fmt.Errorf("Unknown SQL type for %v", v))
		}
		switch v := any(value).(type) {
		case int:
			valueSqlType = "int"
		default:
			panic(fmt.Errorf("Unknown SQL type for %v", v))
		}
		break
	}
	_, err := tx.Exec(ctx, fmt.Sprintf(
		`
			CREATE TEMPORARY TABLE %s (
				%s %s NOT NULL,
				%s %s NOT NULL,
				PRIMARY KEY (%s)
			)
		`,
		tableSpec.tableName,
		tableSpec.keyColummName,
		keySqlType,
		tableSpec.valueColummName,
		valueSqlType,
		tableSpec.keyColummName,
	))
	Raise(err)
	BatchInTx(ctx, tx, func (batch *PgBatch) {
		for key, value := range values {
			batch.Queue(
				fmt.Sprintf(
					`
						INSERT INTO %s (%s, %s) VALUES ($1, $2)
						ON CONFLICT IGNORE
					`,
					tableSpec.tableName,
					tableSpec.keyColummName,
					tableSpec.valueColummName,
				),
				key,
				value,
			)
		}
	})
}


type TempTableSpec struct {
	tableName string
	valueColummName string
}

// spec is `table_name(value_column_name)`
func parseTempTableSpec(spec string) *TempTableSpec {
	re := regexp.MustCompile("(\\w+)\\s*(?:\\s*(\\w+)\\s*)")
	groups := re.FindStringSubmatch(spec)
	if groups == nil {
		panic(errors.New(fmt.Sprintf("Bad spec: %s", spec)))
	}
	return &TempTableSpec{
		tableName: groups[1],
		valueColummName: groups[2],
	}
}


type TempJoinTableSpec struct {
	tableName string
	keyColummName string
	valueColummName string
}

// spec is `table_name(key_column_name, value_column_name)`
func parseTempJoinTableSpec(spec string) *TempJoinTableSpec {
	re := regexp.MustCompile("(\\w+)\\s*(?:\\s*(\\w+),\\s*(\\w+)\\s*)")
	groups := re.FindStringSubmatch(spec)
	if groups == nil {
		panic(errors.New(fmt.Sprintf("Bad spec: %s", spec)))
	}
	return &TempJoinTableSpec{
		tableName: groups[1],
		keyColummName: groups[2],
		valueColummName: groups[3],
	}
}
