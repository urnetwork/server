package controller

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/golang/glog"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
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

	wallet.NetworkId = session.ByJwt.NetworkId

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

func RemoveWallet(args *model.RemoveWalletArgs, session *session.ClientSession) (*model.RemoveWalletResult, error) {

	id, err := server.ParseId(args.WalletId)
	if err != nil {
		return &model.RemoveWalletResult{
			Success: false,
			Error: &model.RemoveWalletError{
				Message: fmt.Sprintf("Error parsing id %s", args.WalletId),
			},
		}, nil
	}

	return model.RemoveWallet(id, session), nil
}

/**
 * Seeker NFT Holder verification
 */

type HeliusAsset struct {
	Id string `json:"id"`
}

type HeliusSearchAssetsResult struct {
	Result struct {
		Items []HeliusAsset `json:"items"`
	} `json:"result"`
}

type VerifySeekerNftHolderError struct {
	Message string `json:"message"`
}

type VerifySeekerNftHolderResult struct {
	Success bool                        `json:"success"`
	Error   *VerifySeekerNftHolderError `json:"error,omitempty"`
}

type VerifySeekerNftHolderArgs struct {
	PublicKey string `json:"wallet_address,omitempty"`
	Signature string `json:"wallet_signature,omitempty"`
	Message   string `json:"wallet_message,omitempty"`
}

func VerifySeekerNftHolder(
	verify *VerifySeekerNftHolderArgs,
	session *session.ClientSession,
) (*VerifySeekerNftHolderResult, error) {

	isValid, err := model.VerifySolanaSignature(
		verify.PublicKey,
		verify.Message,
		verify.Signature,
	)

	if err != nil {
		return &VerifySeekerNftHolderResult{
			Success: false,
			Error: &VerifySeekerNftHolderError{
				Message: fmt.Sprintf("Error verifying signature %s", err.Error()),
			},
		}, err
	}
	if !isValid {
		return &VerifySeekerNftHolderResult{
			Success: false,
			Error: &VerifySeekerNftHolderError{
				Message: "Invalid signature",
			},
		}, nil
	}

	result, returnErr := heliusSearchAssets(verify.PublicKey)

	if returnErr != nil {
		return &VerifySeekerNftHolderResult{
			Success: false,
			Error: &VerifySeekerNftHolderError{
				Message: "Error fetching assets by owner",
			},
		}, returnErr
	}

	isHolder := isSeekerNftHolder(result.Result.Items)

	if !isHolder {
		return &VerifySeekerNftHolderResult{
			Success: false,
			Error: &VerifySeekerNftHolderError{
				Message: "Wallet is not a holder of the Seeker NFT",
			},
		}, nil
	}

	model.MarkWalletSeekerHolder(verify.PublicKey, session)

	return &VerifySeekerNftHolderResult{
		Success: true,
	}, nil
}

func isSeekerNftHolder(
	items []HeliusAsset,
) bool {
	seekerNftAddress := "2DMMamkkxQ6zDMBtkFp8KH7FoWzBMBA1CGTYwom4QH6Z"
	isHolder := false

	for _, item := range items {
		if item.Id == seekerNftAddress {
			isHolder = true
			break
		}

	}

	return isHolder
}

var heliusConfig = sync.OnceValue(func() map[string]any {
	c := server.Vault.RequireSimpleResource("helius.yml").Parse()
	return c["helius"].(map[string]any)
})

func heliusSearchAssets(
	publicKey string,
) (*HeliusSearchAssetsResult, error) {

	id := server.NewId()

	apiKey := heliusConfig()["api_key"].(string)

	url := fmt.Sprintf(
		"https://mainnet.helius-rpc.com/?api-key=%s",
		apiKey,
	)

	return server.HttpPostRequireStatusOk(
		url,
		map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"method":  "searchAssets",
			"params": map[string]any{
				"ownerAddress": publicKey,
				"tokenType":    "fungible",
			},
		},
		func(header http.Header) {
			header.Add("Accept", "application/json")
		},
		func(response *http.Response, responseBodyBytes []byte) (*HeliusSearchAssetsResult, error) {

			var heliusResp HeliusSearchAssetsResult
			if err := json.Unmarshal(responseBodyBytes, &heliusResp); err != nil {
				glog.Infof("error unmarshalling response: %s", err.Error())

				return nil, err
			}

			return &heliusResp, nil
		},
	)

}
