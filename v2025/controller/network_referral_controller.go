package controller

import (
	"github.com/urnetwork/server/v2025/model"
	"github.com/urnetwork/server/v2025/session"
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

	isValid := model.ValidateReferralCode(session.Ctx, setNetworkReferral.ReferralCode)
	if !isValid {
		return &SetNetworkReferralResult{
			Error: &SetNetworkReferralError{
				Message: "Invalid referral code",
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
