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

		wallet1 := &AccountWallet{
			NetworkId:        sourceNetworkId,
			WalletType:       WalletTypeCircleUserControlled,
			Blockchain:       "matic",
			WalletAddress:    "0x0",
			DefaultTokenType: "usdc",
		}

		wallet2 := &AccountWallet{
			NetworkId:        sourceNetworkId,
			WalletType:       WalletTypeCircleUserControlled,
			Blockchain:       "matic",
			WalletAddress:    "0x1",
			DefaultTokenType: "usdc",
		}

		CreateAccountWallet(ctx, wallet1)
		CreateAccountWallet(ctx, wallet2)

		SetPayoutWallet(ctx, sourceNetworkId, wallet1.WalletId)

		payoutWalletId := GetPayoutWallet(ctx, sourceNetworkId)
		payoutAccountWallet := GetAccountWallet(ctx, *payoutWalletId)

		assert.Equal(t, payoutAccountWallet.WalletAddress, wallet1.WalletAddress)

		SetPayoutWallet(ctx, sourceNetworkId, wallet2.WalletId)

		payoutWalletId = GetPayoutWallet(ctx, sourceNetworkId)
		payoutAccountWallet = GetAccountWallet(ctx, *payoutWalletId)

		assert.Equal(t, payoutAccountWallet.WalletAddress, wallet2.WalletAddress)
	})
}
