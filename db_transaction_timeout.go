// Exempts one maintenance operation's session from the application role's
// transaction_timeout (PARTITION-MAINTENANCE-PLAN.md A6). This is the minimal
// version a branch other than feat/pm-horizon carries; that branch owns the
// implementation, and its version replaces this one at merge.
package server

import (
	"context"
)

// Runs callback with transaction_timeout off for conn's session, then resets
// the session's default. conn must be a session of the maintenance pool,
// idle outside a transaction; the callback may begin its own transactions on
// it. The reset runs on a context detached from ctx, so a canceled caller
// still restores the session.
func WithoutTransactionTimeout(ctx context.Context, conn PgConn, callback func()) {
	RaisePgResult(conn.Exec(ctx, `SET transaction_timeout = 0`))
	defer func() {
		resetCtx, resetCancel := context.WithTimeout(context.WithoutCancel(ctx), PgPingTimeout)
		defer resetCancel()
		if _, err := conn.Exec(resetCtx, `RESET transaction_timeout`); err != nil {
			// a session that keeps the exemption must not return to the pool
			closePgConnection(resetCtx, conn.Conn())
		}
	}()
	callback()
}
