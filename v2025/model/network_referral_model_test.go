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

		// create a network referral code
		createdReferralCode := CreateNetworkReferralCode(ctx, referralNetworkId)

		// create a NetworkA referral
		createdNetworkReferral := CreateNetworkReferral(ctx, networkAId, &createdReferralCode.ReferralCode)
		assert.Equal(t, createdNetworkReferral.NetworkId, networkAId)
		assert.Equal(t, createdNetworkReferral.ReferralNetworkId, referralNetworkId)

		// get the network referral by network id
		networkReferral := GetNetworkReferralByNetworkId(ctx, networkAId)
		assert.Equal(t, networkReferral.NetworkId, networkAId)
		assert.Equal(t, networkReferral.ReferralNetworkId, referralNetworkId)

		// create a NetworkB referral
		// to test multiple referrals count
		CreateNetworkReferral(ctx, networkBId, &createdReferralCode.ReferralCode)

		// get all referrals by referral network id
		referrals := GetReferralsByReferralNetworkId(ctx, referralNetworkId)
		assert.Equal(t, len(referrals), 2)
		assert.Equal(t, referrals[0].NetworkId, networkAId)
		assert.Equal(t, referrals[1].NetworkId, networkBId)

	})
}
