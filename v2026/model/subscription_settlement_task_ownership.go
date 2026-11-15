// Financial task completion owns its queue identity before touching a pending
// row.
package model

import (
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/task"
)

type legacyNetEscrowMirrorTaskTarget struct{ task.Target }

func (self *legacyNetEscrowMirrorTaskTarget) TaskCompletionOwnershipKeys(_ *task.Task, _ string) ([]server.PgOwnershipKey, error) {
	return nil, nil
}

// Use the exact production target, including its same-key revision handoff.
func NewLegacyNetEscrowMirrorTaskTarget() task.Target {
	return &legacyNetEscrowMirrorTaskTarget{Target: task.NewTaskTargetWithPost(ApplyLegacyNetEscrowMirror, ApplyLegacyNetEscrowMirrorPost)}
}
