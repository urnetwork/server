package model

import (
	"testing"

	"github.com/urnetwork/server/v2026/task"
)

func TestLegacySettlementDensePageDoesNotJoinColdMirror(t *testing.T) {
	testLegacySettlementDensePageDoesNotJoinColdMirror(t,
		task.NewTaskTargetWithPost(ApplyLegacyNetEscrowMirror, ApplyLegacyNetEscrowMirrorPost))
}
