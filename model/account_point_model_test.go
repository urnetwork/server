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

		/**
		 * We'll use network C will use Network A and B as providers
		 */
		networkIdC := server.NewId()
		userIdC := server.NewId()

		/**
		 * Network D will be the parent referring network for Network A
		 */
		networkIdD := server.NewId()
		userIdD := server.NewId()

		/**
		 * Network E and F will be child networks of Network A
		 */
		networkIdE := server.NewId()
		userIdE := server.NewId()
		networkIdF := server.NewId()
		userIdF := server.NewId()

		/**
		 * Network G will be a child network of Network E
		 * Used to test recursive child payouts
		 */
		networkIdG := server.NewId()
		userIdG := server.NewId()

		/**
		 * Network H will be a child of Network B
		 */
		networkIdH := server.NewId()
		userIdH := server.NewId()

		model.Testing_CreateNetwork(ctx, networkIdA, "a", userIdA)
		model.Testing_CreateNetwork(ctx, networkIdB, "b", userIdB)
		model.Testing_CreateNetwork(ctx, networkIdC, "c", userIdC)
		model.Testing_CreateNetwork(ctx, networkIdD, "d", userIdD)
		model.Testing_CreateNetwork(ctx, networkIdE, "e", userIdE)
		model.Testing_CreateNetwork(ctx, networkIdF, "f", userIdF)
		model.Testing_CreateNetwork(ctx, networkIdG, "g", userIdG)
		model.Testing_CreateNetwork(ctx, networkIdH, "h", userIdH)

		clientSessionC := session.Testing_CreateClientSession(
			ctx,
			jwt.NewByJwt(networkIdC, userIdC, "c", false),
		)

		/*
		   Network Referral Tree:

		           D
		           |
		           A
		          / \
		         E   F
		         |
		         G

		       B
		       |
		       H

		   Legend:
		   - D refers A
		   - A refers E and F
		   - E refers G
		   - B refers H
		*/

		/**
		 * Create referral from network D to network A
		 */
		createdReferralCode := model.CreateNetworkReferralCode(ctx, networkIdD)
		createdNetworkReferral := model.CreateNetworkReferral(ctx, networkIdA, createdReferralCode.ReferralCode)
		assert.Equal(t, createdNetworkReferral.NetworkId, networkIdA)
		assert.Equal(t, createdNetworkReferral.ReferralNetworkId, networkIdD)

		/**
		 * Create referral from network A to network E, F
		 */
		createdReferralCode = model.CreateNetworkReferralCode(ctx, networkIdA)
		createdNetworkReferralE := model.CreateNetworkReferral(ctx, networkIdE, createdReferralCode.ReferralCode)
		assert.Equal(t, createdNetworkReferralE.NetworkId, networkIdE)
		assert.Equal(t, createdNetworkReferralE.ReferralNetworkId, networkIdA)
		createdNetworkReferralF := model.CreateNetworkReferral(ctx, networkIdF, createdReferralCode.ReferralCode)
		assert.Equal(t, createdNetworkReferralF.NetworkId, networkIdF)
		assert.Equal(t, createdNetworkReferralF.ReferralNetworkId, networkIdA)

		/**
		 * Network E creates a referral to network G
		 */
		createdReferralCode = model.CreateNetworkReferralCode(ctx, networkIdE)
		createdNetworkReferralG := model.CreateNetworkReferral(ctx, networkIdG, createdReferralCode.ReferralCode)
		assert.Equal(t, createdNetworkReferralG.NetworkId, networkIdG)
		assert.Equal(t, createdNetworkReferralG.ReferralNetworkId, networkIdE)

		/**
		 * Network B creates a referral to network H
		 */
		createdReferralCode = model.CreateNetworkReferralCode(ctx, networkIdB)
		createdNetworkReferralH := model.CreateNetworkReferral(ctx, networkIdH, createdReferralCode.ReferralCode)
		assert.Equal(t, createdNetworkReferralH.NetworkId, networkIdH)
		assert.Equal(t, createdNetworkReferralH.ReferralNetworkId, networkIdB)

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
		 * 1m points per month (payout?) (1m / sum(accountPoints)) * ((payout / totalPayout) * 1m)
		 * network A points should be 2 / 5 * 1m = 400000
		 * network A is a Seeker holder, so it gets x2 points
		 * network B points should be 3 / 5 * 1m = 600000
		 * network D, the referring network, should get a bonus of +25% of network A points
		 * network D points should be 400000 * 0.25 * seeker_holder_multipler (x2) = 200000
		 */

		/**
		 * Provider Network A points
		 * 2 / 5 * 1_000_000 = 400_000
		 * Since it is a Seeker holder, it gets x2 points
		 */
		networkPointsA := model.FetchAccountPoints(ctx, networkIdA)
		assert.Equal(t, len(networkPointsA), 1)
		assert.Equal(t, networkPointsA[0].NetworkId, networkIdA)
		assert.Equal(t, networkPointsA[0].Event, string(model.AccountPointEventPayout))
		assert.Equal(t, networkPointsA[0].PointValue, int(400_000*model.SeekerHolderMultiplier))

		/**
		 * Provider Network B points
		 * 3 / 5 * 1_000_000 = 600_000
		 * Network B is not a Seeker holder, so it gets the normal points
		 * No parent or child referrals
		 */
		networkPointsB := model.FetchAccountPoints(ctx, networkIdB)
		assert.Equal(t, len(networkPointsB), 1)
		assert.Equal(t, networkPointsB[0].NetworkId, networkIdB)
		assert.Equal(t, networkPointsB[0].Event, string(model.AccountPointEventPayout))
		assert.Equal(t, networkPointsB[0].PointValue, 600_000)

		/**
		 * Network D should get a bonus of 25% of network A points
		 * 200_000 points, since it is a Seeker holder, it gets x2 points
		 */
		networkPointsD := model.FetchAccountPoints(ctx, networkIdD)
		assert.Equal(t, len(networkPointsD), 1)
		assert.Equal(t, networkPointsD[0].NetworkId, networkIdD)
		assert.Equal(t, networkPointsD[0].Event, string(model.AccountPointEventPayout))
		assert.Equal(t, networkPointsD[0].PointValue, int(100_000*model.SeekerHolderMultiplier))

		/**
		 * Network E should get 400_000 * 0.25 * 2 = 200_000 points
		 */
		networkPointsE := model.FetchAccountPoints(ctx, networkIdE)
		assert.Equal(t, len(networkPointsE), 1)
		assert.Equal(t, networkPointsE[0].NetworkId, networkIdE)
		assert.Equal(t, networkPointsE[0].Event, string(model.AccountPointEventPayout))
		assert.Equal(t, networkPointsE[0].PointValue, int(100_000*model.SeekerHolderMultiplier))

		/**
		 * Network F should get 400_000 * 0.25 = 100_000 points * seeker_holder_multiplier (x2) = 200_000 points
		 */
		networkPointsF := model.FetchAccountPoints(ctx, networkIdF)
		assert.Equal(t, len(networkPointsF), 1)
		assert.Equal(t, networkPointsF[0].NetworkId, networkIdF)
		assert.Equal(t, networkPointsF[0].Event, string(model.AccountPointEventPayout))
		assert.Equal(t, networkPointsF[0].PointValue, int(100_000*model.SeekerHolderMultiplier))

		/**
		 * Network G should get 400_000 * 0.125 = 50_000 points * seeker_holder_multiplier (x2) = 100_000 points
		 */
		networkPointsG := model.FetchAccountPoints(ctx, networkIdG)
		assert.Equal(t, len(networkPointsG), 1)
		assert.Equal(t, networkPointsG[0].NetworkId, networkIdG)
		assert.Equal(t, networkPointsG[0].Event, string(model.AccountPointEventPayout))
		assert.Equal(t, networkPointsG[0].PointValue, int(50_000*model.SeekerHolderMultiplier))

		/**
		 * Network H should get 600_000 * 0.25 = 150_000 points
		 */
		networkPointsH := model.FetchAccountPoints(ctx, networkIdH)
		assert.Equal(t, len(networkPointsH), 1)
		assert.Equal(t, networkPointsH[0].NetworkId, networkIdH)
		assert.Equal(t, networkPointsH[0].Event, string(model.AccountPointEventPayout))
		assert.Equal(t, networkPointsH[0].PointValue, 150_000)
	})
}
