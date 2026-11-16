package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"

	"github.com/urnetwork/server/session"
)

// Records one payout window, a subsidy epoch that ended now, with one payment
// of the window's bytes per network.
func leaderboardTestPayoutWindow(t testing.TB, ctx context.Context, networkByteCounts map[server.Id]ByteCount) {
	t.Helper()
	paymentPlanId := server.NewId()
	now := server.NowUtc()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO subsidy_payment
			(payment_plan_id,start_time,end_time,active_user_count,paid_user_count,net_payout_byte_count_paid,net_payout_byte_count_unpaid,net_revenue_nano_cents,net_payout_nano_cents)
			VALUES($1,$2,$3,1,1,0,0,0,0)`, paymentPlanId, now.Add(-24*time.Hour), now))
		for networkId, byteCount := range networkByteCounts {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO account_payment
				(payment_id,payment_plan_id,network_id,payout_byte_count,payout_nano_cents,min_sweep_time)
				VALUES($1,$2,$3,$4,0,$5)`, server.NewId(), paymentPlanId, networkId, byteCount, now.Add(-24*time.Hour)))
		}
	})
}

func TestLeaderboard(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		ctx := context.Background()

		/**
		 * Network A and B will be the providers
		 */
		networkIdA := server.NewId()
		userIdA := server.NewId()
		isPro := false
		// clientIdA := server.NewId()
		clientSessionA := session.Testing_CreateClientSession(
			ctx,
			session.NewByJwt(networkIdA, userIdA, "a", false, isPro),
		)

		networkIdB := server.NewId()
		userIdB := server.NewId()
		clientSessionB := session.Testing_CreateClientSession(
			ctx,
			session.NewByJwt(networkIdB, userIdB, "b", false, isPro),
		)

		/**
		 * We'll use network C will use Network A and B as providers
		 */
		networkIdC := server.NewId()
		userIdC := server.NewId()

		Testing_CreateNetwork(ctx, networkIdA, "shit_contains_profanity", userIdA)
		Testing_CreateNetwork(ctx, networkIdB, "b", userIdB)
		Testing_CreateNetwork(ctx, networkIdC, "c", userIdC)
		Testing_CreateDevice(ctx, networkIdA, server.NewId(), userIdA, "synthetic-leaderboard-provider-a", "synthetic")
		Testing_CreateDevice(ctx, networkIdB, server.NewId(), userIdB, "synthetic-leaderboard-provider-b", "synthetic")
		Testing_CreateDevice(ctx, networkIdC, server.NewId(), userIdC, "synthetic-leaderboard-caller", "synthetic")

		/**
		 * Set public leaderboards
		 */
		err := SetNetworkLeaderboardPublic(true, clientSessionA)
		connect.AssertEqual(t, err, nil)
		SetNetworkLeaderboardPublic(true, clientSessionB)
		connect.AssertEqual(t, err, nil)

		/**
		 * A payout window in which network C's traffic paid network B twice
		 * network A's bytes, recorded as a payout plan recorded its subsidy
		 * epoch and payments before USDC payouts ended
		 */
		leaderboardTestPayoutWindow(t, ctx, map[server.Id]ByteCount{
			networkIdA: 1024 * 1024 * 1024,
			networkIdB: 2 * 1024 * 1024 * 1024,
		})

		/**
		 * check leaderboard stats
		 */
		leaderboardStats, err := GetLeaderboard(ctx)

		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(leaderboardStats), 2)
		connect.AssertEqual(t, leaderboardStats[0].NetworkId, networkIdB.String())
		connect.AssertEqual(t, leaderboardStats[1].NetworkId, networkIdA.String())

		// profanity check
		// network B does not contain profanity
		connect.AssertEqual(t, leaderboardStats[0].ContainsProfanity, false)

		// FIXME - need to safely use goaway lib - network A contains profanity
		// connect.AssertEqual(t, leaderboardStats[1].ContainsProfanity, true)

		/**
		 * Get individual network ranking
		 */
		networkRanking, err := GetNetworkLeaderboardRanking(clientSessionB)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, networkRanking.LeaderboardRank, 1)
		connect.AssertEqual(t, networkRanking.LeaderboardPublic, true)

		networkRanking, err = GetNetworkLeaderboardRanking(clientSessionA)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, networkRanking.LeaderboardRank, 2)
		connect.AssertEqual(t, networkRanking.LeaderboardPublic, true)

		/**
		 * Set network A leaderboard to private
		 */
		err = SetNetworkLeaderboardPublic(false, clientSessionA)
		connect.AssertEqual(t, err, nil)

		leaderboardStats, err = GetLeaderboard(ctx)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(leaderboardStats), 2)
		connect.AssertEqual(t, leaderboardStats[1].NetworkId, "")
		connect.AssertEqual(t, leaderboardStats[1].NetworkId, "")

		/**
		* LeaderboardPublic should be set to false
		 */
		networkRanking, err = GetNetworkLeaderboardRanking(clientSessionA)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, networkRanking.LeaderboardRank, 2)
		connect.AssertEqual(t, networkRanking.LeaderboardPublic, false)

	})
}
