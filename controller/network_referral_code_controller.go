package controller

import (
	"fmt"
	"time"

	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

type NetworkReferralResult struct {
	ReferralCode   *string `json:"referral_code"`
	TotalReferrals int     `json:"total_referrals"`
	// The referral program terms from pro.yml (the single source of truth for
	// these numbers), so every app and the site show the same cap and bonus
	// instead of hardcoding their own. All zero when pro.yml is absent (no cap,
	// no grant); clients treat zero as "unknown" and keep their display defaults.
	MaxReferrals          int   `json:"max_referrals"`
	BonusPerReferralBytes int64 `json:"bonus_per_referral_bytes"`
	ReferredBonusBytes    int64 `json:"referred_bonus_bytes"`
	BonusPeriodSeconds    int64 `json:"bonus_period_seconds"`
}

func GetNetworkReferralCode(
	session *session.ClientSession,
) (*NetworkReferralResult, error) {

	res := model.GetNetworkReferralCode(session.Ctx, session.ByJwt.NetworkId)
	if res == nil {
		return nil, fmt.Errorf("Missing referral code.")
	}

	networkReferralsResult := model.GetReferralsByReferralNetworkId(session.Ctx, session.ByJwt.NetworkId)

	pro := model.Pro()

	return &NetworkReferralResult{
		ReferralCode:          &res.ReferralCode,
		TotalReferrals:        len(networkReferralsResult),
		MaxReferrals:          pro.MaxReferrals,
		BonusPerReferralBytes: pro.ReferralBonus,
		ReferredBonusBytes:    pro.ReferredBonus,
		BonusPeriodSeconds:    int64(pro.ReferralPeriod / time.Second),
	}, nil

}

type ValidateNetworkReferralCodeResult struct {
	IsValid  bool                       `json:"is_valid"`
	IsCapped bool                       `json:"is_capped"`
	Error    *ValidateReferralCodeError `json:"error,omitempty"`
}

type ValidateReferralCodeError struct {
	Message string `json:"message"`
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

	validationResult := model.ValidateReferralCode(session.Ctx, referralCode)

	return &ValidateNetworkReferralCodeResult{
		IsValid:  validationResult.Valid,
		IsCapped: validationResult.IsCapped,
	}, nil

}
