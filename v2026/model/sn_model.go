package model

// sn_model holds the subnet (sn) block clock — the application layer
// calendar of the points leaderboard: its payout periods
// (controller/points_leaderboard_controller.go), the account_payment_block
// rollup (points_leaderboard_rollup.go) and the points readiness signal
// (monitor/signal_points_readiness.go).
//
// Blocks are 7-day periods, 1-based, and every block opens and closes at
// 00:00 UTC Sunday. Block 1 opened Sunday June 28 2026 — the Sunday of the
// launch week.
//
// The public stats no longer use this clock: since 2026-10-15 the stats
// collector, and the grafana public stats feed that ur.xyz reads (warp repo,
// grafana/stats.go), window by the SN25 settlement epoch instead
// (controller/stats_epoch_clock.go). The subnet block is unrelated to the
// warp deploy block label and to the ST contract epoch, which is anchored in
// chain blocks (see the epoch machine in controller/st_controller.go).

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
