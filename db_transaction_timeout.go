package server

import (
	"context"
)

// Runs callback on conn, a direct maintenance session, with transaction_timeout
// off for the session (PARTITION-MAINTENANCE-PLAN.md A6), and resets it before
// the connection goes back to the pool. For an operation that can
// legitimately run past the application role's bound, such as a long copy or
// a partition cutover's verification. The minimal version; feat/pm-horizon owns
// the full one.
func WithoutTransactionTimeout(ctx context.Context, conn PgConn, callback func()) {
	RaisePgResult(conn.Exec(ctx, `SET transaction_timeout = 0`))
	defer func() {
		RaisePgResult(conn.Exec(context.WithoutCancel(ctx), `RESET transaction_timeout`))
	}()
	callback()
}
