package model

import (
	"context"
	"fmt"
	mathrand "math/rand"
	"net/netip"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/glog/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026/session"
)

func TestAccountPoints(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		ctx := context.Background()
		networkId := server.NewId()

		applyAccountPointsArgs := ApplyAccountPointsArgs{
			NetworkId:  networkId,
			Event:      AccountPointEventPayout,
			PointValue: 10,
		}

		err := ApplyAccountPoints(ctx, applyAccountPointsArgs)
		connect.AssertEqual(t, err, nil)

		networkPoints := FetchAccountPoints(ctx, networkId)
		connect.AssertNotEqual(t, networkPoints, nil)
		connect.AssertEqual(t, len(networkPoints), 1)
		connect.AssertEqual(t, networkPoints[0].NetworkId, networkId)
		connect.AssertEqual(t, networkPoints[0].Event, string(AccountPointEventPayout))
		connect.AssertNotEqual(t, networkPoints[0].PointValue, 0)

		applyAccountPointsArgs = ApplyAccountPointsArgs{
			NetworkId:  networkId,
			Event:      AccountPointEventPayout,
			PointValue: 5,
		}

		err = ApplyAccountPoints(ctx, applyAccountPointsArgs)
		connect.AssertEqual(t, err, nil)

		networkPoints = FetchAccountPoints(ctx, networkId)
		connect.AssertEqual(t, len(networkPoints), 2)

		totalPoints := NanoPoints(0)
		for _, point := range networkPoints {
			totalPoints += point.PointValue
		}
		connect.AssertEqual(t, totalPoints, NanoPoints(15))

	})
}

/**
 * Test that the account points are paid out correctly to networks per payout.
 */
func TestAccountPointsPerPayout(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		subsidyConfigCopy := *EnvSubsidyConfig()
		subsidyConfigCopy.ForcePoints = true
		subsidyConfig := &subsidyConfigCopy

		ctx := context.Background()
		netTransferByteCount := ByteCount(1024 * 1024 * 1024 * 1024)
		netRevenue := UsdToNanoCents(10)

		/**
		 * Network A and B will be the providers
		 */
		networkIdA := server.NewId()
		// clientIdA := server.NewId()
		userIdA := server.NewId()
		clientSessionA := session.Testing_CreateClientSession(
			ctx,
			jwt.NewByJwt(networkIdA, userIdA, "a", false, false),
		)

		networkIdB := server.NewId()
		userIdB := server.NewId()

		/**
		 * Network C will use Network A and B as providers
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

		Testing_CreateNetwork(ctx, networkIdA, "a", userIdA)
		Testing_CreateNetwork(ctx, networkIdB, "b", userIdB)
		Testing_CreateNetwork(ctx, networkIdC, "c", userIdC)
		Testing_CreateNetwork(ctx, networkIdD, "d", userIdD)
		Testing_CreateNetwork(ctx, networkIdE, "e", userIdE)
		Testing_CreateNetwork(ctx, networkIdF, "f", userIdF)
		Testing_CreateNetwork(ctx, networkIdG, "g", userIdG)
		Testing_CreateNetwork(ctx, networkIdH, "h", userIdH)

		clientSessionC := session.Testing_CreateClientSession(
			ctx,
			jwt.NewByJwt(networkIdC, userIdC, "c", false, false),
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
		createdReferralCode := CreateNetworkReferralCode(ctx, networkIdD)
		createdNetworkReferral := CreateNetworkReferral(ctx, networkIdA, createdReferralCode.ReferralCode)
		connect.AssertEqual(t, createdNetworkReferral.NetworkId, networkIdA)
		connect.AssertEqual(t, createdNetworkReferral.ReferralNetworkId, networkIdD)

		/**
		 * Create referral from network A to network E, F
		 */
		createdReferralCode = CreateNetworkReferralCode(ctx, networkIdA)
		createdNetworkReferralE := CreateNetworkReferral(ctx, networkIdE, createdReferralCode.ReferralCode)
		connect.AssertEqual(t, createdNetworkReferralE.NetworkId, networkIdE)
		connect.AssertEqual(t, createdNetworkReferralE.ReferralNetworkId, networkIdA)
		createdNetworkReferralF := CreateNetworkReferral(ctx, networkIdF, createdReferralCode.ReferralCode)
		connect.AssertEqual(t, createdNetworkReferralF.NetworkId, networkIdF)
		connect.AssertEqual(t, createdNetworkReferralF.ReferralNetworkId, networkIdA)

		/**
		 * Network E creates a referral to network G
		 */
		createdReferralCode = CreateNetworkReferralCode(ctx, networkIdE)
		createdNetworkReferralG := CreateNetworkReferral(ctx, networkIdG, createdReferralCode.ReferralCode)
		connect.AssertEqual(t, createdNetworkReferralG.NetworkId, networkIdG)
		connect.AssertEqual(t, createdNetworkReferralG.ReferralNetworkId, networkIdE)

		/**
		 * Network B creates a referral to network H
		 */
		createdReferralCode = CreateNetworkReferralCode(ctx, networkIdB)
		createdNetworkReferralH := CreateNetworkReferral(ctx, networkIdH, createdReferralCode.ReferralCode)
		connect.AssertEqual(t, createdNetworkReferralH.NetworkId, networkIdH)
		connect.AssertEqual(t, createdNetworkReferralH.ReferralNetworkId, networkIdB)

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

		/**
		 * Network A provides data to Network C
		 */
		paid := NanoCents(0)
		for paid < UsdToNanoCents(2.00) {
			transferEscrow, err := CreateTransferEscrow(ctx, networkIdC, userIdC, networkIdA, userIdA, usedTransferByteCount)
			connect.AssertEqual(t, err, nil)

			err = CloseContract(ctx, transferEscrow.ContractId, userIdC, usedTransferByteCount, false)
			connect.AssertEqual(t, err, nil)
			err = CloseContract(ctx, transferEscrow.ContractId, userIdA, usedTransferByteCount, false)
			connect.AssertEqual(t, err, nil)
			paid += UsdToNanoCents(ProviderRevenueShare * NanoCentsToUsd(netRevenue) * float64(usedTransferByteCount) / float64(netTransferByteCount))
		}

		/**
		 * Network B provides twices as much data to Network C
		 */
		paid = NanoCents(0)
		for paid < UsdToNanoCents(3.00) {
			transferEscrow, err := CreateTransferEscrow(ctx, networkIdC, userIdC, networkIdB, userIdB, usedTransferByteCount)
			connect.AssertEqual(t, err, nil)

			err = CloseContract(ctx, transferEscrow.ContractId, userIdC, usedTransferByteCount, false)
			connect.AssertEqual(t, err, nil)
			err = CloseContract(ctx, transferEscrow.ContractId, userIdB, usedTransferByteCount, false)
			connect.AssertEqual(t, err, nil)
			paid += UsdToNanoCents(ProviderRevenueShare * NanoCentsToUsd(netRevenue) * float64(usedTransferByteCount) / float64(netTransferByteCount))
		}

		/**
		 * Should have no account points yet
		 */
		accountPoints := FetchAccountPoints(ctx, networkIdA)
		connect.AssertEqual(t, len(accountPoints), 0)
		accountPoints = FetchAccountPoints(ctx, networkIdB)
		connect.AssertEqual(t, len(accountPoints), 0)

		/**
		 * Set network A as Seeker holder
		 */
		// creates a new wallet and marks it as seeker holder
		seekerHolderAddress := "0x1"
		err = MarkWalletSeekerHolder(seekerHolderAddress, clientSessionA)
		connect.AssertEqual(t, err, nil)

		/**
		 * Plan payments
		 */
		paymentPlan, err := PlanPaymentsWithConfig(ctx, subsidyConfig)
		connect.AssertEqual(t, err, nil)

		connect.AssertEqual(t, len(paymentPlan.NetworkPayments), 2)

		// get payment for network A
		paymentNetworkA, ok := paymentPlan.NetworkPayments[networkIdA]
		connect.AssertEqual(t, ok, true)

		paymentNetworkB, ok := paymentPlan.NetworkPayments[networkIdB]
		connect.AssertEqual(t, ok, true)

		/**
		 * total payout: 5.00 USDC
		 * ppp = 1_000_000 points per payout
		 * (ppp / sum(accountPoints)) * ((payout / totalPayout) * 1m)
		 * network A points should be 2 / 5 * ppp = 400_000 points
		 * network A is a Seeker holder, so it gets x2 points
		 * network B points should be 3 / 5 * ppp = 600_000 points
		 */

		/**
		 * Provider Network A points
		 * 2 / 5 * 1_000_000 = 400_000
		 * Since it is a Seeker holder, it gets x2 points
		 */
		networkPointsA := FetchAccountPoints(ctx, networkIdA)
		networkPointsB := FetchAccountPoints(ctx, networkIdB)

		/**
		 * Assert Network A + Network B points add up to 250_000
		 */
		totalPoints := networkPointsA[0].PointValue + networkPointsB[0].PointValue
		connect.AssertEqual(t, NanoPoints(totalPoints), PointsToNanoPoints(float64(EnvSubsidyConfig().AccountPointsPerPayout)))

		expectedPointsA := PointsToNanoPoints(float64(400_000))

		connect.AssertEqual(t, len(networkPointsA), 2)
		connect.AssertEqual(t, networkPointsA[0].NetworkId, networkIdA)
		connect.AssertEqual(t, networkPointsA[0].Event, string(AccountPointEventPayout))
		connect.AssertEqual(t, networkPointsA[0].PointValue, expectedPointsA)
		connect.AssertEqual(t, networkPointsA[0].AccountPaymentId, &paymentNetworkA.PaymentId)
		connect.AssertEqual(t, networkPointsA[0].PaymentPlanId, &paymentPlan.PaymentPlanId)
		connect.AssertEqual(t, networkPointsA[0].LinkedNetworkId, nil)
		connect.AssertEqual(t, networkPointsA[1].NetworkId, networkIdA)
		connect.AssertEqual(t, networkPointsA[1].Event, string(AccountPointEventPayoutMultiplier))
		connect.AssertEqual(t, networkPointsA[1].PointValue, NanoPoints(float64(expectedPointsA)*subsidyConfig.SeekerHolderMultiplier)-expectedPointsA)
		connect.AssertEqual(t, networkPointsA[1].PaymentPlanId, &paymentPlan.PaymentPlanId)
		connect.AssertEqual(t, networkPointsA[1].AccountPaymentId, &paymentNetworkA.PaymentId)
		connect.AssertEqual(t, networkPointsA[1].LinkedNetworkId, nil)

		/**
		 * Provider Network B points
		 * 3 / 5 * 1_000_000 = 600_000
		 * Network B is not a Seeker holder, so it gets the normal points
		 * No parent or child referrals
		 */
		expectedPointsB := PointsToNanoPoints(float64(600_000))
		connect.AssertEqual(t, len(networkPointsB), 1)
		connect.AssertEqual(t, networkPointsB[0].NetworkId, networkIdB)
		connect.AssertEqual(t, networkPointsB[0].Event, string(AccountPointEventPayout))
		connect.AssertEqual(t, networkPointsB[0].PointValue, expectedPointsB)
		connect.AssertEqual(t, networkPointsB[0].PaymentPlanId, &paymentPlan.PaymentPlanId)
		connect.AssertEqual(t, networkPointsB[0].LinkedNetworkId, nil)
		connect.AssertEqual(t, networkPointsB[0].AccountPaymentId, &paymentNetworkB.PaymentId)

		/**
		 * Network D should get a bonus of 25% of network A points
		 * 200_000 points, since it is a Seeker holder, it gets x2 points
		 */
		networkPointsD := FetchAccountPoints(ctx, networkIdD)
		connect.AssertEqual(t, len(networkPointsD), 1)
		connect.AssertEqual(t, networkPointsD[0].NetworkId, networkIdD)
		connect.AssertEqual(t, networkPointsD[0].Event, string(AccountPointEventPayoutLinkedAccount))
		connect.AssertEqual(t, networkPointsD[0].PointValue, NanoPoints(float64(expectedPointsA)*0.25*subsidyConfig.SeekerHolderMultiplier))
		connect.AssertEqual(t, networkPointsD[0].PaymentPlanId, &paymentPlan.PaymentPlanId)
		connect.AssertEqual(t, networkPointsD[0].LinkedNetworkId, networkIdA)
		connect.AssertEqual(t, networkPointsD[0].AccountPaymentId, &paymentNetworkA.PaymentId)

		/**
		 * Network A child Network E should get expectedPointsA (150_000) * 0.25 * seeker multiplier = 75_000 points
		 */
		expectedPointsE := NanoPoints(float64(expectedPointsA) * 0.25 * subsidyConfig.SeekerHolderMultiplier)
		glog.Infof("Expected points E: %d", expectedPointsE)
		networkPointsE := FetchAccountPoints(ctx, networkIdE)
		connect.AssertEqual(t, len(networkPointsE), 1)
		connect.AssertEqual(t, networkPointsE[0].NetworkId, networkIdE)
		connect.AssertEqual(t, networkPointsE[0].Event, string(AccountPointEventPayoutLinkedAccount))
		connect.AssertEqual(t, networkPointsE[0].PointValue, expectedPointsE)
		connect.AssertEqual(t, networkPointsE[0].PaymentPlanId, &paymentPlan.PaymentPlanId)
		connect.AssertEqual(t, networkPointsE[0].LinkedNetworkId, networkIdA)
		connect.AssertEqual(t, networkPointsE[0].AccountPaymentId, &paymentNetworkA.PaymentId)

		/**
		 * Network A child Network F should get expectedPointsA * 0.25 * seeker multiplier = 75_000 points
		 */
		networkPointsF := FetchAccountPoints(ctx, networkIdF)
		connect.AssertEqual(t, len(networkPointsF), 1)
		connect.AssertEqual(t, networkPointsF[0].NetworkId, networkIdF)
		connect.AssertEqual(t, networkPointsF[0].Event, string(AccountPointEventPayoutLinkedAccount))
		connect.AssertEqual(t, networkPointsF[0].PointValue, NanoPoints(float64(expectedPointsA)*0.25*subsidyConfig.SeekerHolderMultiplier))
		connect.AssertEqual(t, networkPointsF[0].PaymentPlanId, &paymentPlan.PaymentPlanId)
		connect.AssertEqual(t, networkPointsF[0].LinkedNetworkId, networkIdA)
		connect.AssertEqual(t, networkPointsF[0].AccountPaymentId, &paymentNetworkA.PaymentId)

		/**
		 * Network E child Network G should get expectedPointsA * seeker multipler * 0.125
		 */
		expectedPointsG := NanoPoints(float64(expectedPointsA) * subsidyConfig.SeekerHolderMultiplier * 0.125)
		networkPointsG := FetchAccountPoints(ctx, networkIdG)
		connect.AssertEqual(t, len(networkPointsG), 1)
		connect.AssertEqual(t, networkPointsG[0].NetworkId, networkIdG)
		connect.AssertEqual(t, networkPointsG[0].Event, string(AccountPointEventPayoutLinkedAccount))
		connect.AssertEqual(t, networkPointsG[0].PointValue, expectedPointsG)
		connect.AssertEqual(t, networkPointsG[0].PaymentPlanId, &paymentPlan.PaymentPlanId)
		connect.AssertEqual(t, networkPointsG[0].LinkedNetworkId, networkIdE)
		connect.AssertEqual(t, networkPointsG[0].AccountPaymentId, &paymentNetworkA.PaymentId)

		/**
		 * Network H should get expectedPointsB * 0.25 = 150_000 * 0.25 points = 37_500 points
		 */
		networkPointsH := FetchAccountPoints(ctx, networkIdH)
		connect.AssertEqual(t, len(networkPointsH), 1)
		connect.AssertEqual(t, networkPointsH[0].NetworkId, networkIdH)
		connect.AssertEqual(t, networkPointsH[0].Event, string(AccountPointEventPayoutLinkedAccount))
		connect.AssertEqual(t, networkPointsH[0].PointValue, NanoPoints(float64(expectedPointsB)*0.25))
		connect.AssertEqual(t, networkPointsH[0].PaymentPlanId, &paymentPlan.PaymentPlanId)
		connect.AssertEqual(t, networkPointsH[0].LinkedNetworkId, networkIdB)
		connect.AssertEqual(t, networkPointsH[0].AccountPaymentId, &paymentNetworkB.PaymentId)
	})
}

func TestReliabilityPoints(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		ctx := context.Background()

		networkIdA := server.NewId()
		clientIdA := server.NewId()

		networkIdB := server.NewId()
		clientIdB := server.NewId()
		clientIdB2 := server.NewId()

		// connect clients
		for clientId, networkId := range map[server.Id]server.Id{
			clientIdA:  networkIdA,
			clientIdB:  networkIdB,
			clientIdB2: networkIdB,
		} {
			// connect the client
			Testing_CreateDevice(
				ctx,
				networkId,
				server.NewId(),
				clientId,
				"",
				"",
			)
			clientAddress := "127.0.0.1:20000"
			handlerId := server.NewId()
			connectionId, _, _, _, err := ConnectNetworkClient(ctx, clientId, clientAddress, handlerId)
			connect.AssertEqual(t, err, nil)
			location := &Location{
				LocationType: "",
				City:         fmt.Sprintf("foo-%s", clientId),
				Region:       fmt.Sprintf("bar-%s", clientId),
				Country:      "United States",
				CountryCode:  "us",
			}
			CreateLocation(ctx, location)
			connectionLocationScores := &ConnectionLocationScores{}
			err = SetConnectionLocation(ctx, connectionId, location.LocationId, connectionLocationScores)
		}

		now := server.NowUtc()

		/**
		 * Network stats A
		 */
		ipv4 := make([]byte, 4)
		mathrand.Read(ipv4)
		ip, _ := netip.AddrFromSlice(ipv4)

		networkStatsA := &ClientReliabilityStats{
			ConnectionEstablishedCount: uint64(1),
			ProvideEnabledCount:        uint64(1),
			ReceiveMessageCount:        uint64(1),
			ReceiveByteCount:           ByteCount(1024),
			SendMessageCount:           uint64(1),
			SendByteCount:              ByteCount(1024),
		}

		networkClientAddressHashA := server.ClientIpHashForAddr(ip)
		AddClientReliabilityStats(
			ctx,
			networkIdA,
			clientIdA,
			networkClientAddressHashA,
			now,
			networkStatsA,
		)

		/**
		 * Network stats B
		 */
		ipv4 = make([]byte, 4)
		mathrand.Read(ipv4)
		ip, _ = netip.AddrFromSlice(ipv4)

		networkStatsB := &ClientReliabilityStats{
			ConnectionEstablishedCount: uint64(1),
			ProvideEnabledCount:        uint64(1),
			ReceiveMessageCount:        uint64(1),
			ReceiveByteCount:           ByteCount(1024),
			SendMessageCount:           uint64(1),
			SendByteCount:              ByteCount(1024),
		}

		networkClientAddressHashB := server.ClientIpHashForAddr(ip)
		statsTime := now.Add(-ReliabilityBlockDuration)

		/**
		 * We want to test different reliability weights
		 * network A will have 1 client, network B will have 2 clients
		 *
		 * network b client b 1
		 */
		AddClientReliabilityStats(
			ctx,
			networkIdB,
			clientIdB,
			networkClientAddressHashB,
			statsTime,
			networkStatsB,
		)

		/**
		 * network b client b 2
		 */
		statsTime = statsTime.Add(-ReliabilityBlockDuration)
		AddClientReliabilityStats(
			ctx,
			networkIdB,
			clientIdB2,
			networkClientAddressHashB,
			statsTime,
			networkStatsB,
		)

		// subsidyConfig := EnvSubsidyConfig()

		// we want to query between now and last 4 blocks
		lastPaymentTime := now.Add(-(ReliabilityBlockDuration * 4))

		reliabilitySubsidies := calculateReliabilityPayout(
			ctx,
			lastPaymentTime,
			1.0,
		)

		networkScores := GetAllNetworkReliabilityScores(ctx)

		expectedReliabilityWeightA := float64(0.2)

		connect.AssertEqual(t, networkScores[networkIdA].IndependentReliabilityScore, 1.0)

		/**
		 * calculating reliability weight for 4 blocks
		 *
		 * reliabilityWeight = SUM(1.0/w.valid_client_count) / (maxBlockNumber - minBlockNumber + 1)
		 *
		 * reliabilityWeightA = 1 / (10 - 6 + 1) = 0.2
		 * reliabilityWeightB = 2 / (10 - 6 + 1) = 0.4
		 */
		connect.AssertEqual(t, networkScores[networkIdA].ReliabilityWeight, expectedReliabilityWeightA)
		connect.AssertEqual(t, networkScores[networkIdA].ReliabilityScore, 1.0)

		connect.AssertEqual(t, networkScores[networkIdB].IndependentReliabilityScore, 2.0)
		expectedReliabilityWeightB := float64(0.4)
		connect.AssertEqual(t, networkScores[networkIdB].ReliabilityWeight, expectedReliabilityWeightB)
		connect.AssertEqual(t, networkScores[networkIdB].ReliabilityScore, 2.0)

		reliabilityPointsPerPayout := PointsToNanoPoints(float64(EnvSubsidyConfig().ReliabilityPointsPerPayout))
		totalWeight := expectedReliabilityWeightA + expectedReliabilityWeightB

		/**
		 * expected points
		 * reliabilityPointsPerPayout * (expectedReliabilityWeightA / totalWeight) = expectedPoints
		 *
		 * Network A
		 * 62_500 * (0.2 / 0.6) = 20833.3333333 * 10^6 = 20833333333 nano points
		 *
		 * Network B
		 * 62_500 * (0.4 / 0.6) = 41666.6666667 * 10^6 = 41666666667 nano points
		 */
		expectedPointsA := NanoPoints(float64(reliabilityPointsPerPayout) * expectedReliabilityWeightA / totalWeight)
		connect.AssertEqual(t, reliabilitySubsidies[networkIdA].Points, NanoPoints(expectedPointsA))

		expectedPointsB := NanoPoints(float64(reliabilityPointsPerPayout) * expectedReliabilityWeightB / totalWeight)
		connect.AssertEqual(t, reliabilitySubsidies[networkIdB].Points, NanoPoints(expectedPointsB))

		totalPoints := NanoPointsToPoints(NanoPoints(reliabilitySubsidies[networkIdA].Points) + NanoPoints(reliabilitySubsidies[networkIdB].Points))

		connect.AssertEqual(t, totalPoints, EnvSubsidyConfig().ReliabilityPointsPerPayout)

		/**
		 * USD subsidy checks
		 *
		 * reliabilitySubsidyPerPayout * (expectedReliabilityWeightA / totalWeight) = expectedPoints
		 */
		reliabilitySubsidyPerPayout := UsdToNanoCents(float64(EnvSubsidyConfig().ReliabilitySubsidyPerPayoutUsd))

		expectedUsdNetworkA := NanoCents(float64(reliabilitySubsidyPerPayout) * (expectedReliabilityWeightA / totalWeight))
		expectedUsdNetworkB := NanoCents(float64(reliabilitySubsidyPerPayout) * (expectedReliabilityWeightB / totalWeight))

		connect.AssertEqual(t, len(reliabilitySubsidies), 2)
		connect.AssertEqual(t, reliabilitySubsidies[networkIdA].Usdc, expectedUsdNetworkA)
		connect.AssertEqual(t, reliabilitySubsidies[networkIdB].Usdc, expectedUsdNetworkB)

	})
}
