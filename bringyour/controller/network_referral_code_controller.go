package controller

import (
	"github.com/urnetwork/server/bringyour"
	"github.com/urnetwork/server/bringyour/model"
	"github.com/urnetwork/server/bringyour/session"
)

type NetworkReferralResult struct {
	ReferralCode bringyour.Id `json:"referral_code"`
}

func GetNetworkReferralCode(
	session *session.ClientSession,
) (*NetworkReferralResult, error) {

	res := model.GetNetworkReferralCode(session.Ctx, session.ByJwt.NetworkId)
	return &NetworkReferralResult{
		ReferralCode: res.ReferralCode,
	}, nil

}
