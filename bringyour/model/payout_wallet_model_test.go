package model

import (
	"context"
	"testing"

	"github.com/go-playground/assert/v2"
	"github.com/urnetwork/server/bringyour"
	"github.com/urnetwork/server/bringyour/jwt"
	"github.com/urnetwork/server/bringyour/session"
)

func TestPayoutWallet(t *testing.T) {
	bringyour.DefaultTestEnv().Run(func() {

		ctx := context.Background()

		networkId := bringyour.NewId()
		clientId := bringyour.NewId()

		session := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			ClientId:  &clientId,
		})

		wallet1 := &CreateAccountWalletExternalArgs{
			Blockchain:       "matic",
			WalletAddress:    "0x0",
			DefaultTokenType: "usdc",
		}

		wallet2 := &CreateAccountWalletExternalArgs{
			Blockchain:       "matic",
			WalletAddress:    "0x1",
			DefaultTokenType: "usdc",
		}

		walletId1 := CreateAccountWalletExternal(session, wallet1)
		walletId2 := CreateAccountWalletExternal(session, wallet2)
		assert.NotEqual(t, walletId1, nil)
		assert.NotEqual(t, walletId2, nil)

		SetPayoutWallet(ctx, networkId, *walletId1)

		payoutWalletId := GetPayoutWalletId(ctx, networkId)
		payoutAccountWallet := GetAccountWallet(ctx, *payoutWalletId)

		assert.Equal(t, payoutAccountWallet.WalletAddress, wallet1.WalletAddress)

		SetPayoutWallet(ctx, networkId, *walletId2)

		payoutWalletId = GetPayoutWalletId(ctx, networkId)
		payoutAccountWallet = GetAccountWallet(ctx, *payoutWalletId)

		assert.Equal(t, payoutAccountWallet.WalletAddress, wallet2.WalletAddress)

		deletePayoutWallet(*payoutWalletId, session)
		payoutWalletId = GetPayoutWalletId(ctx, networkId)
		assert.Equal(t, payoutWalletId, nil)

	})
}
