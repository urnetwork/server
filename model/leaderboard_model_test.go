package model

import (
	"context"
	"testing"

	"github.com/go-playground/assert/v2"
	"github.com/golang/glog"
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
		// clientIdB := server.NewId()

		/**
		 * We'll use network C will use Network A and B as providers
		 */
		networkIdC := server.NewId()
		userIdC := server.NewId()
		// clientIdC := server.NewId()

		Testing_CreateNetwork(ctx, networkIdA, "a", userIdA)
		Testing_CreateNetwork(ctx, networkIdB, "b", userIdB)
		Testing_CreateNetwork(ctx, networkIdC, "c", userIdC)
		clientSessionC := session.Testing_CreateClientSession(
			ctx,
			jwt.NewByJwt(networkIdC, userIdC, "c", false),
		)

		glog.Infof("about to set network A + B leaderboards public")

		/**
		 * Set public leaderboards
		 */
		err := SetNetworkLeaderboardPublic(true, clientSessionA)
		assert.Equal(t, err, nil)
		SetNetworkLeaderboardPublic(true, clientSessionB)
		assert.Equal(t, err, nil)

		glog.Infof("network A + B leaderboards set public")
		glog.Infof("create balance for network C")

		/**
		 * Create balance for network C
		 */
		balanceCode, err := CreateBalanceCode(ctx, 2*netTransferByteCount, 2*netRevenue, "", "", "")
		assert.Equal(t, err, nil)
		RedeemBalanceCode(&RedeemBalanceCodeArgs{
			Secret: balanceCode.Secret,
		}, clientSessionC)

		usedTransferByteCount := ByteCount(1024 * 1024 * 1024)
		// paid := UsdToNanoCents(ProviderRevenueShare * NanoCentsToUsd(netRevenue) * float64(usedTransferByteCount) / float64(netTransferByteCount))
		paid := NanoCents(0)

		glog.Infof("About to start transfer escrow C -> A")

		/**
		 * Network C uses Network A
		 */
		// for paid < UsdToNanoCents(EnvSubsidyConfig().MinWalletPayoutUsd) {
		// 	// companionTransferEscrow, err := model.CreateTransferEscrow(ctx, destinationNetworkId, destinationId, sourceNetworkId, sourceId, usedTransferByteCount)
		// 	companionTransferEscrow, err := CreateTransferEscrow(ctx, networkIdC, userIdC, networkIdA, userIdA, usedTransferByteCount)
		// 	assert.Equal(t, err, nil)
		// 	transferEscrow, err := CreateCompanionTransferEscrow(ctx, networkIdA, userIdA, networkIdC, userIdC, usedTransferByteCount, 1*time.Hour)
		// 	assert.Equal(t, err, nil)

		// 	err = CloseContract(ctx, transferEscrow.ContractId, userIdA, usedTransferByteCount, false)
		// 	assert.Equal(t, err, nil)
		// 	err = CloseContract(ctx, transferEscrow.ContractId, userIdC, usedTransferByteCount, false)
		// 	assert.Equal(t, err, nil)
		// 	CloseContract(ctx, companionTransferEscrow.ContractId, userIdA, ByteCount(0), false)
		// 	CloseContract(ctx, companionTransferEscrow.ContractId, userIdC, ByteCount(0), false)

		// 	// paidByteCount += usedTransferByteCount
		// 	paid += UsdToNanoCents(ProviderRevenueShare * NanoCentsToUsd(netRevenue) * float64(usedTransferByteCount) / float64(netTransferByteCount))
		// }

		// for paid < UsdToNanoCents(EnvSubsidyConfig().MinWalletPayoutUsd) {
		// 	transferEscrow, err := CreateTransferEscrow(ctx, networkIdA, userIdA, networkIdC, userIdC, usedTransferByteCount)
		// 	assert.Equal(t, err, nil)

		// 	err = CloseContract(ctx, transferEscrow.ContractId, userIdA, usedTransferByteCount, false)
		// 	assert.Equal(t, err, nil)
		// 	err = CloseContract(ctx, transferEscrow.ContractId, userIdC, usedTransferByteCount, false)
		// 	assert.Equal(t, err, nil)
		// 	// paidByteCount += usedTransferByteCount
		// 	paid += UsdToNanoCents(ProviderRevenueShare * NanoCentsToUsd(netRevenue) * float64(usedTransferByteCount) / float64(netTransferByteCount))
		// }

		for paid < UsdToNanoCents(EnvSubsidyConfig().MinWalletPayoutUsd) {
			transferEscrow, err := CreateTransferEscrow(ctx, networkIdC, userIdC, networkIdA, userIdA, usedTransferByteCount)
			assert.Equal(t, err, nil)

			err = CloseContract(ctx, transferEscrow.ContractId, userIdC, usedTransferByteCount, false)
			assert.Equal(t, err, nil)
			err = CloseContract(ctx, transferEscrow.ContractId, userIdA, usedTransferByteCount, false)
			assert.Equal(t, err, nil)
			// paidByteCount += usedTransferByteCount
			paid += UsdToNanoCents(ProviderRevenueShare * NanoCentsToUsd(netRevenue) * float64(usedTransferByteCount) / float64(netTransferByteCount))
			glog.Infof("Paid: %d / %d", paid, UsdToNanoCents(EnvSubsidyConfig().MinWalletPayoutUsd))
		}

		glog.Infof("C -> A complete")
		glog.Infof("About to start transfer escrow C -> B")

		paid = NanoCents(0)

		/**
		 * Network C uses Network B more
		 */
		// for paid < 2*UsdToNanoCents(EnvSubsidyConfig().MinWalletPayoutUsd) {
		// 	// companionTransferEscrow, err := model.CreateTransferEscrow(ctx, destinationNetworkId, destinationId, sourceNetworkId, sourceId, usedTransferByteCount)
		// 	companionTransferEscrow, err := CreateTransferEscrow(ctx, networkIdC, userIdC, networkIdB, userIdB, usedTransferByteCount)
		// 	assert.Equal(t, err, nil)
		// 	transferEscrow, err := CreateCompanionTransferEscrow(ctx, networkIdB, userIdB, networkIdC, userIdC, usedTransferByteCount, 1*time.Hour)
		// 	assert.Equal(t, err, nil)

		// 	err = CloseContract(ctx, transferEscrow.ContractId, userIdB, usedTransferByteCount, false)
		// 	assert.Equal(t, err, nil)
		// 	err = CloseContract(ctx, transferEscrow.ContractId, userIdC, usedTransferByteCount, false)
		// 	assert.Equal(t, err, nil)
		// 	CloseContract(ctx, companionTransferEscrow.ContractId, userIdB, ByteCount(0), false)
		// 	CloseContract(ctx, companionTransferEscrow.ContractId, userIdC, ByteCount(0), false)

		// 	// paidByteCount += usedTransferByteCount
		// 	paid += UsdToNanoCents(ProviderRevenueShare * NanoCentsToUsd(netRevenue) * float64(usedTransferByteCount) / float64(netTransferByteCount))
		// }

		for paid < 2*UsdToNanoCents(EnvSubsidyConfig().MinWalletPayoutUsd) {
			transferEscrow, err := CreateTransferEscrow(ctx, networkIdC, userIdC, networkIdB, userIdB, usedTransferByteCount)
			assert.Equal(t, err, nil)

			err = CloseContract(ctx, transferEscrow.ContractId, userIdC, usedTransferByteCount, false)
			assert.Equal(t, err, nil)
			err = CloseContract(ctx, transferEscrow.ContractId, userIdB, usedTransferByteCount, false)
			assert.Equal(t, err, nil)
			// paidByteCount += usedTransferByteCount
			paid += UsdToNanoCents(ProviderRevenueShare * NanoCentsToUsd(netRevenue) * float64(usedTransferByteCount) / float64(netTransferByteCount))
			glog.Infof("Paid: %d / %d", paid, UsdToNanoCents(EnvSubsidyConfig().MinWalletPayoutUsd))
		}

		glog.Infof("C -> B complete")
		glog.Infof("About to plan payments")

		/**
		 * Plan payments
		 */
		_, err = PlanPayments(ctx)
		assert.Equal(t, err, nil)

		glog.Infof("About to get leaderboard stats")

		leaderboardStats, err := GetLeaderboard(ctx)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(leaderboardStats), 2)

		glog.Infof("Leaderboard stats: %v", leaderboardStats)
		glog.Infof("Network A mib: %v", leaderboardStats[0].NetMiBCount)
		glog.Infof("Network B mib: %v", leaderboardStats[1].NetMiBCount)

		assert.Equal(t, leaderboardStats[1].NetMiBCount > leaderboardStats[0].NetMiBCount, true)

	})
}
