// The exemption from the application role's transaction_timeout
// (PARTITION-MAINTENANCE-PLAN.md A6) for one operation that can legitimately
// run longer, such as REINDEX CONCURRENTLY. This is the minimal version that
// branches needing the exemption share; feat/pm-horizon owns the
// implementation.
package server

import (
	"context"
)

// Runs callback with transaction_timeout off on conn, a direct maintenance
// session, and resets it afterwards, also when callback panics or the caller
// has stopped. A closed session keeps no setting, so it is not reset.
func WithoutTransactionTimeout(ctx context.Context, conn PgConn, callback func()) {
	RaisePgResult(conn.Exec(ctx, `SET transaction_timeout = 0`))
	defer func() {
		if conn.Conn().IsClosed() {
			return
		}
		resetCtx, resetCancel := context.WithTimeout(context.WithoutCancel(ctx), PgCloseTimeout)
		defer resetCancel()
		RaisePgResult(conn.Exec(resetCtx, `RESET transaction_timeout`))
	}()
	callback()
}
