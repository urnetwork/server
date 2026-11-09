package sidecart3

// T3's half of the oracle's store view (TEST-M2-01): the Postgres a T3 Net's oracle reads through fault's
// StoreView, which T2 has none of. NetSettings sets it on every T3 Net, so an invariant reads server's tables the
// way it reads the clusters, in a read-only transaction the database enforces: an oracle writes nothing. Every other
// store the view reads is the Net's host, the same wrappers T2's fakes stand beside, under InspectContext, so a
// look takes no plan on T3 either.

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
)

// server's Postgres, read-only, for an oracle.
type postgresReader struct{}

// Server's Postgres as the oracle reads it.
func PostgresReader() sidecartest.PostgresReader {
	return &postgresReader{}
}

// One query's rows in a read-only transaction, each row its columns' values. A query that would write fails in
// the database.
func (self *postgresReader) Rows(ctx context.Context, query string, args ...any) (rows [][]any, returnErr error) {
	defer func() {
		// server.Db raises a pool's failure as a panic
		if recovered := recover(); recovered != nil {
			returnErr = fmt.Errorf("the oracle's Postgres read: %v", recovered)
		}
	}()
	server.Db(ctx, func(conn server.PgConn) {
		tx, err := conn.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
		if err != nil {
			returnErr = err
			return
		}
		defer tx.Rollback(ctx)
		result, err := tx.Query(ctx, query, args...)
		if err != nil {
			returnErr = err
			return
		}
		defer result.Close()
		for result.Next() {
			values, err := result.Values()
			if err != nil {
				returnErr = err
				return
			}
			rows = append(rows, values)
		}
		returnErr = result.Err()
	})
	return rows, returnErr
}
