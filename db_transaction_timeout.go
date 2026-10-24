// The per-operation exemption from the application role's transaction_timeout
// (PARTITION-MAINTENANCE-PLAN.md A6). This is the minimal version a branch
// needing the exemption creates; feat/pm-horizon owns the full one.
package server

import (
	"context"
)

// Runs callback with transaction_timeout off for conn's session, so a statement
// that may legitimately run past the role's bound keeps its session. The SET is
// session-wide, so conn must be a direct maintenance session (never one behind
// PgBouncer), and the setting is reset when callback returns or raises.
func WithoutTransactionTimeout(ctx context.Context, conn PgConn, callback func()) {
	RaisePgResult(conn.Exec(ctx, `SET transaction_timeout = 0`))
	defer func() {
		RaisePgResult(conn.Exec(ctx, `RESET transaction_timeout`))
	}()
	callback()
}
