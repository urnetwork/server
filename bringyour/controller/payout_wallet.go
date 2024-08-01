package controller

import (
	"bringyour.com/bringyour/model"
	"bringyour.com/bringyour/session"
)

/**
 * Associates the AccountWallet as the payout wallet for the network.
 */
func SetPayoutWallet(
	setWalletPayout model.SetPayoutWalletArgs,
	session *session.ClientSession,
) (*model.SetPayoutWalletResult, error) {

	networkId := session.ByJwt.NetworkId
	model.SetPayoutWallet(session.Ctx, networkId, setWalletPayout.WalletId)

	return &model.SetPayoutWalletResult{}, nil

}
