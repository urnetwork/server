// The payout history apps read. USDC provider payouts ended (owner decisions
// 2026-11-12 and 2026-11-13: "settle closed rows, let's close out usdc
// entirely"): nothing plans or sends a payment any more, and these keep
// answering from the payment rows, as before, for every app that reads them.
package controller

import (
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

type GetNetworkAccountPaymentsError struct {
	Message string `json:"message"`
}

type GetNetworkAccountPaymentsResult struct {
	Asset           string                          `json:"asset"`
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
		Asset:           "USDC",
		AccountPayments: networkAccountPayments,
	}, nil
}

func TransferStats(session *session.ClientSession) (*model.TransferStats, error) {
	return model.GetTransferStats(session.Ctx, session.ByJwt.NetworkId), nil
}
