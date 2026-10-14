package controller

// st_ledger_v2_readers.go — the earnings readers of settlement v2 epochs
// (VERSION2-FINAL.md §8.4). The earnings email and GET /account/epochs take a
// v2 epoch's amounts from the ledger, per covered epoch, where a v1 epoch's
// come from its leaf shares. The email and the leaderboard rebuild fire when a
// finalized v2 root's amounts are recorded, not when v1 finalizes the epoch.

import (
	"cmp"
	"context"
	"math/big"
	"slices"

	"github.com/holiman/uint256"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// stLedgerV2Finalized runs the readers' finalization hooks for a finalized v2
// root whose amounts were just recorded: the leaderboard rebuild, and the
// earnings email of each epoch the root covers.
func stLedgerV2Finalized(ctx context.Context, deploymentKey model.StDeploymentKey, epochs []*StLedgerV2CoveredEpoch) {
	TriggerRebuildPointsLeaderboard(ctx)
	for _, covered := range epochs {
		StNotifyEpochEarnings(ctx, deploymentKey, covered.Epoch)
	}
}

// stLedgerV2Epoch reports whether settlement v2 recorded the epoch's provider
// rows for the pool, which makes it a v2 epoch for the readers.
func stLedgerV2Epoch(ctx context.Context, deploymentKey model.StDeploymentKey, noId uint64, epoch uint64) bool {
	return stConfig().LedgerV2Enabled() && len(model.GetStLedgerV2EpochRows(ctx, deploymentKey, noId, epoch, epoch)) != 0
}

// stLedgerV2ShareBps is amount's share of total in basis points, rounded down.
func stLedgerV2ShareBps(amount *uint256.Int, total *uint256.Int) int {
	if total.IsZero() {
		return 0
	}
	share := new(uint256.Int).Mul(amount, uint256.NewInt(10_000))
	return int(share.Div(share, total).Uint64())
}

// stEpochNetworkEarnings returns each network's share of an epoch's pool in
// basis points, and the alpha in rao that the epoch gave it. A v2 epoch's
// both come from its recorded amounts. A v1 epoch's share comes from its
// committed leaves, and its alpha is the pool total times the share, absent
// when the pool total cannot be read.
func stEpochNetworkEarnings(ctx context.Context, deploymentKey model.StDeploymentKey, epoch uint64, noId uint64) (map[server.Id]int, map[server.Id]*big.Int) {
	unclaimed := map[server.Id]*big.Int{}
	if stLedgerV2Epoch(ctx, deploymentKey, noId, epoch) {
		amounts, total := model.GetStLedgerV2NetworkAmounts(ctx, deploymentKey, noId, epoch)
		shares := map[server.Id]int{}
		for networkId, amount := range amounts {
			shares[networkId] = stLedgerV2ShareBps(&amount, &total)
			unclaimed[networkId] = amount.ToBig()
		}
		return shares, unclaimed
	}
	shares := model.GetStPayoutNetworkShares(ctx, deploymentKey, epoch, noId)
	if poolTotal := stEpochPoolTotal(ctx, epoch, noId); poolTotal != nil {
		for networkId, share := range shares {
			if 0 < share {
				amount := new(big.Int).Mul(poolTotal, big.NewInt(int64(share)))
				unclaimed[networkId] = amount.Div(amount, big.NewInt(10_000))
			}
		}
	}
	return shares, unclaimed
}

// stLedgerV2AccountEpochs places the pool's v2 epochs that finalized roots
// cover among the v1 finalized epoch rows, newest first and at most limit,
// leaving out a v1 row of an epoch that v2 settles. It returns the network's
// amount and the epoch's total for each v2 epoch, zero without amounts.
func stLedgerV2AccountEpochs(ctx context.Context, cfg *StConfig, networkId server.Id, rows []*model.StEpoch, limit int) ([]*model.StEpoch, map[uint64]*model.StLedgerV2EpochAmount) {
	amounts := map[uint64]*model.StLedgerV2EpochAmount{}
	if !cfg.LedgerV2Enabled() {
		return rows, amounts
	}
	deploymentKey := cfg.DeploymentKey()
	merged := model.GetStLedgerV2FinalizedEpochs(ctx, deploymentKey, cfg.NoId, limit)
	epochs := make([]uint64, len(merged))
	for i, row := range merged {
		epochs[i] = row.Epoch
	}
	if len(rows) != 0 {
		settled := model.GetStLedgerV2EpochRows(ctx, deploymentKey, cfg.NoId, rows[len(rows)-1].Epoch, rows[0].Epoch)
		for _, row := range rows {
			if settled[row.Epoch] == nil {
				merged = append(merged, row)
			}
		}
	}
	slices.SortFunc(merged, func(a *model.StEpoch, b *model.StEpoch) int {
		return cmp.Compare(b.Epoch, a.Epoch)
	})
	recorded := model.GetStLedgerV2AmountsForNetwork(ctx, deploymentKey, networkId, cfg.NoId, epochs)
	for _, epoch := range epochs {
		amount := recorded[epoch]
		if amount == nil {
			amount = &model.StLedgerV2EpochAmount{}
		}
		amounts[epoch] = amount
	}
	return merged[:min(limit, len(merged))], amounts
}
