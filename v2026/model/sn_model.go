package model

// sn_model holds the subnet (sn) block clock — the application layer
// settlement calendar shared by the public stats collector
// (controller/stats_collector.go), the grafana public stats feed
// (warp repo, grafana/stats.go), and the sites.
//
// Blocks are the subnet's 7-day settlement periods, 1-based, and every
// block opens and closes at 00:00 UTC Sunday. Block 1 opened Sunday
// June 28 2026 — the Sunday of the launch week. The clock is duplicated
// in warp/grafana/stats.go (statsBlockGenesis) and ur.xyz
// react/src/lib/network.js (BLOCK_GENESIS_MS) — keep them in sync.
//
// The subnet block is unrelated to the warp deploy block label and to the
// ST contract epoch, which is anchored in chain blocks (see the epoch
// machine in controller/st_controller.go).

import (
	"time"
)

// SubnetBlockGenesis is the open time of block 1: 00:00 UTC Sunday
// June 28 2026.
var SubnetBlockGenesis = time.Date(2026, time.June, 28, 0, 0, 0, 0, time.UTC)

// SubnetBlockDuration is the length of one block.
const SubnetBlockDuration = 7 * 24 * time.Hour

// SubnetBlockStart returns the open time of the block containing now,
// pinned to the genesis before it.
func SubnetBlockStart(now time.Time) time.Time {
	elapsed := now.Sub(SubnetBlockGenesis)
	if elapsed < 0 {
		return SubnetBlockGenesis
	}
	return SubnetBlockGenesis.Add((elapsed / SubnetBlockDuration) * SubnetBlockDuration)
}

// SubnetBlockNumber returns the 1-based number of the block containing
// now.
func SubnetBlockNumber(now time.Time) int {
	elapsed := now.Sub(SubnetBlockGenesis)
	if elapsed < 0 {
		return 1
	}
	return int(elapsed/SubnetBlockDuration) + 1
}
