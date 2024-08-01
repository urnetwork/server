package controller

import (
	"errors"

	"bringyour.com/bringyour/model"
	"bringyour.com/bringyour/session"
)

// used for creating external wallets
func CreateAccountWallet(
	wallet *model.CreateAccountWalletArgs,
	session *session.ClientSession,
) (*model.CreateAccountWalletResult, error) {

	if wallet.Blockchain != "SOL" && wallet.Blockchain != "MATIC" {
		return nil, errors.New("invalid blockchain, use SOL or MATIC")
	}

	walletValidateAddressArgs := WalletValidateAddressArgs{
		Address: wallet.WalletAddress,
		Chain:   wallet.Blockchain,
	}
	validationResult, err := WalletValidateAddress(&walletValidateAddressArgs, session)
	if err != nil {
		return nil, err
	}

	if !validationResult.Valid {
		return nil, errors.New("invalid wallet address")
	}

	model.CreateAccountWallet(session.Ctx, wallet, session.ByJwt.NetworkId)

	return &model.CreateAccountWalletResult{WalletId: wallet.WalletId}, nil
}
