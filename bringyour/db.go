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
type PgUUID = pgtype.UUID


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
	// rerun the entire callback on error
	rerunOnError bool
}
func newDbRetryOptions(rerunOnError bool) *DbRetryOptions {
	return &DbRetryOptions{
		rerunOnError: rerunOnError,
	}
}



func Db(callback func(context.Context, PgConn), options ...any) {
	// fixme rerun callback on disconnect with a new connection

	context := context.Background()

	var conn *pgxpool.Conn
	var err error

	// fixme retry if error

	for attempts := 0; attempts < 8; attempts += 1 {
		conn, err = pool().Acquire(context)
		if err != nil {
			panic(err)
		}
		defer conn.Release()

		err = conn.Ping(context)
		if err != nil {
			// take the bad connection out of the pool
			pgxConn := conn.Hijack()
			pgxConn.Close(context)
			conn = nil
		}
		break
	}
	if conn == nil {
		panic("Could not acquire connection.")
	}

	callback(context, conn)
}


func Tx(callback func(context.Context, PgTx), options ...any) {
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

	Db(func (context context.Context, conn PgConn) {
		txOptions := pgx.TxOptions{}
		tx, err := conn.BeginTx(context, txOptions)
		if err != nil {
			panic(err)
		}
		defer func() {
			err := recover()
			if err == nil {
				Logger().Printf("Db commit\n")
				tx.Commit(context)
			} else {
				tx.Rollback(context)
				panic(err)
			}
		}()
		callback(context, tx)
	}, options...)
}


type Closeable interface {
	Close()
	Err() error
}

func With[T Closeable](t T, err error, callback func()) {
	Raise(err)
	defer t.Close()
	callback()
	if err := t.Err(); err != nil {
		panic(err)
	}
}


