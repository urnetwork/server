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

	return &model.CreateAccountWalletResult{WalletId: *walletId}, nil
}

func GetAccountWallets(session *session.ClientSession) (*model.GetAccountWalletsResult, error) {
	walletsResult := model.GetActiveAccountWallets(session)
	return walletsResult, nil
}