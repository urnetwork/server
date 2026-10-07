package model

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
)

// This single census read keeps its existing caller deadline and statement
// snapshot. JIT is disabled only while this read-only transaction owns its
// backend; rollback restores the setting before the connection is reused.
// Cancellation also cancels rollback, which makes pgx discard an unclean
// connection instead of returning a transaction-local setting to the pool.
func providerUrlProbeFleetRead(ctx context.Context, conn server.PgConn, read func(server.PgTx)) {
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly})
	server.Raise(err)
	completed := false
	defer func() {
		rollbackErr := tx.Rollback(ctx)
		if completed {
			server.Raise(rollbackErr)
		}
	}()
	server.RaisePgResult(tx.Exec(ctx, "SET LOCAL jit=off"))
	read(tx)
	server.Raise(ctx.Err())
	completed = true
}
