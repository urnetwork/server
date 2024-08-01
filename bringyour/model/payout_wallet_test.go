package model

import (
	"context"
	"testing"

	"bringyour.com/bringyour"
	"github.com/go-playground/assert/v2"
)

func TestPayoutWallet(t *testing.T) {
	bringyour.DefaultTestEnv().Run(func() {

		ctx := context.Background()
		sourceNetworkId := bringyour.NewId()

		walletId1 := bringyour.NewId()
		wallet1 := &CreateAccountWalletArgs{
			NetworkId:        sourceNetworkId,
			WalletType:       WalletTypeCircleUserControlled,
			Blockchain:       "matic",
			WalletAddress:    "0x0",
			DefaultTokenType: "usdc",
		}

		walletId2 := bringyour.NewId()
		wallet2 := &CreateAccountWalletArgs{
			NetworkId:        sourceNetworkId,
			WalletType:       WalletTypeCircleUserControlled,
			Blockchain:       "matic",
			WalletAddress:    "0x1",
			DefaultTokenType: "usdc",
		}

		CreateAccountWallet(ctx, walletId1, wallet1, sourceNetworkId)
		CreateAccountWallet(ctx, walletId2, wallet2, sourceNetworkId)

		SetPayoutWallet(ctx, sourceNetworkId, walletId1)

		payoutWalletId := GetPayoutWallet(ctx, sourceNetworkId)
		payoutAccountWallet := GetAccountWallet(ctx, *payoutWalletId)

		assert.Equal(t, payoutAccountWallet.WalletAddress, wallet1.WalletAddress)

		SetPayoutWallet(ctx, sourceNetworkId, walletId2)

		payoutWalletId = GetPayoutWallet(ctx, sourceNetworkId)
		payoutAccountWallet = GetAccountWallet(ctx, *payoutWalletId)

		assert.Equal(t, payoutAccountWallet.WalletAddress, wallet2.WalletAddress)
	})
}
