// Lifts or replaces the application role's transaction_timeout for one
// transaction (PARTITION-MAINTENANCE-PLAN.md A6).
//
// Migration 889 gives the application roles a 15-minute transaction_timeout
// default, and PostgreSQL ends the session (FATAL 25P04) once a transaction
// runs past it. This release must keep working when the migration is applied
// before the next release serves, so each long transaction it runs lifts the
// bound for itself, as the next release does. Before the migration the bound
// is unset and these statements change nothing.
//
// PostgreSQL 18 arms the timer when a transaction starts, with the value in
// effect then, and disarms it at commit or abort. Inside a transaction the
// setting's assign hook disarms a running timer when the value drops to zero,
// arms a new one counted from that moment when a positive value is set while
// none runs, and leaves a running timer alone otherwise. So `SET LOCAL
// transaction_timeout = 0` as the first statement of a transaction exempts that
// whole transaction. SET LOCAL ends with the transaction, so this is safe on
// the main pool's transaction pooler too.
package server

import (
	"context"
	"fmt"
	"time"
)

// PgCanExec is satisfied by PgConn, PgTx and any other value that can execute
// a statement.
type PgCanExec interface {
	Exec(ctx context.Context, sql string, arguments ...any) (PgTag, error)
}

// Lifts the role's transaction bound for the rest of a transaction.
const liftTransactionTimeoutInTxSql = `SET LOCAL transaction_timeout = 0`

// The statement that replaces the transaction's bound: zero lifts it, and a
// positive bound is armed afresh after the running timer is disarmed, since
// PostgreSQL leaves a running timer alone when a positive value is set.
func transactionTimeoutInTxSql(bound time.Duration) string {
	if bound <= 0 {
		return liftTransactionTimeoutInTxSql
	}
	return fmt.Sprintf(`SET LOCAL transaction_timeout = 0; SET LOCAL transaction_timeout = %d`, max(int64(1), bound.Milliseconds()))
}

// Replaces the role's transaction_timeout for the rest of the caller's
// transaction with bound, counted from this call; a non-positive bound lifts
// it. Called first in the transaction: the role's bound counts from BEGIN.
func SetTransactionTimeoutInTx(ctx context.Context, tx PgCanExec, bound time.Duration) {
	RaisePgResult(tx.Exec(ctx, transactionTimeoutInTxSql(bound)))
}
