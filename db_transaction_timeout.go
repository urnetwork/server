// The application role's transaction_timeout (PARTITION-MAINTENANCE-PLAN.md
// A6) ends any transaction that outlives its bound, so an operation that may
// legitimately run longer, such as an online index build of a large table,
// lifts it for its own maintenance session only.
//
// Minimal version for feat/pm-identity; feat/pm-horizon owns this helper and
// its version replaces this one at merge.
package server

import "context"

// Runs callback with the session's transaction_timeout off, then restores the
// role default. conn must be a direct maintenance session (MaintenanceDb),
// never a transaction-pooled connection, since the setting is session-wide. A
// session the reset cannot reach is closed rather than returned to the pool
// without its bound.
func WithoutTransactionTimeout(ctx context.Context, conn PgConn, callback func()) {
	RaisePgResult(conn.Exec(ctx, `SET transaction_timeout = 0`))
	defer func() {
		resetCtx, resetCancel := context.WithTimeout(context.WithoutCancel(ctx), PgCloseTimeout)
		defer resetCancel()
		if _, err := conn.Exec(resetCtx, `RESET transaction_timeout`); err != nil {
			closePgConnection(resetCtx, conn.Conn())
		}
	}()
	callback()
}
