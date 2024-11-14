package controller

import (
	"github.com/urnetwork/server/bringyour"
	"github.com/urnetwork/server/bringyour/model"
	"github.com/urnetwork/server/bringyour/session"
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

type GetPayoutWalletResult struct {
	WalletId *bringyour.Id `json:"wallet_id"`
}

func GetPayoutWallet(
	session *session.ClientSession,
) (*GetPayoutWalletResult, error) {
	networkId := session.ByJwt.NetworkId
	walletId := model.GetPayoutWalletId(session.Ctx, networkId)

	return &GetPayoutWalletResult{
		WalletId: walletId,
	}, nil

}
