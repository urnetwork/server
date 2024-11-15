package controller

import (
	"github.com/urnetwork/server"
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
