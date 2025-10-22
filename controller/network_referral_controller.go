package controller

import (
	"strings"

	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

type SetNetworkReferralArgs struct {
	ReferralCode string `json:"referral_code"`
}

type SetNetworkReferralError struct {
	Message string `json:"message"`
}

type SetNetworkReferralResult struct {
	Error *SetNetworkReferralError `json:"error,omitempty"`
}

/**
 * Allow users to manually set the parent network referral
 */
func SetNetworkReferral(
	setNetworkReferral *SetNetworkReferralArgs,
	session *session.ClientSession,
) (*SetNetworkReferralResult, error) {

	validationResult := model.ValidateReferralCode(session.Ctx, setNetworkReferral.ReferralCode)
	if !validationResult.Valid {
		return &SetNetworkReferralResult{
			Error: &SetNetworkReferralError{
				Message: "Invalid referral code",
			},
		}, nil
	}

	if validationResult.IsCapped {
		return &SetNetworkReferralResult{
			Error: &SetNetworkReferralError{
				Message: "Referral code has reached maximum number of referrals",
			},
		}, nil
	}

	networkReferralCode := model.GetNetworkReferralCode(session.Ctx, session.ByJwt.NetworkId)
	if networkReferralCode == nil {
		return &SetNetworkReferralResult{
			Error: &SetNetworkReferralError{
				Message: "No referral code found for the current network",
			},
		}, nil

	}

	if strings.EqualFold(networkReferralCode.ReferralCode, setNetworkReferral.ReferralCode) {
		return &SetNetworkReferralResult{
			Error: &SetNetworkReferralError{
				Message: "Cannot set the same referral code as the current network",
			},
		}, nil
	}

	networkReferral := model.CreateNetworkReferral(
		session.Ctx,
		session.ByJwt.NetworkId,
		setNetworkReferral.ReferralCode,
	)

	if networkReferral == nil {
		return &SetNetworkReferralResult{
			Error: &SetNetworkReferralError{
				Message: "Failed to create network referral",
			},
		}, nil
	}

	return &SetNetworkReferralResult{}, nil

}

/**
 * Get the referral network for the current network.
 */

type GetNetworkReferralError struct {
	Message string `json:"message"`
}

type GetNetworkReferralResult struct {
	Network *model.ReferralNetwork   `json:"network,omitempty"`
	Error   *GetNetworkReferralError `json:"error,omitempty"`
}

func GetReferralNetwork(
	session *session.ClientSession,
) (*GetNetworkReferralResult, error) {

	referralNetwork := model.GetReferralNetworkByChildNetworkId(session.Ctx, session.ByJwt.NetworkId)

	if referralNetwork == nil {
		return &GetNetworkReferralResult{
			Error: &GetNetworkReferralError{
				Message: "No referral network found",
			},
		}, nil
	}

	return &GetNetworkReferralResult{
		Network: referralNetwork,
	}, nil
}

/**
 * Unlink parent network referral
 */

type UnlinkReferralNetworkResult struct{}

func UnlinkReferralNetwork(
	session *session.ClientSession,
) (*UnlinkReferralNetworkResult, error) {
	model.UnlinkReferralNetwork(session.Ctx, session.ByJwt.NetworkId)
	return &UnlinkReferralNetworkResult{}, nil
}
