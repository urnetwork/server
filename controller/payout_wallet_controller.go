package controller

import (
	"errors"
	"fmt"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

/**
 * Associates the AccountWallet as the payout wallet for the network.
 */
func SetPayoutWallet(
	setWalletPayout model.SetPayoutWalletArgs,
	session *session.ClientSession,
) (*model.SetPayoutWalletResult, error) {

	networkId := session.ByJwt.NetworkId
	err := model.SetPayoutWallet(session.Ctx, networkId, setWalletPayout.WalletId)
	if err != nil {
		return nil, payoutWalletAnswer(err)
	}

	return &model.SetPayoutWalletResult{}, nil

}

// A wallet the payout cannot go to is the caller's mistake, a 400 with the
// refusal's text; any other failure is internal.
func payoutWalletAnswer(err error) error {
	if errors.Is(err, model.ErrPayoutWalletBittensor) || errors.Is(err, model.ErrPayoutWalletNotOwned) {
		return fmt.Errorf("400 %w", err)
	}
	return err
}

type GetPayoutWalletResult struct {
	WalletId *server.Id `json:"wallet_id"`
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
