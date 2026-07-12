package model

import (
	"context"
	"fmt"
	"testing"

	"github.com/go-playground/assert/v2"
	"github.com/urnetwork/server"
)

func TestNetworkReferral(t *testing.T) {
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
		assert.Equal(t, createdNetworkReferral.NetworkId, networkAId)
		assert.Equal(t, createdNetworkReferral.ReferralNetworkId, referralNetworkId)

		// get the network referral by network id
		referralNetwork := GetReferralNetworkByChildNetworkId(ctx, networkAId)
		// assert.Equal(t, referralNetwork.NetworkId, networkAId)
		assert.Equal(t, referralNetwork.Id, referralNetworkId)

		// create a NetworkB referral
		// to test multiple referrals count
		CreateNetworkReferral(ctx, networkBId, createdReferralCode.ReferralCode)

		// get all referrals by referral network id
		referrals := GetReferralsByReferralNetworkId(ctx, referralNetworkId)
		assert.Equal(t, len(referrals), 2)
		assert.Equal(t, referrals[0].NetworkId, networkAId)
		assert.Equal(t, referrals[1].NetworkId, networkBId)

		// meet the limit -- networks A and B are already referred, so top up to the cap
		for i := 2; i < maxReferrals; i += 1 {
			CreateNetworkReferral(ctx, fillNetworkIds[i], createdReferralCode.ReferralCode)
		}
		referrals = GetReferralsByReferralNetworkId(ctx, referralNetworkId)
		assert.Equal(t, len(referrals), maxReferrals)

		// exceed the limit -- refused, and the count does not move
		exceedLimitReferral := CreateNetworkReferral(ctx, exceedNetworkId, createdReferralCode.ReferralCode)
		assert.Equal(t, exceedLimitReferral, nil)
		referrals = GetReferralsByReferralNetworkId(ctx, referralNetworkId)
		assert.Equal(t, len(referrals), maxReferrals)

		// users can update their referral code
		CreateNetworkReferral(ctx, networkAId, createReferralCodeNetworkB.ReferralCode)
		referralNetwork = GetReferralNetworkByChildNetworkId(ctx, networkAId)
		assert.Equal(t, referralNetwork.Id, createReferralCodeNetworkB.NetworkId)

		// remove referral code. networkA moved to networkB's code just above, so it
		// already left this referrer's list -- one short of the cap remains.
		UnlinkReferralNetwork(ctx, networkAId)
		referrals = GetReferralsByReferralNetworkId(ctx, referralNetworkId)
		assert.Equal(t, len(referrals), maxReferrals-1)
		referralNetwork = GetReferralNetworkByChildNetworkId(ctx, networkAId)
		assert.Equal(t, referralNetwork, nil)
	})
}
