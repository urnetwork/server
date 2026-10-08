// Financial task completion owns its queue identity before touching a pending
// row. Dispatcher publishers additionally declare their complete payer set.
package model

import (
	"fmt"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/task"
)

type legacyPayerSettlementTaskTarget struct{ task.Target }

func (self *legacyPayerSettlementTaskTarget) TaskCompletionOwnershipKeys(_ *task.Task, _ string) ([]server.PgOwnershipKey, error) {
	return nil, nil
}

type legacyNetEscrowMirrorTaskTarget struct{ task.Target }

func (self *legacyNetEscrowMirrorTaskTarget) TaskCompletionOwnershipKeys(_ *task.Task, _ string) ([]server.PgOwnershipKey, error) {
	return nil, nil
}

// Use the exact production target, including its same-key revision handoff.
func NewLegacyNetEscrowMirrorTaskTarget() task.Target {
	return &legacyNetEscrowMirrorTaskTarget{Target: task.NewTaskTargetWithPost(ApplyLegacyNetEscrowMirror, ApplyLegacyNetEscrowMirrorPost)}
}

// A dispatch result contains at most one bounded discovery round. The worker
// combines these keys with the dispatcher's own stored RunOnce identity.
func LegacyPayerSettlementQueueOwnershipKeys(payerIds []server.Id) ([]server.PgOwnershipKey, error) {
	if len(payerIds) > legacySettlementPayerProbeLimit {
		return nil, fmt.Errorf("legacy payer publication exceeds discovery bound")
	}
	keys := make([]server.PgOwnershipKey, 0, len(payerIds))
	for _, id := range payerIds {
		if id == (server.Id{}) {
			return nil, fmt.Errorf("legacy payer publication has empty scope")
		}
		keys = append(keys, task.RunOnceOwnershipKey(task.RunOnce("flush_legacy_payer_settlements", id)))
	}
	return keys, nil
}
