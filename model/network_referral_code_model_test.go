package model

import (
	"context"
	"fmt"
	"testing"

	"github.com/go-playground/assert/v2"
	"github.com/urnetwork/server"
)

func TestNetworkReferralCode(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

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

		// check capped status. The cap comes from pro.yml (referral.max_referrals);
		// never hardcode it here, or this test rots the next time the cap changes.
		maxReferrals := Pro().MaxReferrals

		// The rest of this test asserts the CONFIGURED cap, so it needs pro.yml. In the
		// stripped harness (WARP_CONFIG_HOME with no pro.yml) the cap is 0 -- correctly, an
		// absent spec means UNCAPPED -- and the indexing below would run off the front of
		// the slice. TestProAbsent owns the absent case.
		if maxReferrals <= 0 {
			t.Skip("pro.yml is not present in this environment; see TestProAbsent")
		}

		referredNetworkIds := []server.Id{}
		for i := 0; i < maxReferrals; i += 1 {
			referredNetworkId := server.NewId()
			Testing_CreateNetwork(ctx, referredNetworkId, fmt.Sprintf("r%d", i), referredNetworkId)
			referredNetworkIds = append(referredNetworkIds, referredNetworkId)
		}

		// one short of the cap -> not capped
		for i := 0; i < maxReferrals-1; i += 1 {
			CreateNetworkReferral(ctx, referredNetworkIds[i], createdReferralCode.ReferralCode)
		}

		validationResult = ValidateReferralCode(ctx, createdReferralCode.ReferralCode)
		assert.Equal(t, validationResult.Valid, true)
		assert.Equal(t, validationResult.IsCapped, false)

		// the one that reaches the cap -> capped
		CreateNetworkReferral(ctx, referredNetworkIds[maxReferrals-1], createdReferralCode.ReferralCode)

		validationResult = ValidateReferralCode(ctx, createdReferralCode.ReferralCode)
		assert.Equal(t, validationResult.Valid, true)
		assert.Equal(t, validationResult.IsCapped, true)

	})
}
