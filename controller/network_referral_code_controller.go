package controller

import (
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

type NetworkReferralResult struct {
	ReferralCode   *string `json:"referral_code"`
	TotalReferrals int     `json:"total_referrals"`
}

func GetNetworkReferralCode(
	session *session.ClientSession,
) (*NetworkReferralResult, error) {

	res := model.GetNetworkReferralCode(session.Ctx, session.ByJwt.NetworkId)

	networkReferralsResult := model.GetReferralsByReferralNetworkId(session.Ctx, session.ByJwt.NetworkId)

	return &NetworkReferralResult{
		ReferralCode:   &res.ReferralCode,
		TotalReferrals: len(networkReferralsResult),
	}, nil

}

type ValidateNetworkReferralCodeResult struct {
	IsValid bool `json:"is_valid"`
}

type ValidateReferralCodeArgs struct {
	ReferralCode string `json:"referral_code"`
}

/**
 * When users manually enter a referral code, we want to show users whether it is valid or not.
 */
func ValidateReferralCode(
	validateReferralCode *ValidateReferralCodeArgs,
	session *session.ClientSession,
) (*ValidateNetworkReferralCodeResult, error) {

	referralCode := validateReferralCode.ReferralCode

	isValid := model.ValidateReferralCode(session.Ctx, referralCode)

	return &ValidateNetworkReferralCodeResult{
		IsValid: isValid,
	}, nil

}
