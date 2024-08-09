package model

import (
	"context"
	"testing"

	"bringyour.com/bringyour"
	"github.com/go-playground/assert/v2"
)

func TestAccountWallet(t *testing.T) {
	bringyour.DefaultTestEnv().Run(func() {

		ctx := context.Background()
		sourceNetworkId := bringyour.NewId()

		// create without a set Wallet Id
		// Wallet id will be generated
		wallet := &AccountWallet{
			NetworkId:        sourceNetworkId,
			WalletType:       WalletTypeCircleUserControlled,
			Blockchain:       "Polygon",
			WalletAddress:    "0x0",
			DefaultTokenType: "USDC",
		}

		CreateAccountWallet(ctx, wallet)

		assert.Equal(t, wallet.CircleWalletId, nil)

		fetchWallet := GetAccountWallet(ctx, wallet.WalletId)

		assert.Equal(t, wallet.WalletId, fetchWallet.WalletId)
		assert.Equal(t, wallet.NetworkId, fetchWallet.NetworkId)
		assert.Equal(t, wallet.WalletType, fetchWallet.WalletType)
		assert.Equal(t, wallet.Blockchain, fetchWallet.Blockchain)
		assert.Equal(t, wallet.WalletAddress, fetchWallet.WalletAddress)
		assert.Equal(t, wallet.DefaultTokenType, fetchWallet.DefaultTokenType)
		assert.Equal(t, fetchWallet.CircleWalletId, nil)

		// test with setting a CircleWalletId
		circleWalletId := bringyour.NewId().String()
		wallet = &AccountWallet{
			NetworkId:        sourceNetworkId,
			WalletType:       WalletTypeCircleUserControlled,
			Blockchain:       "Polygon",
			WalletAddress:    "0x0",
			DefaultTokenType: "USDC",
			CircleWalletId:   &circleWalletId,
		}

		CreateAccountWallet(ctx, wallet)

		fetchWallet = GetAccountWalletByCircleId(ctx, circleWalletId)
		assert.NotEqual(t, fetchWallet, nil)
		assert.Equal(t, fetchWallet.WalletId, wallet.WalletId)
		assert.Equal(t, fetchWallet.CircleWalletId, wallet.CircleWalletId)

		// try and fetch incorrect id
		fakeId := bringyour.NewId()
		fetchWallet = GetAccountWallet(ctx, fakeId)
		assert.Equal(t, fetchWallet, nil)
	})
}
