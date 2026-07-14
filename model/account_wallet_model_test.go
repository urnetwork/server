package model

import (
	"context"
	"log"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/session"
)

func TestAccountWallet(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		ctx := context.Background()
		networkId := server.NewId()
		clientId := server.NewId()

		session := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			ClientId:  &clientId,
		})

		args := &CreateAccountWalletExternalArgs{
			Blockchain:       "MATIC",
			WalletAddress:    "0x0",
			DefaultTokenType: "USDC",
			NetworkId:        networkId,
		}

		walletId := CreateAccountWalletExternal(session, args)
		connect.AssertNotEqual(t, walletId, nil)

		fetchWallet := GetAccountWallet(ctx, *walletId)

		connect.AssertEqual(t, walletId, fetchWallet.WalletId)
		connect.AssertEqual(t, networkId, fetchWallet.NetworkId)
		connect.AssertEqual(t, WalletTypeExternal, fetchWallet.WalletType)
		connect.AssertEqual(t, args.Blockchain, fetchWallet.Blockchain)
		connect.AssertEqual(t, args.WalletAddress, fetchWallet.WalletAddress)
		connect.AssertEqual(t, args.DefaultTokenType, fetchWallet.DefaultTokenType)
		connect.AssertEqual(t, fetchWallet.CircleWalletId, nil)
		connect.AssertEqual(t, fetchWallet.HasSeekerToken, false)

		// mark wallet as having a seeker token
		MarkWalletSeekerHolder(fetchWallet.WalletAddress, session)
		fetchWallet = GetAccountWallet(ctx, *walletId)
		connect.AssertEqual(t, fetchWallet.HasSeekerToken, true)

		/**
		 * Try and insert the same wallet address with network id
		 * It should return the same wallet id as before
		 */

		walletId2 := CreateAccountWalletExternal(session, args)
		connect.AssertEqual(t, walletId, walletId2)

		// remove wallet (set account wallet as active = false)
		// we also clear the payout wallet if it matches
		SetPayoutWallet(ctx, networkId, *walletId)

		result := RemoveWallet(*walletId, session)
		connect.AssertEqual(t, result.Success, true)
		connect.AssertEqual(t, result.Error, nil)

		payoutWalletId := GetPayoutWalletId(ctx, networkId)
		connect.AssertEqual(t, payoutWalletId, nil)

		// try and fetch the wallet again
		// should be marked as inactive
		fetchWallet = GetAccountWallet(ctx, *walletId)
		connect.AssertEqual(t, fetchWallet.Active, false)

		// Create a new wallet with the same address and network id
		walletId2 = CreateAccountWalletExternal(session, args)
		connect.AssertEqual(t, walletId, walletId2)

		// fetch the wallet again
		// active should be reset to true
		fetchWallet = GetAccountWallet(ctx, *walletId)
		connect.AssertEqual(t, fetchWallet.Active, true)

		/**
		 * Associate a wallet with a seeker token that is not yet in the db
		 * This should create a new wallet with the same address and network id
		 * and set the has_seeker_token to true
		 */
		seekerHolderAddress := "0x1"
		err := MarkWalletSeekerHolder(seekerHolderAddress, session)
		connect.AssertEqual(t, err, nil)
		accountWallets := GetActiveAccountWallets(session)
		connect.AssertEqual(t, len(accountWallets.Wallets), 2)
		connect.AssertEqual(t, accountWallets.Wallets[1].WalletAddress, seekerHolderAddress)
		connect.AssertEqual(t, accountWallets.Wallets[1].NetworkId, networkId)
		connect.AssertEqual(t, accountWallets.Wallets[1].HasSeekerToken, true)
		connect.AssertEqual(t, accountWallets.Wallets[1].WalletType, WalletTypeExternal)
		connect.AssertEqual(t, accountWallets.Wallets[1].CircleWalletId, nil)
		connect.AssertEqual(t, accountWallets.Wallets[1].Active, true)
		connect.AssertEqual(t, accountWallets.Wallets[1].Blockchain, SOL.String())

		// get all seeker holders
		seekerHolders := GetAllSeekerHolders(ctx)
		connect.AssertEqual(t, len(seekerHolders), 1)
		connect.AssertEqual(t, seekerHolders[networkId], true)

		// test with setting a CircleWalletId
		circleWalletId := server.NewId().String()
		circleArgs := &CreateAccountWalletCircleArgs{
			NetworkId:        networkId,
			Blockchain:       "MATIC",
			WalletAddress:    "0x2",
			DefaultTokenType: "USDC",
			CircleWalletId:   circleWalletId,
		}

		walletId = CreateAccountWalletCircle(ctx, circleArgs)

		fetchWallet = GetAccountWalletByCircleId(ctx, circleWalletId)
		connect.AssertNotEqual(t, fetchWallet, nil)
		connect.AssertEqual(t, fetchWallet.WalletId, walletId)
		connect.AssertEqual(t, networkId, fetchWallet.NetworkId)
		connect.AssertEqual(t, WalletTypeCircleUserControlled, fetchWallet.WalletType)
		connect.AssertEqual(t, circleArgs.Blockchain, fetchWallet.Blockchain)
		connect.AssertEqual(t, circleArgs.WalletAddress, fetchWallet.WalletAddress)
		connect.AssertEqual(t, circleArgs.DefaultTokenType, fetchWallet.DefaultTokenType)
		connect.AssertEqual(t, fetchWallet.CircleWalletId, circleWalletId)
		connect.AssertEqual(t, fetchWallet.HasSeekerToken, false)

		// try and fetch incorrect id
		fakeId := server.NewId()
		fetchWallet = GetAccountWallet(ctx, fakeId)
		connect.AssertEqual(t, fetchWallet, nil)

	})
}

func TestCreateEthereumWallet(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		clientId := server.NewId()

		session := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			ClientId:  &clientId,
		})

		privateKey, err := crypto.GenerateKey()
		if err != nil {
			log.Fatal(err)
		}

		address := crypto.PubkeyToAddress(privateKey.PublicKey)

		args := &CreateAccountWalletExternalArgs{
			Blockchain:       "ETHEREUM",
			WalletAddress:    address.String(),
			DefaultTokenType: "USDC",
			NetworkId:        networkId,
		}

		walletId := CreateAccountWalletExternal(session, args)
		connect.AssertNotEqual(t, walletId, nil)
	})
}
