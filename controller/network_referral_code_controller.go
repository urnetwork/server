package controller

import (
	"github.com/urnetwork/server"
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
	ReferralCode    *server.Id `json:"referral_code"`
	ReferralCodeStr string     `json:"referral_code_str"`
}

/**
 * When users manually enter a referral code, we want to show users whether it is valid or not.
 */
func ValidateReferralCode(
	validateReferralCode *ValidateReferralCodeArgs,
	session *session.ClientSession,
) (*ValidateNetworkReferralCodeResult, error) {

	var referralCode string
	if validateReferralCode.ReferralCode != nil {
		referralCode = validateReferralCode.ReferralCode.String()
	}

	if validateReferralCode.ReferralCodeStr != "" {
		referralCode = validateReferralCode.ReferralCodeStr
	}

	isValid := model.ValidateReferralCode(session.Ctx, referralCode)

	return &ValidateNetworkReferralCodeResult{
		IsValid: isValid,
	}, nil

}
