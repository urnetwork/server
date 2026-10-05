// Close-report lock fixtures retain a bounded cleanup owner after cancellation.
package model

import (
	"context"
	"time"

	"github.com/urnetwork/server"
)

// Releasing a synthetic lock cannot inherit its canceled test owner or wait
// forever. As in server.Tx, cleanup leaves the original outcome authoritative;
// a canceled connection can already be closed before rollback is attempted.
func rollbackCloseReportTestTransaction(ctx context.Context, tx server.PgTx) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	_ = tx.Rollback(cleanupCtx)
}
