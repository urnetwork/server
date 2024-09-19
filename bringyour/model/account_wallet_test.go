package model

import (
	"context"
	"testing"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/jwt"
	"bringyour.com/bringyour/session"
	"github.com/go-playground/assert/v2"
)

func TestAccountWallet(t *testing.T) {
	bringyour.DefaultTestEnv().Run(func() {

		ctx := context.Background()
		networkId := bringyour.NewId()
		clientId := bringyour.NewId()

		session := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			ClientId:  &clientId,
		})

		args := &CreateAccountWalletExternalArgs{
			Blockchain:       "Polygon",
			WalletAddress:    "0x0",
			DefaultTokenType: "USDC",
		}

		walletId := CreateAccountWalletExternal(session, args)
		assert.NotEqual(t, walletId, nil)

		fetchWallet := GetAccountWallet(ctx, *walletId)

		assert.Equal(t, walletId, fetchWallet.WalletId)
		assert.Equal(t, networkId, fetchWallet.NetworkId)
		assert.Equal(t, WalletTypeExternal, fetchWallet.WalletType)
		assert.Equal(t, args.Blockchain, fetchWallet.Blockchain)
		assert.Equal(t, args.WalletAddress, fetchWallet.WalletAddress)
		assert.Equal(t, args.DefaultTokenType, fetchWallet.DefaultTokenType)
		assert.Equal(t, fetchWallet.CircleWalletId, nil)

		// test with setting a CircleWalletId
		circleWalletId := bringyour.NewId().String()
		circleArgs := &CreateAccountWalletCircleArgs{
			NetworkId:        networkId,
			Blockchain:       "Polygon",
			WalletAddress:    "0x0",
			DefaultTokenType: "USDC",
			CircleWalletId:   circleWalletId,
		}

		walletId = CreateAccountWalletCircle(ctx, circleArgs)

		fetchWallet = GetAccountWalletByCircleId(ctx, circleWalletId)
		assert.NotEqual(t, fetchWallet, nil)
		assert.Equal(t, fetchWallet.WalletId, walletId)
		assert.Equal(t, networkId, fetchWallet.NetworkId)
		assert.Equal(t, WalletTypeCircleUserControlled, fetchWallet.WalletType)
		assert.Equal(t, circleArgs.Blockchain, fetchWallet.Blockchain)
		assert.Equal(t, circleArgs.WalletAddress, fetchWallet.WalletAddress)
		assert.Equal(t, circleArgs.DefaultTokenType, fetchWallet.DefaultTokenType)
		assert.Equal(t, fetchWallet.CircleWalletId, circleWalletId)

		// try and fetch incorrect id
		fakeId := bringyour.NewId()
		fetchWallet = GetAccountWallet(ctx, fakeId)
		assert.Equal(t, fetchWallet, nil)
	})
}