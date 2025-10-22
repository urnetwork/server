package model

import (
	"context"
	"testing"

	"github.com/go-playground/assert/v2"
	"github.com/urnetwork/server"
)

func TestNetworkReferral(t *testing.T) {
	server.DefaultTestEnv().Run(func() {

		ctx := context.Background()

		referralNetworkId := server.NewId()
		networkAId := server.NewId()
		networkBId := server.NewId()

		// create networks
		Testing_CreateNetwork(ctx, referralNetworkId, "referral", referralNetworkId)
		Testing_CreateNetwork(ctx, networkAId, "a", networkAId)
		Testing_CreateNetwork(ctx, networkBId, "b", networkBId)

		// networks to meet max limit
		networkCId := server.NewId()
		networkDId := server.NewId()
		networkEId := server.NewId()
		networkFId := server.NewId()
		Testing_CreateNetwork(ctx, networkCId, "c", networkCId)
		Testing_CreateNetwork(ctx, networkDId, "d", networkDId)
		Testing_CreateNetwork(ctx, networkEId, "e", networkEId)
		Testing_CreateNetwork(ctx, networkFId, "f", networkFId)

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

		// meet limit
		CreateNetworkReferral(ctx, networkCId, createdReferralCode.ReferralCode)
		CreateNetworkReferral(ctx, networkDId, createdReferralCode.ReferralCode)
		CreateNetworkReferral(ctx, networkEId, createdReferralCode.ReferralCode)
		referrals = GetReferralsByReferralNetworkId(ctx, referralNetworkId)
		assert.Equal(t, len(referrals), 5)

		// exceed limit
		exceedLimitReferral := CreateNetworkReferral(ctx, networkFId, createdReferralCode.ReferralCode)
		assert.Equal(t, exceedLimitReferral, nil)
		referrals = GetReferralsByReferralNetworkId(ctx, referralNetworkId)
		assert.Equal(t, len(referrals), 5)

		// users can update their referral code
		CreateNetworkReferral(ctx, networkAId, createReferralCodeNetworkB.ReferralCode)
		referralNetwork = GetReferralNetworkByChildNetworkId(ctx, networkAId)
		assert.Equal(t, referralNetwork.Id, createReferralCodeNetworkB.NetworkId)

		// remove referral code
		UnlinkReferralNetwork(ctx, networkAId)
		referrals = GetReferralsByReferralNetworkId(ctx, referralNetworkId)
		assert.Equal(t, len(referrals), 4)
		referralNetwork = GetReferralNetworkByChildNetworkId(ctx, networkAId)
		assert.Equal(t, referralNetwork, nil)
	})
}
