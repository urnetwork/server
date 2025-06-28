package model

import (
	"context"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

type Earner struct {
	NetworkId         string  `json:"network_id"`
	NetworkName       string  `json:"network_name"`
	NetMiBCount       float32 `json:"net_mib_count"`
	IsPublic          bool    `json:"is_public"`
	ContainsProfanity bool    `json:"contains_profanity"` // network name contains profanity
}

type LeaderboardResult struct {
	Earners []Earner         `json:"earners"`
	Error   *TopEarnersError `json:"error,omitempty"`
}

type TopEarnersError struct {
	Message string `json:"message"`
}

/**
 * Gets an ordered list of the top earners
 */
func GetLeaderboard(ctx context.Context) (earners []Earner, queryErr error) {

	server.Db(ctx, func(conn server.PgConn) {
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

							INNER JOIN (SELECT *
									FROM subsidy_payment
									ORDER BY end_time DESC
									LIMIT 4) t ON t.payment_plan_id = account_payment.payment_plan_id

							GROUP BY account_payment.network_id
							ORDER BY net_mib_count DESC
							LIMIT 100
				) t

				INNER JOIN network ON network.network_id = t.network_id
				ORDER BY t.net_mib_count DESC
				;
		`,
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

/**
 * Gets the ranking for the session network
 */
func GetNetworkLeaderboardRanking(session *session.ClientSession) (networkRanking NetworkRanking, queryErr error) {
	server.Db(session.Ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			session.Ctx,
			`
			SELECT
					t.net_mib_count,
					t.leaderboard_rank,
					network.leaderboard_public
			FROM (
					SELECT account_payment.network_id,
											sum(account_payment.payout_byte_count) / (1024 * 1024)                   AS net_mib_count,
											row_number() OVER (ORDER BY sum(account_payment.payout_byte_count) DESC) AS leaderboard_rank
								FROM account_payment

								INNER JOIN (SELECT *
														FROM subsidy_payment
														ORDER BY end_time DESC
														LIMIT 4) t ON t.payment_plan_id = account_payment.payment_plan_id
								GROUP BY account_payment.network_id
			) t
			INNER JOIN network ON network.network_id = t.network_id
			WHERE t.network_id = $1;
		`,
			session.ByJwt.NetworkId,
		)

		server.WithPgResult(result, err, func() {

			if err != nil {
				queryErr = err
				return
			}

			if result.Next() {

				server.Raise(result.Scan(
					&networkRanking.NetMiBCount,
					&networkRanking.LeaderboardRank,
					&networkRanking.LeaderboardPublic,
				))
			}
		})

	})

	return networkRanking, queryErr
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
