// The application role's transaction_timeout (PARTITION-MAINTENANCE-PLAN.md
// A6) ends any transaction that runs past it, so a maintenance operation that
// legitimately runs longer, such as a VALIDATE over a large table, lifts it
// for its own session only. Minimal version for branches that need the
// exemption before feat/pm-horizon, which owns this helper, is merged.
package server

import (
	"context"
)

// Runs callback with transaction_timeout off for conn, a maintenance session,
// and resets it before conn goes back to the pool.
func WithoutTransactionTimeout(ctx context.Context, conn PgConn, callback func()) {
	RaisePgResult(conn.Exec(ctx, `SET transaction_timeout = 0`))
	defer func() {
		// reset even when ctx is done, so the session leaves without the
		// exemption
		RaisePgResult(conn.Exec(context.WithoutCancel(ctx), `RESET transaction_timeout`))
	}()
	callback()
}
