package model

import (
	"context"
	"testing"

	"github.com/go-playground/assert/v2"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/session"
)

func TestAccountWallet(t *testing.T) {
	server.DefaultTestEnv().Run(func() {

		ctx := context.Background()
		networkId := server.NewId()
		clientId := server.NewId()

		session := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			ClientId:  &clientId,
		})

		args := &CreateAccountWalletExternalArgs{
			Blockchain:       "Polygon",
			WalletAddress:    "0x0",
			DefaultTokenType: "USDC",
			NetworkId:        networkId,
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
		assert.Equal(t, fetchWallet.HasSeekerToken, false)

		// mark wallet as having a seeker token
		MarkWalletSeekerHolder(fetchWallet.WalletAddress, session)
		fetchWallet = GetAccountWallet(ctx, *walletId)
		assert.Equal(t, fetchWallet.HasSeekerToken, true)

		/**
		 * Try and insert the same wallet address with network id
		 * It should return the same wallet id as before
		 */

		walletId2 := CreateAccountWalletExternal(session, args)
		assert.Equal(t, walletId, walletId2)

		// remove wallet (set account wallet as active = false)
		// we also clear the payout wallet if it matches
		SetPayoutWallet(ctx, networkId, *walletId)

		result := RemoveWallet(*walletId, session)
		assert.Equal(t, result.Success, true)
		assert.Equal(t, result.Error, nil)

		payoutWalletId := GetPayoutWalletId(ctx, networkId)
		assert.Equal(t, payoutWalletId, nil)

		// try and fetch the wallet again
		// should be marked as inactive
		fetchWallet = GetAccountWallet(ctx, *walletId)
		assert.Equal(t, fetchWallet.Active, false)

		// Create a new wallet with the same address and network id
		walletId2 = CreateAccountWalletExternal(session, args)
		assert.Equal(t, walletId, walletId2)

		// fetch the wallet again
		// active should be reset to true
		fetchWallet = GetAccountWallet(ctx, *walletId)
		assert.Equal(t, fetchWallet.Active, true)

		/**
		 * Associate a wallet with a seeker token that is not yet in the db
		 * This should create a new wallet with the same address and network id
		 * and set the has_seeker_token to true
		 */
		seekerHolderAddress := "0x1"
		err := MarkWalletSeekerHolder(seekerHolderAddress, session)
		assert.Equal(t, err, nil)
		accountWallets := GetActiveAccountWallets(session)
		assert.Equal(t, len(accountWallets.Wallets), 2)
		assert.Equal(t, accountWallets.Wallets[1].WalletAddress, seekerHolderAddress)
		assert.Equal(t, accountWallets.Wallets[1].NetworkId, networkId)
		assert.Equal(t, accountWallets.Wallets[1].HasSeekerToken, true)
		assert.Equal(t, accountWallets.Wallets[1].WalletType, WalletTypeExternal)
		assert.Equal(t, accountWallets.Wallets[1].CircleWalletId, nil)
		assert.Equal(t, accountWallets.Wallets[1].Active, true)
		assert.Equal(t, accountWallets.Wallets[1].Blockchain, SOL.String())

		// get all seeker holders
		seekerHolders := GetAllSeekerHolders(ctx)
		assert.Equal(t, len(seekerHolders), 1)
		assert.Equal(t, seekerHolders[networkId], true)

		// test with setting a CircleWalletId
		circleWalletId := server.NewId().String()
		circleArgs := &CreateAccountWalletCircleArgs{
			NetworkId:        networkId,
			Blockchain:       "Polygon",
			WalletAddress:    "0x2",
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
		assert.Equal(t, fetchWallet.HasSeekerToken, false)

		// try and fetch incorrect id
		fakeId := server.NewId()
		fetchWallet = GetAccountWallet(ctx, fakeId)
		assert.Equal(t, fetchWallet, nil)

	})
}
