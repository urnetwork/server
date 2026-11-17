package controller

import (
	"errors"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// USDC provider payouts ended (owner decisions 2026-11-12 and 2026-11-13), so
// no wallet is selected for payouts any more. An app built before then still
// offers the choice, and shows this refusal as it shows any wallet refusal.
var ErrPayoutWalletEnded = errors.New("400 USDC payouts have ended, so a payout wallet is no longer used.")

// Refuses every selection (ErrPayoutWalletEnded).
func SetPayoutWallet(
	setWalletPayout model.SetPayoutWalletArgs,
	session *session.ClientSession,
) (*model.SetPayoutWalletResult, error) {
	return nil, ErrPayoutWalletEnded
}

// The network's payout wallet as apps read it: the one selected while USDC
// payouts ran, until that wallet is removed.
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
