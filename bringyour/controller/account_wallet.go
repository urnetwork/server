package controller

import (
	"errors"
	"fmt"
	"strings"

	"bringyour.com/bringyour/model"
	"bringyour.com/bringyour/session"
)

type AccountErrorMessage string

var (
	ErrInvalidBlockchain    = errors.New("invalid blockchain, use SOL or MATIC")
	ErrInvalidWalletAddress = errors.New("invalid wallet address")
)

// used for creating external wallets
func CreateAccountWalletExternal(
	wallet *model.CreateAccountWalletExternalArgs,
	session *session.ClientSession,
) (*model.CreateAccountWalletResult, error) {

	blockchain, err := model.ParseBlockchain(strings.ToUpper(wallet.Blockchain))
	if err != nil {
		return nil, ErrInvalidBlockchain
	}

	wallet.Blockchain = blockchain.String()

	walletValidateAddressArgs := WalletValidateAddressArgs{
		Address: wallet.WalletAddress,
		Chain:   wallet.Blockchain,
	}
	validationResult, err := WalletValidateAddress(&walletValidateAddressArgs, session)
	if err != nil {
		return nil, err
	}

	if !validationResult.Valid {
		return nil, ErrInvalidWalletAddress
	}

	walletId := model.CreateAccountWalletExternal(session, wallet)

	if walletId == nil {
		return nil, fmt.Errorf("error creating new wallet")
	}

	// check if a payout wallet is set for this network
	payoutWallet := model.GetPayoutWalletId(session.Ctx, session.ByJwt.NetworkId)

	// if a payout wallet doesn't exist for the network
	// set payout wallet
	if payoutWallet == nil {
		model.SetPayoutWallet(session.Ctx, session.ByJwt.NetworkId, *walletId)
	}

	return &model.CreateAccountWalletResult{WalletId: *walletId}, nil
}

func GetAccountWallets(session *session.ClientSession) (*model.GetAccountWalletsResult, error) {
	walletsResult := model.GetActiveAccountWallets(session)
	return walletsResult, nil
}
