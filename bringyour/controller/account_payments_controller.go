package controller

import (
	"github.com/urnetwork/server/bringyour/model"
	"github.com/urnetwork/server/bringyour/session"
)

type GetNetworkAccountPaymentsError struct {
	Message string `json:"message"`
}

type GetNetworkAccountPaymentsResult struct {
	AccountPayments []*model.AccountPayment         `json:"account_payments,omitempty"`
	Error           *GetNetworkAccountPaymentsError `json:"error,omitempty"`
}

func GetNetworkAccountPayments(session *session.ClientSession) (*GetNetworkAccountPaymentsResult, error) {
	networkAccountPayments, err := model.GetNetworkPayments(session)

	if err != nil {
		return &GetNetworkAccountPaymentsResult{
			Error: &GetNetworkAccountPaymentsError{
				Message: err.Error(),
			},
		}, err
	}

	return &GetNetworkAccountPaymentsResult{
		AccountPayments: networkAccountPayments,
	}, nil
}
