package model

import (
	"context"
	"testing"

	"github.com/go-playground/assert/v2"
	"github.com/urnetwork/server/v2025"
)

func TestNetworkReferralCode(t *testing.T) {
	server.DefaultTestEnv().Run(func() {

		ctx := context.Background()
		networkId := server.NewId()

		// create a network referral code
		createdReferralCode := CreateNetworkReferralCode(ctx, networkId)
		assert.Equal(t, createdReferralCode.NetworkId, networkId)

		assert.Equal(t, len(createdReferralCode.ReferralCode), 6)

		// get the network referral code
		networkReferralCode := GetNetworkReferralCode(ctx, networkId)
		assert.Equal(t, networkReferralCode.NetworkId, networkId)
		assert.Equal(t, networkReferralCode.ReferralCode, createdReferralCode.ReferralCode)

		// get the network id by referral code
		referralNetworkId := GetNetworkIdByReferralCode(createdReferralCode.ReferralCode)
		assert.Equal(t, referralNetworkId, networkId)

		// validity checks
		invalidReferralCode := "invalid_referral_code"
		validationResult := ValidateReferralCode(ctx, invalidReferralCode)
		assert.Equal(t, validationResult.Valid, false)

		validationResult = ValidateReferralCode(ctx, createdReferralCode.ReferralCode)
		assert.Equal(t, validationResult.Valid, true)

		// check capped status
		networkIdA := server.NewId()
		networkIdB := server.NewId()
		networkIdC := server.NewId()
		networkIdD := server.NewId()
		networkIdE := server.NewId()
		networkIdF := server.NewId()

		Testing_CreateNetwork(ctx, networkIdA, "a", networkIdA)
		Testing_CreateNetwork(ctx, networkIdB, "b", networkIdB)
		Testing_CreateNetwork(ctx, networkIdC, "c", networkIdC)
		Testing_CreateNetwork(ctx, networkIdD, "d", networkIdD)
		Testing_CreateNetwork(ctx, networkIdE, "e", networkIdE)
		Testing_CreateNetwork(ctx, networkIdF, "f", networkIdF)

		CreateNetworkReferral(ctx, networkIdA, createdReferralCode.ReferralCode)
		CreateNetworkReferral(ctx, networkIdB, createdReferralCode.ReferralCode)
		CreateNetworkReferral(ctx, networkIdC, createdReferralCode.ReferralCode)
		CreateNetworkReferral(ctx, networkIdD, createdReferralCode.ReferralCode)

		validationResult = ValidateReferralCode(ctx, createdReferralCode.ReferralCode)
		assert.Equal(t, validationResult.Valid, true)
		assert.Equal(t, validationResult.IsCapped, false)

		CreateNetworkReferral(ctx, networkIdE, createdReferralCode.ReferralCode)

		validationResult = ValidateReferralCode(ctx, createdReferralCode.ReferralCode)
		assert.Equal(t, validationResult.Valid, true)
		assert.Equal(t, validationResult.IsCapped, true)

	})
}
