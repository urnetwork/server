package model

import (
	"context"
	"testing"

	"github.com/go-playground/assert/v2"
	"github.com/urnetwork/server"
)

func TestNetworkReferralCode(t *testing.T) {
	server.DefaultTestEnv().Run(func() {

		ctx := context.Background()
		networkId := server.NewId()

		// create a network referral code
		createdReferralCode := CreateNetworkReferralCode(ctx, networkId)
		assert.Equal(t, createdReferralCode.NetworkId, networkId)

		// get the network referral code
		networkReferralCode := GetNetworkReferralCode(ctx, networkId)
		assert.Equal(t, networkReferralCode.NetworkId, networkId)
		assert.Equal(t, networkReferralCode.ReferralCode, createdReferralCode.ReferralCode)

	})
}
