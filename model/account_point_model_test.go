package model_test

import (
	"context"
	"testing"

	"github.com/go-playground/assert/v2"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

func TestAccountPoints(t *testing.T) {
	server.DefaultTestEnv().Run(func() {

		ctx := context.Background()
		networkId := server.NewId()

		err := model.ApplyAccountPoints(ctx, networkId, model.AccountPointEventReferral, 10)
		assert.Equal(t, err, nil)

		networkPoints := model.FetchAccountPoints(ctx, networkId)
		assert.NotEqual(t, networkPoints, nil)
		assert.Equal(t, len(networkPoints), 1)
		assert.Equal(t, networkPoints[0].NetworkId, networkId)
		assert.Equal(t, networkPoints[0].Event, "referral")
		assert.NotEqual(t, networkPoints[0].PointValue, 0)

		err = model.ApplyAccountPoints(ctx, networkId, model.AccountPointEventReferral, 5)
		assert.Equal(t, err, nil)

		networkPoints = model.FetchAccountPoints(ctx, networkId)
		assert.Equal(t, len(networkPoints), 2)

		totalPoints := 0
		for _, point := range networkPoints {
			totalPoints += point.PointValue
		}
		assert.Equal(t, totalPoints, 15)

	})
}

/**
 * Test that the account points are paid out correctly to networks per payout.
 */
func TestAccountPointsPerPayout(t *testing.T) {
	server.DefaultTestEnv().Run(func() {

		ctx := context.Background()
		netTransferByteCount := model.ByteCount(1024 * 1024 * 1024 * 1024)
		netRevenue := model.UsdToNanoCents(10)

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
		// clientSessionB := session.Testing_CreateClientSession(
		// 	ctx,
		// 	jwt.NewByJwt(networkIdB, userIdB, "b", false),
		// )

		/**
		 * We'll use network C will use Network A and B as providers
		 */
		networkIdC := server.NewId()
		userIdC := server.NewId()

		model.Testing_CreateNetwork(ctx, networkIdA, "a", userIdA)
		model.Testing_CreateNetwork(ctx, networkIdB, "b", userIdB)
		model.Testing_CreateNetwork(ctx, networkIdC, "c", userIdC)
		clientSessionC := session.Testing_CreateClientSession(
			ctx,
			jwt.NewByJwt(networkIdC, userIdC, "c", false),
		)

		/**
		 * Create balance for network C
		 */
		balanceCode, err := model.CreateBalanceCode(ctx, 2*netTransferByteCount, 2*netRevenue, "", "", "")
		assert.Equal(t, err, nil)
		model.RedeemBalanceCode(&model.RedeemBalanceCodeArgs{
			Secret: balanceCode.Secret,
		}, clientSessionC)

		usedTransferByteCount := model.ByteCount(1024 * 1024 * 1024)

		/**
		 * Network A provides data to Network C
		 */
		paid := model.NanoCents(0)
		for paid < model.UsdToNanoCents(2.00) {
			transferEscrow, err := model.CreateTransferEscrow(ctx, networkIdC, userIdC, networkIdA, userIdA, usedTransferByteCount)
			assert.Equal(t, err, nil)

			err = model.CloseContract(ctx, transferEscrow.ContractId, userIdC, usedTransferByteCount, false)
			assert.Equal(t, err, nil)
			err = model.CloseContract(ctx, transferEscrow.ContractId, userIdA, usedTransferByteCount, false)
			assert.Equal(t, err, nil)
			paid += model.UsdToNanoCents(model.ProviderRevenueShare * model.NanoCentsToUsd(netRevenue) * float64(usedTransferByteCount) / float64(netTransferByteCount))
		}

		/**
		 * Network B provides twices as much data to Network C
		 */
		paid = model.NanoCents(0)
		for paid < model.UsdToNanoCents(3.00) {
			transferEscrow, err := model.CreateTransferEscrow(ctx, networkIdC, userIdC, networkIdB, userIdB, usedTransferByteCount)
			assert.Equal(t, err, nil)

			err = model.CloseContract(ctx, transferEscrow.ContractId, userIdC, usedTransferByteCount, false)
			assert.Equal(t, err, nil)
			err = model.CloseContract(ctx, transferEscrow.ContractId, userIdB, usedTransferByteCount, false)
			assert.Equal(t, err, nil)
			paid += model.UsdToNanoCents(model.ProviderRevenueShare * model.NanoCentsToUsd(netRevenue) * float64(usedTransferByteCount) / float64(netTransferByteCount))
		}

		/**
		 * Should have no account points yet
		 */
		accountPoints := model.FetchAccountPoints(ctx, networkIdA)
		assert.Equal(t, len(accountPoints), 0)
		accountPoints = model.FetchAccountPoints(ctx, networkIdB)
		assert.Equal(t, len(accountPoints), 0)

		/**
		 * Set network A as Seeker holder
		 */
		// creates a new wallet and marks it as seeker holder
		seekerHolderAddress := "0x1"
		err = model.MarkWalletSeekerHolder(seekerHolderAddress, clientSessionA)
		assert.Equal(t, err, nil)

		/**
		 * Plan payments
		 */
		paymentPlan, err := model.PlanPayments(ctx)
		assert.Equal(t, err, nil)

		assert.Equal(t, len(paymentPlan.NetworkPayments), 2)

		// get payment for network A
		_, ok := paymentPlan.NetworkPayments[networkIdA]
		assert.Equal(t, ok, true)

		_, ok = paymentPlan.NetworkPayments[networkIdB]
		assert.Equal(t, ok, true)

		/**
		 * total payout: 5.00 USDC
		 * total points should be 500 * 1m
		 * network A points should be 2 / 5 * 1m = 400000
		 * network A is a Seeker holder, so it gets x2 points
		 * network B points should be 3 / 5 * 1m = 600000
		 */

		networkPointsA := model.FetchAccountPoints(ctx, networkIdA)
		assert.Equal(t, len(networkPointsA), 1)
		assert.Equal(t, networkPointsA[0].NetworkId, networkIdA)
		assert.Equal(t, networkPointsA[0].Event, string(model.AccountPointEventPayout))
		assert.Equal(t, networkPointsA[0].PointValue, int(400_000*model.SeekerHolderMultiplier))
		networkPointsB := model.FetchAccountPoints(ctx, networkIdB)
		assert.Equal(t, len(networkPointsB), 1)
		assert.Equal(t, networkPointsB[0].NetworkId, networkIdB)
		assert.Equal(t, networkPointsB[0].Event, string(model.AccountPointEventPayout))
		assert.Equal(t, networkPointsB[0].PointValue, 600_000)

	})
}
