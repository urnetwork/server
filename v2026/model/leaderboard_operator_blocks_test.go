package model

import (
	"testing"
	"time"
)

// Pure: the candidate block windows are blocks 1 through the open block, under
// the plan ids their awards are written with.
func TestLeaderboardOperatorBlocksAreTheAwardPlans(t *testing.T) {
	now := operatorBlockStart(20).Add(3 * 24 * time.Hour)
	planIds, blocks := leaderboardOperatorBlocks(now)
	if len(planIds) != 20 || len(blocks) != 20 {
		t.Fatalf("%d plan ids and %d blocks, want 20", len(planIds), len(blocks))
	}
	for i, block := range blocks {
		if block != int64(i+1) || planIds[i] != OperatorBlockPointsPlanId(block) {
			t.Fatalf("window %d: block %d plan %s", i, block, planIds[i])
		}
	}
	if planIds, blocks := leaderboardOperatorBlocks(SubnetBlockGenesis.Add(-time.Hour)); len(planIds) != 1 || blocks[0] != 1 {
		t.Fatalf("before genesis: %v %v", planIds, blocks)
	}
}
