package model

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
)

// This census keeps its caller deadline and statement snapshot. JIT and the
// server statement cap belong only to this read-only transaction; rollback
// restores both before reuse. The server cap also retires the query snapshot
// if the client's separate cancellation connection cannot reach PostgreSQL.
// Cancellation of the caller can still precede that server cap.
func providerUrlProbeFleetRead(ctx context.Context, conn server.PgConn, read func(server.PgTx)) {
	server.Raise(ctx.Err())
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly})
	server.Raise(err)
	completed := false
	defer func() {
		rollbackErr := tx.Rollback(ctx)
		if completed {
			server.Raise(rollbackErr)
		}
	}()
	server.Raise(ctx.Err())
	if deadline, bounded := ctx.Deadline(); bounded {
		// PostgreSQL accepts integer milliseconds. Round down and refuse an
		// exhausted/sub-millisecond budget rather than accidentally setting 0
		// (which disables the server timeout). Preserve a tighter role/session
		// setting. This is a statement budget measured when configured, not a
		// claim that asynchronous server cancellation is instantaneous.
		millis := time.Until(deadline).Milliseconds()
		if millis < 1 {
			server.Raise(ctx.Err())
			panic(context.DeadlineExceeded)
		}
		millis = min(millis, (1<<31)-1)
		server.RaisePgResult(tx.Exec(ctx, `SELECT set_config('jit','off',true),
			set_config('statement_timeout', CASE
				WHEN current_setting('statement_timeout')::interval > interval '0'
				THEN LEAST((EXTRACT(EPOCH FROM current_setting('statement_timeout')::interval)*1000)::bigint,$1::bigint)::text
				ELSE $1::bigint::text END,true)`, millis))
	} else {
		server.RaisePgResult(tx.Exec(ctx, "SET LOCAL jit=off"))
	}
	server.Raise(ctx.Err())
	read(tx)
	server.Raise(ctx.Err())
	completed = true
}
