// Explicit recovery wakes the existing sweep and settlement owners together.
package work

import (
	"context"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

// Counts acknowledge scheduling requests, not completed contracts or new rows.
// Repeated requests coalesce without replacing an active owner's scan state.
type ContractExpiryRecoveryResult struct {
	RequestedAt              time.Time `json:"requested_at"`
	SweepRequests            int       `json:"sweep_requests"`
	LegacyDispatcherRequests int       `json:"legacy_dispatcher_requests"`
}

// The bounded publisher writes only the ordinary 17 RunOnce owners. The sweep
// keeps its fixed keyset passes and normal expiry policy; pre-existing intents
// remain with their source/payer dispatcher. A request racing an active claim
// retains the earliest wake generation for its successor, including at EOF.
func QueueContractExpiryRecovery(ctx context.Context) (result ContractExpiryRecoveryResult, returnErr error) {
	keys := legacySettlementStartupOwnershipKeys()
	keys = append(keys, task.RunOnceOwnershipKey(task.RunOnce("close_expired_contracts_1_0")))
	server.HandleError(func() {
		server.OwnedTx(ctx, keys, func(tx server.PgTx) {
			owner := session.NewLocalClientSession(ctx, "", nil)
			defer owner.Cancel()
			result.RequestedAt = server.NowUtc()
			ScheduleCloseExpiredContracts(owner, tx, 0, false)
			for shard := range model.LegacySettlementShardCount {
				scheduleFlushLegacySettlements(owner, tx, shard, nil, nil, true, nil, nil)
			}
		}, server.TxReadCommitted, server.OptNoRetry())
	}, func(err error) { returnErr = err })
	if returnErr != nil {
		return ContractExpiryRecoveryResult{}, returnErr
	}
	result.SweepRequests = 1
	result.LegacyDispatcherRequests = model.LegacySettlementShardCount
	return
}
