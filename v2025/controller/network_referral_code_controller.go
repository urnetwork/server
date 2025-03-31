package controller

import (
	"github.com/urnetwork/server/v2025"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

type NetworkReferralResult struct {
	ReferralCode server.Id `json:"referral_code"`
}

func GetNetworkReferralCode(
	session *session.ClientSession,
) (*NetworkReferralResult, error) {

	res := model.GetNetworkReferralCode(session.Ctx, session.ByJwt.NetworkId)
	return &NetworkReferralResult{
		ReferralCode: res.ReferralCode,
	}, nil

}

type ValidateNetworkReferralCodeResult struct {
	IsValid bool `json:"is_valid"`
}

type ValidateReferralCodeArgs struct {
	ReferralCode server.Id `json:"referral_code"`
}

/**
 * When users manually enter a referral code, we want to show users whether it is valid or not.
 */
func ValidateReferralCode(
	validateReferralCode *ValidateReferralCodeArgs,
	session *session.ClientSession,
) (*ValidateNetworkReferralCodeResult, error) {

	isValid := model.ValidateReferralCode(session.Ctx, validateReferralCode.ReferralCode)

	return &ValidateNetworkReferralCodeResult{
		IsValid: isValid,
	}, nil

}
