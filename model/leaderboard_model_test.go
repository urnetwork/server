package model

import (
	"context"
	"testing"

	"github.com/go-playground/assert/v2"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/session"
)

func TestLeaderboard(t *testing.T) {
	server.DefaultTestEnv().Run(func() {

		ctx := context.Background()
		netTransferByteCount := ByteCount(1024 * 1024 * 1024 * 1024)
		netRevenue := UsdToNanoCents(10.00)

		/**
		 * Network A and B will be the providers
		 */
		networkIdA := server.NewId()
		userIdA := server.NewId()
		// clientIdA := server.NewId()
		clientSessionA := session.Testing_CreateClientSession(
			ctx,
			jwt.NewByJwt(networkIdA, userIdA, "a", false),
		)

		networkIdB := server.NewId()
		userIdB := server.NewId()
		clientSessionB := session.Testing_CreateClientSession(
			ctx,
			jwt.NewByJwt(networkIdB, userIdB, "b", false),
		)

		/**
		 * We'll use network C will use Network A and B as providers
		 */
		networkIdC := server.NewId()
		userIdC := server.NewId()

		Testing_CreateNetwork(ctx, networkIdA, "a", userIdA)
		Testing_CreateNetwork(ctx, networkIdB, "b", userIdB)
		Testing_CreateNetwork(ctx, networkIdC, "c", userIdC)
		clientSessionC := session.Testing_CreateClientSession(
			ctx,
			jwt.NewByJwt(networkIdC, userIdC, "c", false),
		)

		/**
		 * Set public leaderboards
		 */
		err := SetNetworkLeaderboardPublic(true, clientSessionA)
		assert.Equal(t, err, nil)
		SetNetworkLeaderboardPublic(true, clientSessionB)
		assert.Equal(t, err, nil)

		/**
		 * Create balance for network C
		 */
		balanceCode, err := CreateBalanceCode(ctx, 2*netTransferByteCount, 2*netRevenue, "", "", "")
		assert.Equal(t, err, nil)
		RedeemBalanceCode(&RedeemBalanceCodeArgs{
			Secret: balanceCode.Secret,
		}, clientSessionC)

		usedTransferByteCount := ByteCount(1024 * 1024 * 1024)
		paid := NanoCents(0)

		for paid < UsdToNanoCents(EnvSubsidyConfig().MinWalletPayoutUsd) {
			transferEscrow, err := CreateTransferEscrow(ctx, networkIdC, userIdC, networkIdA, userIdA, usedTransferByteCount)
			assert.Equal(t, err, nil)

			err = CloseContract(ctx, transferEscrow.ContractId, userIdC, usedTransferByteCount, false)
			assert.Equal(t, err, nil)
			err = CloseContract(ctx, transferEscrow.ContractId, userIdA, usedTransferByteCount, false)
			assert.Equal(t, err, nil)
			paid += UsdToNanoCents(ProviderRevenueShare * NanoCentsToUsd(netRevenue) * float64(usedTransferByteCount) / float64(netTransferByteCount))
		}

		paid = NanoCents(0)

		for paid < 2*UsdToNanoCents(EnvSubsidyConfig().MinWalletPayoutUsd) {
			transferEscrow, err := CreateTransferEscrow(ctx, networkIdC, userIdC, networkIdB, userIdB, usedTransferByteCount)
			assert.Equal(t, err, nil)

			err = CloseContract(ctx, transferEscrow.ContractId, userIdC, usedTransferByteCount, false)
			assert.Equal(t, err, nil)
			err = CloseContract(ctx, transferEscrow.ContractId, userIdB, usedTransferByteCount, false)
			assert.Equal(t, err, nil)
			paid += UsdToNanoCents(ProviderRevenueShare * NanoCentsToUsd(netRevenue) * float64(usedTransferByteCount) / float64(netTransferByteCount))
		}

		/**
		 * Plan payments
		 */
		_, err = PlanPayments(ctx)
		assert.Equal(t, err, nil)

		leaderboardStats, err := GetLeaderboard(ctx)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(leaderboardStats), 2)
		assert.Equal(t, leaderboardStats[1].NetMiBCount > leaderboardStats[0].NetMiBCount, true)

		/**
		 * Set network A leaderboard to private
		 */
		err = SetNetworkLeaderboardPublic(false, clientSessionA)
		assert.Equal(t, err, nil)

		leaderboardStats, err = GetLeaderboard(ctx)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(leaderboardStats), 1)
		assert.Equal(t, leaderboardStats[0].NetworkId, networkIdB.String())

	})
}
