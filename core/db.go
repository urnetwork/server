// operator.Db over server's own helpers (server/db.go): server.Db and server.Tx run every call, with
// their pools, retry taxonomy, lock bound, statement stop and Postgres guards, and this file only turns
// operator-core's types and errors into theirs and back. server's helpers raise errors by panic; a
// callback's error is raised for them to classify, as server's own callbacks raise theirs, and whatever
// error ends the call comes back as the call's error. A runtime panic stays a panic.

package core

import (
	"context"
	"errors"
	"fmt"
	"runtime"

	"github.com/jackc/pgx/v5"

	operator "github.com/urnetwork/operator-core"
	"github.com/urnetwork/server"
)

// server's Postgres as an operator.Db. Safe for concurrent use: it holds nothing, and server's pools are
// the process's.
type Db struct{}

var _ operator.Db = (*Db)(nil)

// The db of this process's pools.
func NewDb() *Db {
	return &Db{}
}

// Runs callback through server.Db.
func (self *Db) Db(ctx context.Context, callback func(conn operator.DbConn) error, options ...operator.DbOption) (returnErr error) {
	serverOptions := dbServerOptions(ctx, options, false)
	defer recoverDbError(&returnErr)
	server.Db(ctx, func(conn server.PgConn) {
		raiseCallbackError(callback(&dbConn{conn: conn}))
	}, serverOptions...)
	return nil
}

// Runs callback through server.Tx.
func (self *Db) Tx(ctx context.Context, callback func(tx operator.DbTx) error, options ...operator.DbOption) (returnErr error) {
	serverOptions := dbServerOptions(ctx, options, true)
	defer recoverDbError(&returnErr)
	server.Tx(ctx, func(tx server.PgTx) {
		raiseCallbackError(callback(&dbTx{tx: tx}))
	}, serverOptions...)
	return nil
}

// server's options for operator-core's, in order, so a later option of a kind stands as in server.
func dbServerOptions(ctx context.Context, options []operator.DbOption, tx bool) []any {
	serverOptions := []any{}
	for _, option := range options {
		switch v := option.(type) {
		case operator.DbIsolation:
			switch v {
			case operator.DbIsolationReadCommitted:
				serverOptions = append(serverOptions, pgx.ReadCommitted)
			case operator.DbIsolationRepeatableRead:
				serverOptions = append(serverOptions, pgx.RepeatableRead)
			case operator.DbIsolationSerializable:
				serverOptions = append(serverOptions, pgx.Serializable)
			}
		case operator.DbAccess:
			switch {
			case v == operator.DbReadWrite && tx:
				serverOptions = append(serverOptions, pgx.ReadWrite)
			case v == operator.DbReadWrite:
				serverOptions = append(serverOptions, server.OptReadWrite())
			case v == operator.DbReadOnly && tx:
				serverOptions = append(serverOptions, pgx.ReadOnly)
			case v == operator.DbReadOnly:
				serverOptions = append(serverOptions, server.OptReadOnly())
			}
		case operator.DbRetry:
			switch v {
			case operator.DbRetryTransient:
				serverOptions = append(serverOptions, server.OptRetryDefault())
			case operator.DbRetryNone:
				serverOptions = append(serverOptions, server.OptNoRetry())
			}
		case operator.DbReadBeforeBegin:
			if v != nil {
				read := v
				serverOptions = append(serverOptions, server.TxReadBeforeBegin(func(querier server.PgCanQuery) {
					raiseCallbackError(read(ctx, &dbReadQuerier{querier: querier}))
				}))
			}
		case operator.DbLockTimeout:
			serverOptions = append(serverOptions, server.TxLockTimeout(v))
		}
	}
	return serverOptions
}

// A callback's ErrDbRerun, raised so server's taxonomy reruns it as it reruns server.PgRetry, and still
// matching ErrDbRerun once the reruns are spent.
type dbRerunError struct {
	err error
}

// The callback's error's words.
func (self *dbRerunError) Error() string {
	return self.err.Error()
}

// The callback's error, and server.PgRetry for server's taxonomy.
func (self *dbRerunError) Unwrap() []error {
	return []error{self.err, &server.PgRetry{}}
}

// Raises a callback's error for server's helpers, which classify what they recover.
func raiseCallbackError(err error) {
	if err == nil {
		return
	}
	if errors.Is(err, operator.ErrDbRerun) {
		panic(&dbRerunError{err: err})
	}
	panic(err)
}

// The error a server helper raised, as the call's error: a lock bound's 55P03 wrapped with
// ErrDbLockTimeout. A runtime panic, or a panic that is no error, panics on.
func recoverDbError(returnErr *error) {
	recovered := recover()
	if recovered == nil {
		return
	}
	err, ok := recovered.(error)
	if !ok {
		panic(recovered)
	}
	var runtimeErr runtime.Error
	if errors.As(err, &runtimeErr) {
		panic(recovered)
	}
	if server.IsLockTimeout(err) {
		err = fmt.Errorf("%w: %w", operator.ErrDbLockTimeout, err)
	}
	*returnErr = err
}

// What a server helper raised inside run, as an error, for a method that answers one.
func catchRaised(run func()) (returnErr error) {
	defer recoverDbError(&returnErr)
	run()
	return nil
}

// A statement's args for pgx: one DbNamedArgs alone becomes pgx's named args.
func pgArgs(args []any) []any {
	if len(args) == 1 {
		if namedArgs, ok := args[0].(operator.DbNamedArgs); ok {
			return []any{pgx.NamedArgs(namedArgs)}
		}
	}
	return args
}

// operator-core's queued statements as pgx's.
func pgBatch(batch *operator.DbBatch) *pgx.Batch {
	queued := &pgx.Batch{}
	for index := range batch.Len() {
		sql, args := batch.Statement(index)
		queued.Queue(sql, pgArgs(args)...)
	}
	return queued
}

// Reads a sent batch's results with read, then closes them: the first error stands.
func readBatch(results pgx.BatchResults, read func(results operator.DbBatchResults) error) error {
	var readErr error
	if read != nil {
		readErr = read(&dbBatchResults{results: results})
	}
	closeErr := results.Close()
	if readErr != nil {
		return readErr
	}
	return closeErr
}

// What server's pooled connection runs, inside a Db callback.
type dbConn struct {
	conn server.PgConn
}

// The connection's query, its args as pgx takes them.
func (self *dbConn) Query(ctx context.Context, sql string, args ...any) (operator.DbRows, error) {
	rows, err := self.conn.Query(ctx, sql, pgArgs(args)...)
	if err != nil {
		return nil, err
	}
	return &dbRows{rows: rows}, nil
}

// The connection's exec, with the rows it touched.
func (self *dbConn) Exec(ctx context.Context, sql string, args ...any) (operator.DbResult, error) {
	tag, err := self.conn.Exec(ctx, sql, pgArgs(args)...)
	return operator.DbResult{RowsAffected: tag.RowsAffected()}, err
}

// The batch in one round trip on the connection.
func (self *dbConn) Batch(ctx context.Context, batch *operator.DbBatch, read func(results operator.DbBatchResults) error) error {
	return readBatch(self.conn.SendBatch(ctx, pgBatch(batch)), read)
}

// What server's transaction runs, inside a Tx callback.
type dbTx struct {
	tx server.PgTx
}

// The transaction's query, its args as pgx takes them.
func (self *dbTx) Query(ctx context.Context, sql string, args ...any) (operator.DbRows, error) {
	rows, err := self.tx.Query(ctx, sql, pgArgs(args)...)
	if err != nil {
		return nil, err
	}
	return &dbRows{rows: rows}, nil
}

// The transaction's exec, with the rows it touched.
func (self *dbTx) Exec(ctx context.Context, sql string, args ...any) (operator.DbResult, error) {
	tag, err := self.tx.Exec(ctx, sql, pgArgs(args)...)
	return operator.DbResult{RowsAffected: tag.RowsAffected()}, err
}

// The batch in one round trip in the transaction.
func (self *dbTx) Batch(ctx context.Context, batch *operator.DbBatch, read func(results operator.DbBatchResults) error) error {
	return readBatch(self.tx.SendBatch(ctx, pgBatch(batch)), read)
}

// server.CreateTempTableInTx; a value implementing operator.DbComplexValue fills several columns, as
// server.ComplexValue's method set is the same.
func (self *dbTx) CreateTempTable(ctx context.Context, spec string, values ...any) error {
	return catchRaised(func() {
		server.CreateTempTableInTx(ctx, self.tx, spec, values...)
	})
}

// server.CreateTempTableInTxAllowDuplicates.
func (self *dbTx) CreateTempTableAllowDuplicates(ctx context.Context, spec string, values ...any) error {
	return catchRaised(func() {
		server.CreateTempTableInTxAllowDuplicates(ctx, self.tx, spec, values...)
	})
}

// server.CreateTempJoinTableInTx.
func (self *dbTx) CreateTempJoinTable(ctx context.Context, spec string, keyValues map[any]any) error {
	return catchRaised(func() {
		server.CreateTempJoinTableInTx(ctx, self.tx, spec, keyValues)
	})
}

// A read before begin's querier: it queries, and refuses anything that could write, as the read must
// not.
type dbReadQuerier struct {
	querier server.PgCanQuery
}

// The read's query.
func (self *dbReadQuerier) Query(ctx context.Context, sql string, args ...any) (operator.DbRows, error) {
	rows, err := self.querier.Query(ctx, sql, pgArgs(args)...)
	if err != nil {
		return nil, err
	}
	return &dbRows{rows: rows}, nil
}

// Refused: a read before begin writes nothing.
func (self *dbReadQuerier) Exec(ctx context.Context, sql string, args ...any) (operator.DbResult, error) {
	return operator.DbResult{}, fmt.Errorf("a read before begin only queries")
}

// Refused: a read before begin writes nothing.
func (self *dbReadQuerier) Batch(ctx context.Context, batch *operator.DbBatch, read func(results operator.DbBatchResults) error) error {
	return fmt.Errorf("a read before begin only queries")
}

// pgx's rows as operator.DbRows.
type dbRows struct {
	rows pgx.Rows
}

// pgx's Next.
func (self *dbRows) Next() bool {
	return self.rows.Next()
}

// pgx's Scan, which takes Id and *Id through server's codec.
func (self *dbRows) Scan(dest ...any) error {
	return self.rows.Scan(dest...)
}

// pgx's Err.
func (self *dbRows) Err() error {
	return self.rows.Err()
}

// pgx's Close.
func (self *dbRows) Close() {
	self.rows.Close()
}

// pgx's batch results as operator.DbBatchResults.
type dbBatchResults struct {
	results pgx.BatchResults
}

// The next statement's exec.
func (self *dbBatchResults) Exec() (operator.DbResult, error) {
	tag, err := self.results.Exec()
	return operator.DbResult{RowsAffected: tag.RowsAffected()}, err
}

// The next statement's rows.
func (self *dbBatchResults) Query() (operator.DbRows, error) {
	rows, err := self.results.Query()
	if err != nil {
		return nil, err
	}
	return &dbRows{rows: rows}, nil
}
