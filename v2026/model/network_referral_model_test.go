package model

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
)

func TestNetworkReferral(t *testing.T) {
	skipWithoutProYml(t)

	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		ctx := context.Background()

		referralNetworkId := server.NewId()
		networkAId := server.NewId()
		networkBId := server.NewId()

		// create networks
		Testing_CreateNetwork(ctx, referralNetworkId, "referral", referralNetworkId)
		Testing_CreateNetwork(ctx, networkAId, "a", networkAId)
		Testing_CreateNetwork(ctx, networkBId, "b", networkBId)

		// networks to meet the max limit, which comes from pro.yml
		// (referral.max_referrals) -- never hardcode it here, or this test rots the
		// next time the cap changes
		maxReferrals := Pro().MaxReferrals
		fillNetworkIds := []server.Id{}
		for i := 0; i < maxReferrals; i += 1 {
			fillNetworkId := server.NewId()
			Testing_CreateNetwork(ctx, fillNetworkId, fmt.Sprintf("fill%d", i), fillNetworkId)
			fillNetworkIds = append(fillNetworkIds, fillNetworkId)
		}
		// one more, to try to exceed the cap
		exceedNetworkId := server.NewId()
		Testing_CreateNetwork(ctx, exceedNetworkId, "exceed", exceedNetworkId)

		// create a network referral code
		createdReferralCode := CreateNetworkReferralCode(ctx, referralNetworkId)
		createReferralCodeNetworkB := CreateNetworkReferralCode(ctx, networkBId)

		// create a NetworkA referral
		createdNetworkReferral := CreateNetworkReferral(ctx, networkAId, createdReferralCode.ReferralCode)
		connect.AssertEqual(t, createdNetworkReferral.NetworkId, networkAId)
		connect.AssertEqual(t, createdNetworkReferral.ReferralNetworkId, referralNetworkId)

		// get the network referral by network id
		referralNetwork := GetReferralNetworkByChildNetworkId(ctx, networkAId)
		// connect.AssertEqual(t, referralNetwork.NetworkId, networkAId)
		connect.AssertEqual(t, referralNetwork.Id, referralNetworkId)

		// create a NetworkB referral
		// to test multiple referrals count
		CreateNetworkReferral(ctx, networkBId, createdReferralCode.ReferralCode)

		// get all referrals by referral network id
		referrals := GetReferralsByReferralNetworkId(ctx, referralNetworkId)
		connect.AssertEqual(t, len(referrals), 2)
		connect.AssertEqual(t, referrals[0].NetworkId, networkAId)
		connect.AssertEqual(t, referrals[1].NetworkId, networkBId)

		// meet the limit -- networks A and B are already referred, so top up to the cap
		for i := 2; i < maxReferrals; i += 1 {
			CreateNetworkReferral(ctx, fillNetworkIds[i], createdReferralCode.ReferralCode)
		}
		referrals = GetReferralsByReferralNetworkId(ctx, referralNetworkId)
		connect.AssertEqual(t, len(referrals), maxReferrals)

		// exceed the limit -- refused, and the count does not move
		exceedLimitReferral := CreateNetworkReferral(ctx, exceedNetworkId, createdReferralCode.ReferralCode)
		connect.AssertEqual(t, exceedLimitReferral, nil)
		referrals = GetReferralsByReferralNetworkId(ctx, referralNetworkId)
		connect.AssertEqual(t, len(referrals), maxReferrals)

		// users can update their referral code
		CreateNetworkReferral(ctx, networkAId, createReferralCodeNetworkB.ReferralCode)
		referralNetwork = GetReferralNetworkByChildNetworkId(ctx, networkAId)
		connect.AssertEqual(t, referralNetwork.Id, createReferralCodeNetworkB.NetworkId)

		// remove referral code. networkA moved to networkB's code just above, so it
		// already left this referrer's list -- one short of the cap remains.
		UnlinkReferralNetwork(ctx, networkAId)
		referrals = GetReferralsByReferralNetworkId(ctx, referralNetworkId)
		connect.AssertEqual(t, len(referrals), maxReferrals-1)
		referralNetwork = GetReferralNetworkByChildNetworkId(ctx, networkAId)
		connect.AssertEqual(t, referralNetwork, nil)
	})
}

// TestAddReferralBonusesGrantsBothSides pins the payout policy: a single grant window
// credits BOTH the referrer (bonus_per_referral × its referral count) AND each referred
// network (a flat referred_bonus). Deltas are used so any balance a network already has
// does not skew the assertion.
func TestAddReferralBonusesGrantsBothSides(t *testing.T) {
	skipWithoutProYml(t)

	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		referrerId := server.NewId()
		refereeAId := server.NewId()
		refereeBId := server.NewId()
		Testing_CreateNetwork(ctx, referrerId, "referrer", referrerId)
		Testing_CreateNetwork(ctx, refereeAId, "refereeA", refereeAId)
		Testing_CreateNetwork(ctx, refereeBId, "refereeB", refereeBId)

		code := CreateNetworkReferralCode(ctx, referrerId)
		CreateNetworkReferral(ctx, refereeAId, code.ReferralCode)
		CreateNetworkReferral(ctx, refereeBId, code.ReferralCode)

		bonusPerReferral := Pro().ReferralBonus
		referredBonus := Pro().ReferredBonus

		beforeReferrer := GetActiveTransferBalanceByteCount(ctx, referrerId)
		beforeA := GetActiveTransferBalanceByteCount(ctx, refereeAId)
		beforeB := GetActiveTransferBalanceByteCount(ctx, refereeBId)

		startTime := server.NowUtc()
		endTime := startTime.Add(24 * time.Hour)
		AddReferralBonusesToAllNetworks(ctx, startTime, endTime, bonusPerReferral, referredBonus)

		// referrer earns its per-referral bonus for both referrals
		connect.AssertEqual(t, GetActiveTransferBalanceByteCount(ctx, referrerId)-beforeReferrer, bonusPerReferral*2)
		// each referee earns the flat referred bonus -- the grant that previously did not exist
		connect.AssertEqual(t, GetActiveTransferBalanceByteCount(ctx, refereeAId)-beforeA, referredBonus)
		connect.AssertEqual(t, GetActiveTransferBalanceByteCount(ctx, refereeBId)-beforeB, referredBonus)
	})
}
