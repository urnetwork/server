package model

import (
	"context"
	"fmt"
	"testing"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
)

func TestNetworkReferralCode(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		ctx := context.Background()
		networkId := server.NewId()

		// create a network referral code
		createdReferralCode := CreateNetworkReferralCode(ctx, networkId)
		connect.AssertEqual(t, createdReferralCode.NetworkId, networkId)

		connect.AssertEqual(t, len(createdReferralCode.ReferralCode), 6)

		// get the network referral code
		networkReferralCode := GetNetworkReferralCode(ctx, networkId)
		connect.AssertEqual(t, networkReferralCode.NetworkId, networkId)
		connect.AssertEqual(t, networkReferralCode.ReferralCode, createdReferralCode.ReferralCode)

		// get the network id by referral code
		referralNetworkId := GetNetworkIdByReferralCode(createdReferralCode.ReferralCode)
		connect.AssertEqual(t, referralNetworkId, networkId)

		// validity checks
		invalidReferralCode := "invalid_referral_code"
		validationResult := ValidateReferralCode(ctx, invalidReferralCode)
		connect.AssertEqual(t, validationResult.Valid, false)

		validationResult = ValidateReferralCode(ctx, createdReferralCode.ReferralCode)
		connect.AssertEqual(t, validationResult.Valid, true)

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
		connect.AssertEqual(t, validationResult.Valid, true)
		connect.AssertEqual(t, validationResult.IsCapped, false)

		// the one that reaches the cap -> capped
		CreateNetworkReferral(ctx, referredNetworkIds[maxReferrals-1], createdReferralCode.ReferralCode)

		validationResult = ValidateReferralCode(ctx, createdReferralCode.ReferralCode)
		connect.AssertEqual(t, validationResult.Valid, true)
		connect.AssertEqual(t, validationResult.IsCapped, true)

	})
}
