package model

import (
	"context"
	"testing"

	"bringyour.com/bringyour"
	"github.com/go-playground/assert/v2"
)

func TestNetworkReferralCode(t *testing.T) { bringyour.DefaultTestEnv().Run(func() {

	ctx := context.Background()
	networkId := bringyour.NewId()

	// create a network referral code
	createdReferralCode := CreateNetworkReferralCode(ctx, networkId)
	assert.Equal(t, createdReferralCode.NetworkId, networkId)

	// get the network referral code
	networkReferralCode := GetNetworkReferralCode(ctx, networkId)
	assert.Equal(t, networkReferralCode.NetworkId, networkId)
	assert.Equal(t, networkReferralCode.ReferralCode, createdReferralCode.ReferralCode)

})}