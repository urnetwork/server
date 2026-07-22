package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026/session"
)

func TestLeaderboard(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		ctx := context.Background()
		netTransferByteCount := ByteCount(1024 * 1024 * 1024 * 1024)
		netRevenue := UsdToNanoCents(10.00)

		/**
		 * Network A and B will be the providers
		 */
		networkIdA := server.NewId()
		userIdA := server.NewId()
		isPro := false
		// clientIdA := server.NewId()
		clientSessionA := session.Testing_CreateClientSession(
			ctx,
			jwt.NewByJwt(networkIdA, userIdA, "a", false, isPro),
		)

		networkIdB := server.NewId()
		userIdB := server.NewId()
		clientSessionB := session.Testing_CreateClientSession(
			ctx,
			jwt.NewByJwt(networkIdB, userIdB, "b", false, isPro),
		)

		/**
		 * We'll use network C will use Network A and B as providers
		 */
		networkIdC := server.NewId()
		userIdC := server.NewId()

		Testing_CreateNetwork(ctx, networkIdA, "shit_contains_profanity", userIdA)
		Testing_CreateNetwork(ctx, networkIdB, "b", userIdB)
		Testing_CreateNetwork(ctx, networkIdC, "c", userIdC)
		clientSessionC := session.Testing_CreateClientSession(
			ctx,
			jwt.NewByJwt(networkIdC, userIdC, "c", false, isPro),
		)

		/**
		 * Set public leaderboards
		 */
		err := SetNetworkLeaderboardPublic(true, clientSessionA)
		connect.AssertEqual(t, err, nil)
		SetNetworkLeaderboardPublic(true, clientSessionB)
		connect.AssertEqual(t, err, nil)

		/**
		 * Create balance for network C
		 */
		subscriptionYearDuration := 365 * 24 * time.Hour
		balanceCode, err := CreateBalanceCode(
			ctx,
			2*netTransferByteCount,
			subscriptionYearDuration,
			2*netRevenue,
			"",
			"",
			"",
		)

		connect.AssertEqual(t, err, nil)
		RedeemBalanceCode(&RedeemBalanceCodeArgs{
			Secret:    balanceCode.Secret,
			NetworkId: clientSessionC.ByJwt.NetworkId,
		}, clientSessionC.Ctx)

		usedTransferByteCount := ByteCount(1024 * 1024 * 1024)
		paid := NanoCents(0)

		for paid < UsdToNanoCents(EnvSubsidyConfig().MinWalletPayoutUsd) {
			transferEscrow, err := CreateTransferEscrow(ctx, networkIdC, userIdC, networkIdA, userIdA, usedTransferByteCount)
			connect.AssertEqual(t, err, nil)

			err = CloseContract(ctx, transferEscrow.ContractId, userIdC, usedTransferByteCount, false)
			connect.AssertEqual(t, err, nil)
			err = CloseContract(ctx, transferEscrow.ContractId, userIdA, usedTransferByteCount, false)
			connect.AssertEqual(t, err, nil)
			paid += UsdToNanoCents(ProviderRevenueShare * NanoCentsToUsd(netRevenue) * float64(usedTransferByteCount) / float64(netTransferByteCount))
		}

		paid = NanoCents(0)

		for paid < 2*UsdToNanoCents(EnvSubsidyConfig().MinWalletPayoutUsd) {
			transferEscrow, err := CreateTransferEscrow(ctx, networkIdC, userIdC, networkIdB, userIdB, usedTransferByteCount)
			connect.AssertEqual(t, err, nil)

			err = CloseContract(ctx, transferEscrow.ContractId, userIdC, usedTransferByteCount, false)
			connect.AssertEqual(t, err, nil)
			err = CloseContract(ctx, transferEscrow.ContractId, userIdB, usedTransferByteCount, false)
			connect.AssertEqual(t, err, nil)
			paid += UsdToNanoCents(ProviderRevenueShare * NanoCentsToUsd(netRevenue) * float64(usedTransferByteCount) / float64(netTransferByteCount))
		}

		/**
		 * Plan payments
		 */
		_, err = PlanPayments(ctx)
		connect.AssertEqual(t, err, nil)

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
