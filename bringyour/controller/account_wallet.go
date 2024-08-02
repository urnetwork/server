package controller

import (
	"errors"

	"bringyour.com/bringyour/model"
	"bringyour.com/bringyour/session"
)

type AccountErrorMessage string

var (
	ErrInvalidBlockchain    = errors.New("invalid blockchain, use SOL or MATIC")
	ErrInvalidWalletAddress = errors.New("invalid wallet address")
)

// used for creating external wallets
func CreateAccountWallet(
	wallet *model.AccountWallet,
	session *session.ClientSession,
) (*model.CreateAccountWalletResult, error) {

	if wallet.Blockchain != "SOL" && wallet.Blockchain != "MATIC" {
		return nil, ErrInvalidBlockchain
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
		return nil, ErrInvalidWalletAddress
	}

	wallet.NetworkId = session.ByJwt.NetworkId

	model.CreateAccountWallet(session.Ctx, wallet)

	return &model.CreateAccountWalletResult{WalletId: wallet.WalletId}, nil
}
