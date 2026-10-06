package model

import (
	"testing"

	"github.com/urnetwork/server/task"
)

func TestLegacySettlementDensePageDoesNotJoinColdMirror(t *testing.T) {
	testLegacySettlementDensePageDoesNotJoinColdMirror(t,
		task.NewTaskTargetWithPost(ApplyLegacyNetEscrowMirror, ApplyLegacyNetEscrowMirrorPost))
}
