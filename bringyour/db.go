package bringyour

import (
	"sync"
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)


// type aliases to simplify user code
type PgConn = *pgxpool.Conn
type PgTx = pgx.Tx
type PgResult = pgx.Rows
type PgNamedArgs = pgx.NamedArgs

// FIXME Id should just be Pg Scanner/Valuer compatible, then we can just Id everywhere
// type PgUUID = pgtype.UUIDw


type safePgPool struct {
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
			// "bringyour",
			// "pigsty-vesicle-trombone-vigour",
			// "192.168.208.135:5432",
			// "bringyour",
			optionsString,
		)
		Logger().Printf("Db url %d %s\n", postgresUrl)

		var err error
		self.pool, err = pgxpool.New(context.Background(), postgresUrl)
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

var safePool *safePgPool = &safePgPool{}

func pool() *pgxpool.Pool {
	return safePool.open()
}



type DbRetryOptions struct {
	// rerun the entire callback on commit error
	rerunOnCommitError bool
	rerunOnDisconnect bool
	// this only works if the conflict, e.g. an ID, is changed on each run
	// the BY coding style will generate the id in the callback, so this is generally considered safe
	rerunOnConstraintError bool
	retryTimeout time.Duration
}
// this is the default for `Db` and `Tx`
func OptRetryDefault() DbRetryOptions {
	return &DbRetryOptions{
		rerunOnCommitError: true,
		rerunOnConnectionError: true,
		rerunOnConstraintError: true,
		retryTimeout: 200 * time.Millisecond,
	}
}
func OptNoRetry() DbRetryOptions {
	return &DbRetryOptions{
		rerunOnCommitError: false,
		rerunOnConnectionError: false,
		rerunOnConstraintError: false,
	}
}


const TxSerializable = pgx.Serializable



// FIXME by default Db Should be read-only
func Db(ctx context.Context, callback func(PgConn), options ...any) error {
	// fixme rerun callback on disconnect with a new connection

	retryOptions := OptRetryDefault()
	for _, option := range options {
		switch v := option.(type) {
		case DbRetryOptions:
			retryOptions = v
	}

	// context := context.Background()

	// var conn *pgxpool.Conn
	// var connErr error

	// fixme retry if error


	for {
		var pgErr pgx.PgError
		conn, connErr := pool().Acquire(context)
		if connErr != nil {
			if retryOptions != nil && retryOptions.rerunOnConnectionError {
				select {
				case <- ctx.Done():
					return errors.New("Done")
				case time.After(retryOptions.retryTimeout):
				}
			}
			return connErr
		}

		connErr = conn.Ping(context)
		if connErr != nil {
			// take the bad connection out of the pool
			pgxConn := conn.Hijack()
			pgxConn.Close(context)
			conn = nil

			f retryOptions != nil && retryOptions.rerunOnConnectionError {
				select {
				case <- ctx.Done():
					return errors.New("Done")
				case time.After(retryOptions.retryTimeout):
				}
			}
			return connErr
		}

		func() {
			defer func() {
				if err := recover(); err != nil {
					switch v := err.(type) {
					case pgx.PgError:
						pgErr = v
					default:
						panic(err)
					}
				}
			}()
			defer conn.Release()
			callback(context, conn)
		}()

		if pgErr != nil {
			if pgErr.ConstraintName != "" && retryOptions != nil && retryOptions.rerunOnConstraintError {
				select {
				case <- ctx.Done():
					return errors.New("Done")
				case time.After(retryOptions.retryTimeout):
				}
				LOG("constraint", pgErr)
				continue
			}
			return pgErr
		}

		return nil
	}
}


func Tx(ctx context.Context, callback func(PgTx), options ...any) error {
	// context := context.Background()

	// var tx pgx.Tx
	// var err error

	// // fixme retry if error

	// tx, err = pool().Begin(context)
	// if err != nil {
	// 	panic(err)
	// }
	// defer func() {
	// 	err := recover()
	// 	if err == nil {
	// 		tx.Commit(context)
	// 	} else {
	// 		tx.Rollback(context)
	// 	}
	// }()

	// callback(context, tx)

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
		err := Db(ctx, func (context context.Context, conn PgConn) {
			tx, err := conn.BeginTx(context, txOptions)
			if err != nil {
				return err
			}
			defer func() {
				if err := recover(); err != nil {
					if rollbackErr := tx.Rollback(context); rollbackErr != nil {
						panic(rollbackErr)
					}
					panic(err)
				}
			}()
			func() {
				defer func() {
					if err := recover(); err != nil {
						switch v := err.(type) {
						case pgx.PgError:
							pgErr = v
						default:
							panic(err)
						}
					}
				}()
				callback(context, tx)
			}()
			Logger().Printf("Db commit\n")
			commitErr := tx.Commit(context)
		}, options...)
		if err != nil {
			return err
		}
		if commitErr != nil {
			if retryOptions != nil && retryOptions.rerunOnCommitError {
				select {
				case <- ctx.Done():
					return errors.New("Done")
				case time.After(retryOptions.retryTimeout):
				}
				LOG("commit")
				continue
			}
			return commitErr
		}
		return nil
	}
}


// the return of many pgx operations is (r, error) where r conforms to the interface below
// type DbResult interface {
// 	Close()
// 	Err() error
// }

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

// PgConn and PgTx conform to this
type PgDb interface {
	Query
	Exec
}


// spec is `table_name(value_column_name)`
func CreateTempTable[T any](ctx context.Context, db PgDb, spec string, ...values T) {

}

// many to one join table
// spec is `table_name(key_column_name, value_column_name)`
func CreateTempJoinTable[K any, V any](ctx context.Context, db PgDb, spec string, values map[K]V) {

}

