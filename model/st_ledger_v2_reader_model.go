package model

// st_ledger_v2_reader_model reads settlement v2's per-epoch amounts for the
// earnings surfaces (VERSION2-FINAL.md §8.4): the epochs that finalized roots
// cover, and each network's amount in them. A coldkey's amount in an epoch
// belongs to the network of the smallest client whose wallet resolution in
// that epoch selected the coldkey, as v1 gives a payout leaf to its smallest
// client's network. An amount that no resolution of its epoch selected belongs
// to no network and counts only in the epoch's total.

import (
	"context"

	"github.com/holiman/uint256"

	"github.com/urnetwork/server"
)

// StLedgerV2EpochAmount is a network's amount of one epoch and the epoch's
// total amount, in rao.
type StLedgerV2EpochAmount struct {
	Amount uint256.Int
	Total  uint256.Int
}

// The pool's recorded amounts of the epochs $3, each with the network its
// coldkey belongs to in its epoch, NULL for none.
const stLedgerV2AttributedAmounts = `
	st_ledger_v2_amount a
	LEFT JOIN (
		SELECT DISTINCT ON (epoch, coldkey) epoch, coldkey, network_id
		FROM st_payout_wallet_resolution
		WHERE deployment_key = $1 AND no_id = $2 AND epoch = ANY($3)
		ORDER BY epoch, coldkey, client_id
	) attribution ON attribution.epoch = a.epoch AND attribution.coldkey = a.coldkey
	WHERE a.deployment_key = $1 AND a.no_id = $2 AND a.epoch = ANY($3)
`

// GetStLedgerV2FinalizedEpochs returns, newest first and at most limit, the
// pool's epochs covered by a finalized root whose amounts are recorded, as
// their mirrored epoch rows, with zero blocks for an epoch never mirrored.
func GetStLedgerV2FinalizedEpochs(ctx context.Context, deploymentKey StDeploymentKey, noId uint64, limit int) []*StEpoch {
	key := requireStDeploymentKey(deploymentKey)
	epochs := []*StEpoch{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					r.epoch,
					coalesce(e.start_block, 0),
					coalesce(e.commit_deadline_block, 0),
					coalesce(e.trails_deadline_block, 0),
					coalesce(e.finalize_block, 0),
					coalesce(e.status, 'open'),
					e.finalized_time
				FROM st_ledger_v2_epoch_rows r
				LEFT JOIN st_epoch e ON e.deployment_key = r.deployment_key AND e.epoch = r.epoch
				WHERE r.deployment_key = $1 AND r.no_id = $2 AND r.epoch <= (
					SELECT max(epoch) FROM st_ledger_v2_root
					WHERE deployment_key = $1 AND no_id = $2 AND status = 'finalized' AND recorded_time IS NOT NULL
				)
				ORDER BY r.epoch DESC
				LIMIT $3
			`,
			key, int64(noId), limit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				epochs = append(epochs, scanStEpoch(result))
			}
		})
	})
	return epochs
}

// GetStLedgerV2AmountsForNetwork returns, for each of the epochs that has
// recorded amounts, the network's amount and the epoch's total.
func GetStLedgerV2AmountsForNetwork(ctx context.Context, deploymentKey StDeploymentKey, networkId server.Id, noId uint64, epochs []uint64) map[uint64]*StLedgerV2EpochAmount {
	key := requireStDeploymentKey(deploymentKey)
	amounts := map[uint64]*StLedgerV2EpochAmount{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT a.epoch, coalesce(sum(a.amount) FILTER (WHERE attribution.network_id = $4), 0)::text, sum(a.amount)::text
				FROM `+stLedgerV2AttributedAmounts+`
				GROUP BY a.epoch
			`,
			key, int64(noId), stLedgerV2Epochs(epochs), networkId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var epoch int64
				var amount, total string
				server.Raise(result.Scan(&epoch, &amount, &total))
				epochAmount := &StLedgerV2EpochAmount{}
				server.Raise(epochAmount.Amount.SetFromDecimal(amount))
				server.Raise(epochAmount.Total.SetFromDecimal(total))
				amounts[uint64(epoch)] = epochAmount
			}
		})
	})
	return amounts
}

// GetStLedgerV2NetworkAmounts returns an epoch's amount per network, for the
// networks with an amount, and the epoch's total.
func GetStLedgerV2NetworkAmounts(ctx context.Context, deploymentKey StDeploymentKey, noId uint64, epoch uint64) (map[server.Id]uint256.Int, uint256.Int) {
	key := requireStDeploymentKey(deploymentKey)
	amounts := map[server.Id]uint256.Int{}
	var total uint256.Int
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT attribution.network_id, sum(a.amount)::text
				FROM `+stLedgerV2AttributedAmounts+`
				GROUP BY attribution.network_id
			`,
			key, int64(noId), stLedgerV2Epochs([]uint64{epoch}),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var networkId *server.Id
				var sum string
				server.Raise(result.Scan(&networkId, &sum))
				var amount uint256.Int
				server.Raise(amount.SetFromDecimal(sum))
				total.Add(&total, &amount)
				if networkId != nil {
					amounts[*networkId] = amount
				}
			}
		})
	})
	return amounts, total
}

func stLedgerV2Epochs(epochs []uint64) []int64 {
	values := make([]int64, len(epochs))
	for i, epoch := range epochs {
		values[i] = int64(epoch)
	}
	return values
}
