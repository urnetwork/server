package controller

import (
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

type GetNetworkRedeemedBalanceCodesResult struct {
	BalanceCodes []*model.RedeemedBalanceCode         `json:"balance_codes"`
	Error        *GetNetworkRedeemedBalanceCodesError `json:"error,omitempty"`
}

type GetNetworkRedeemedBalanceCodesError struct {
	Message string `json:"message"`
}

func GetNetworkRedeemedBalanceCodes(
	session *session.ClientSession,
) (*GetNetworkRedeemedBalanceCodesResult, error) {
	balanceCodes, err := model.FetchNetworkRedeemedBalanceCodes(session)
	if err != nil {
		return &GetNetworkRedeemedBalanceCodesResult{
			Error: &GetNetworkRedeemedBalanceCodesError{
				Message: "Failed to fetch redeemed balance codes",
			},
		}, err
	}

	return &GetNetworkRedeemedBalanceCodesResult{
		BalanceCodes: balanceCodes,
	}, nil

}
