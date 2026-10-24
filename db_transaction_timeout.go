package server

import "context"

// Runs callback with transaction_timeout off for conn's session, for one
// operation that can legitimately run longer than the application role's
// bound (an index build or a validation scan of a large table), and resets it
// before the connection can return to its pool. conn must be a direct
// maintenance session: a session SET through the transaction pooler would
// reach other clients.
//
// The minimal version from feat/pm-events (PARTITION-MAINTENANCE-PLAN.md C4);
// feat/pm-horizon owns this helper and its version replaces this one.
func WithoutTransactionTimeout(ctx context.Context, conn PgConn, callback func()) {
	RaisePgResult(conn.Exec(ctx, `SET transaction_timeout = 0`))
	defer conn.Exec(context.WithoutCancel(ctx), `RESET transaction_timeout`)
	callback()
}
