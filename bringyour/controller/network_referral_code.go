package controller

import (
	"bringyour.com/bringyour"
	"bringyour.com/bringyour/model"
	"bringyour.com/bringyour/session"
)

type NetworkReferralResult struct {
	ReferralCode bringyour.Id `json:"referralCode"`
}

func GetNetworkReferralCode(
	session *session.ClientSession,
) (*NetworkReferralResult, error) {

	res := model.GetNetworkReferralCode(session.Ctx, session.ByJwt.NetworkId)
	return &NetworkReferralResult{
		ReferralCode: res.ReferralCode,
	}, nil

}
