package model

import (
	"context"
	"strconv"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

// The data leaderboard ranks each network's provided bytes over the newest
// payout windows: "refreshed every payout" (/stats/leaderboard). Before the
// provider earning boundary a window is a payout plan's subsidy epoch
// (subsidy_payment). Past the boundary no plan forms an epoch
// (operator_block_points.go), so with subsidy epochs alone the board, the
// network ranking and the epoch earnings email's rank froze at the plans of
// 2026-10-11. Since then a window is a completed operator block whose points
// were awarded: the award stores one payment per provider network with its
// bytes in the block under the block's plan id (OperatorBlockPointsPlanId),
// so the block counts as a plan did.
const leaderboardPayoutWindowCount = 4

// The candidate operator block windows for leaderboardPayoutWindowsSql: the
// plan ids and numbers of blocks 1 through the block containing now. Only the
// blocks with an award (a payment_report row) are windows.
func leaderboardOperatorBlocks(now time.Time) (planIds []server.Id, blocks []int64) {
	last := int64(SubnetBlockNumber(now))
	planIds = make([]server.Id, 0, last)
	blocks = make([]int64, 0, last)
	for block := int64(1); block <= last; block += 1 {
		planIds = append(planIds, OperatorBlockPointsPlanId(block))
		blocks = append(blocks, block)
	}
	return
}

// The plan ids of the newest leaderboardPayoutWindowCount payout windows,
// subsidy epochs and awarded operator blocks together, ordered by when each
// window ends (a block ends at genesis + block * duration). Its parameters are
// leaderboardPayoutWindowsArgs.
var leaderboardPayoutWindowsSql = `
	SELECT payout_window.payment_plan_id
	FROM (
		SELECT subsidy_payment.payment_plan_id, subsidy_payment.end_time
		FROM subsidy_payment

		UNION ALL

		SELECT
			operator_block.payment_plan_id,
			$3::timestamp + (operator_block.block_number * $4::bigint) * interval '1 second' AS end_time
		FROM unnest($1::uuid[], $2::bigint[]) AS operator_block(payment_plan_id, block_number)
		INNER JOIN payment_report ON payment_report.payment_plan_id = operator_block.payment_plan_id
	) payout_window
	ORDER BY payout_window.end_time DESC
	LIMIT ` + strconv.Itoa(leaderboardPayoutWindowCount) + `
`

func leaderboardPayoutWindowsArgs(now time.Time) []any {
	planIds, blocks := leaderboardOperatorBlocks(now)
	return []any{
		planIds,
		blocks,
		SubnetBlockGenesis,
		int64(SubnetBlockDuration / time.Second),
	}
}

type Earner struct {
	NetworkId         string  `json:"network_id"`
	NetworkName       string  `json:"network_name"`
	NetMiBCount       float32 `json:"net_mib_count"`
	IsPublic          bool    `json:"is_public"`
	ContainsProfanity bool    `json:"contains_profanity"` // network name contains profanity
}

type LeaderboardResult struct {
	Earners []Earner `json:"earners"`
	// Rank is the caller network's leaderboard position (1 = top earner), 0
	// when the network has no ranked payouts; Total is the number of ranked
	// networks. Both come from the same ranking the leaderboard is cut from.
	Rank  int              `json:"rank"`
	Total int              `json:"total"`
	Error *TopEarnersError `json:"error,omitempty"`
}

type TopEarnersError struct {
	Message string `json:"message"`
}

/**
 * Gets an ordered list of the top earners
 */
func GetLeaderboard(ctx context.Context) (earners []Earner, queryErr error) {
	// must be non-nil: a nil slice marshals as JSON `null`, and the gomobile
	// sdk binds this field as a pointer, so `null` becomes a nil object that
	// the android client dereferences inside a jni callback -- the NPE cannot
	// cross jni and ART aborts the whole process. An empty leaderboard is `[]`.
	earners = []Earner{}

	// stats read: tolerates replica delay
	server.ReplicaDb(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
						t.network_id,
						t.net_mib_count,
						network.network_name,
						network.leaderboard_public,
						network.contains_profanity

				FROM (SELECT account_payment.network_id,
										SUM(account_payment.payout_byte_count) / (1024 * 1024) AS net_mib_count
							FROM account_payment

							INNER JOIN (`+leaderboardPayoutWindowsSql+`) t ON t.payment_plan_id = account_payment.payment_plan_id

							-- a corrected legacy payment counts once, as its replacement
							WHERE NOT `+legacyProviderPaymentReplacedSql("account_payment.payment_id")+`

							GROUP BY account_payment.network_id
							ORDER BY net_mib_count DESC
							LIMIT 100
				) t

				INNER JOIN network ON network.network_id = t.network_id
				ORDER BY t.net_mib_count DESC
				;
		`,
			leaderboardPayoutWindowsArgs(server.NowUtc())...,
		)

		server.WithPgResult(result, err, func() {

			if err != nil {
				queryErr = err
				return
			}

			for result.Next() {
				var earner Earner
				server.Raise(result.Scan(
					&earner.NetworkId,
					&earner.NetMiBCount,
					&earner.NetworkName,
					&earner.IsPublic,
					&earner.ContainsProfanity,
				))

				if !earner.IsPublic {
					earner.NetworkId = ""
					earner.NetworkName = ""
				}

				earners = append(earners, earner)
			}
		})

	})

	return earners, queryErr

}

type NetworkRanking struct {
	NetMiBCount       float32 `json:"net_mib_count"`
	LeaderboardRank   int     `json:"leaderboard_rank"`
	LeaderboardPublic bool    `json:"leaderboard_public"`
}

// GetNetworkLeaderboardRankings computes the full network_id -> ranking map: the
// row_number() ordering over every network's payout. This is O(all networks)
// (Postgres evaluates the window over all networks regardless of any per-network
// filter), so the controller caches it -- see GetNetworkLeaderboardRanking in
// controller/leaderboard_controller.go. This function itself is always fresh.
func GetNetworkLeaderboardRankings(ctx context.Context) (map[server.Id]NetworkRanking, error) {
	rankings := map[server.Id]NetworkRanking{}
	var queryErr error
	// stats read: tolerates replica delay
	server.ReplicaDb(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT t.network_id, t.net_mib_count, t.leaderboard_rank, network.leaderboard_public
			FROM (
				SELECT account_payment.network_id,
					sum(account_payment.payout_byte_count) / (1024 * 1024) AS net_mib_count,
					row_number() OVER (ORDER BY sum(account_payment.payout_byte_count) DESC) AS leaderboard_rank
				FROM account_payment
				INNER JOIN (`+leaderboardPayoutWindowsSql+`) t ON t.payment_plan_id = account_payment.payment_plan_id
				-- a corrected legacy payment counts once, as its replacement
				WHERE NOT `+legacyProviderPaymentReplacedSql("account_payment.payment_id")+`
				GROUP BY account_payment.network_id
			) t
			INNER JOIN network ON network.network_id = t.network_id
			`,
			leaderboardPayoutWindowsArgs(server.NowUtc())...,
		)
		server.WithPgResult(result, err, func() {
			if err != nil {
				queryErr = err
				return
			}
			for result.Next() {
				var networkId server.Id
				var ranking NetworkRanking
				server.Raise(result.Scan(
					&networkId,
					&ranking.NetMiBCount,
					&ranking.LeaderboardRank,
					&ranking.LeaderboardPublic,
				))
				rankings[networkId] = ranking
			}
		})
	})
	if queryErr != nil {
		return nil, queryErr
	}
	return rankings, nil
}

/**
 * Gets the ranking for the session network
 */
func GetNetworkLeaderboardRanking(session *session.ClientSession) (networkRanking NetworkRanking, queryErr error) {
	rankings, err := GetNetworkLeaderboardRankings(session.Ctx)
	if err != nil {
		return NetworkRanking{}, err
	}
	// a network with no payouts is simply absent -> zero ranking
	return rankings[session.ByJwt.NetworkId], nil
}

/**
 * A network can opt into having their network displayed on the leaderboard
 */
func SetNetworkLeaderboardPublic(isPublic bool, session *session.ClientSession) (err error) {
	server.Tx(session.Ctx, func(tx server.PgTx) {
		_ = server.RaisePgResult(tx.Exec(
			session.Ctx,
			`
				UPDATE network
				SET leaderboard_public = $1
				WHERE network_id = $2;
		`,
			isPublic,
			session.ByJwt.NetworkId,
		))
	})

	return err
}
