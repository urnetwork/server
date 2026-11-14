// Explicit recovery wakes the existing sweep owner.
package work

import (
	"context"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

// Counts acknowledge scheduling requests, not completed contracts or new rows.
// Repeated requests coalesce without replacing an active owner's scan state.
type ContractExpiryRecoveryResult struct {
	RequestedAt   time.Time `json:"requested_at"`
	SweepRequests int       `json:"sweep_requests"`
}

// The bounded publisher writes only the ordinary sweep RunOnce owner. The
// sweep keeps its fixed keyset passes and normal expiry policy. A request
// racing an active claim retains the earliest wake generation for its
// successor, including at EOF.
func QueueContractExpiryRecovery(ctx context.Context) (result ContractExpiryRecoveryResult, returnErr error) {
	keys := []server.PgOwnershipKey{task.RunOnceOwnershipKey(task.RunOnce("close_expired_contracts_1_0"))}
	server.HandleError(func() {
		server.OwnedTx(ctx, keys, func(tx server.PgTx) {
			owner := session.NewLocalClientSession(ctx, "", nil)
			defer owner.Cancel()
			result.RequestedAt = server.NowUtc()
			ScheduleCloseExpiredContracts(owner, tx, 0, false)
		}, server.TxReadCommitted, server.OptNoRetry())
	}, func(err error) { returnErr = err })
	if returnErr != nil {
		return ContractExpiryRecoveryResult{}, returnErr
	}
	result.SweepRequests = 1
	return
}
