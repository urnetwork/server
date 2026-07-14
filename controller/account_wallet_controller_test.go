package controller

import (
	"context"
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

func TestAccountWallet(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		ctx := context.Background()

		networkId := server.NewId()
		clientId := server.NewId()

		networkIdB := server.NewId()
		clientIdB := server.NewId()

		ownerSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			ClientId:  &clientId,
		})

		nonOwnerSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkIdB,
			ClientId:  &clientIdB,
		})

		// invalid chain
		result, err := CreateAccountWalletExternal(&model.CreateAccountWalletExternalArgs{
			Blockchain: "BTC",
		}, ownerSession)
		connect.AssertEqual(t, result, nil)
		connect.AssertEqual(t, err, ErrInvalidBlockchain)

		// invalid address
		result, err = CreateAccountWalletExternal(&model.CreateAccountWalletExternalArgs{
			Blockchain:    "SOL",
			WalletAddress: "1234",
		}, ownerSession)
		connect.AssertEqual(t, result, nil)
		connect.AssertEqual(t, err, ErrInvalidWalletAddress)

		// should have 0 wallets associated with this session
		walletResults, err := GetAccountWallets(ownerSession)

		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(walletResults.Wallets), 0)

		// payout wallet should be nil
		payoutWalletId := model.GetPayoutWalletId(ctx, networkId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, payoutWalletId, nil)

		// success
		wallet := &model.CreateAccountWalletExternalArgs{
			Blockchain:    "SOL",
			WalletAddress: "74UNdYRpvakSABaYHSZMQNaXBVtA6eY9Nt8chcqocKe7",
		}

		_, err = CreateAccountWalletExternal(wallet, ownerSession)
		connect.AssertEqual(t, err, nil)

		// should have 1 wallets associated with this session
		walletResults, err = GetAccountWallets(ownerSession)

		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(walletResults.Wallets), 1)

		firstWalletId := walletResults.Wallets[0].WalletId

		// check if a payout wallet has been created too
		payoutWalletId = model.GetPayoutWalletId(ctx, networkId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, payoutWalletId, firstWalletId)

		wallet2 := &model.CreateAccountWalletExternalArgs{
			Blockchain:    "SOL",
			WalletAddress: "74UNdYRpvakSABaYHSZMQNaXBVtA6eY9Nt8chcqocKe8",
		}

		_, err = CreateAccountWalletExternal(wallet2, ownerSession)
		connect.AssertEqual(t, err, nil)

		walletResults, err = GetAccountWallets(ownerSession)

		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(walletResults.Wallets), 2)

		// payout wallet should still be the first wallet
		payoutWalletId = model.GetPayoutWalletId(ctx, networkId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, payoutWalletId, firstWalletId)

		// fail with invalid wallet id string
		toRemoveArgs := &model.RemoveWalletArgs{
			WalletId: "abc",
		}

		removeResult, err := RemoveWallet(toRemoveArgs, ownerSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, removeResult.Success, false)
		connect.AssertNotEqual(t, removeResult.Error, nil)

		walletResults, err = GetAccountWallets(ownerSession)

		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(walletResults.Wallets), 2)

		// fail removing another users wallet
		toRemoveId := walletResults.Wallets[0].WalletId
		toRemoveArgs = &model.RemoveWalletArgs{
			WalletId: toRemoveId.String(),
		}

		removeResult, err = RemoveWallet(toRemoveArgs, nonOwnerSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, removeResult.Success, false)

		walletResults, err = GetAccountWallets(ownerSession)

		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(walletResults.Wallets), 2)

		// successfully remove wallet (set active = false)
		toRemoveArgs = &model.RemoveWalletArgs{
			WalletId: toRemoveId.String(),
		}

		removeResult, err = RemoveWallet(toRemoveArgs, ownerSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, removeResult.Success, true)
		connect.AssertEqual(t, removeResult.Error, nil)

		walletResults, err = GetAccountWallets(ownerSession)

		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(walletResults.Wallets), 1)

	})
}

func TestSeekerNFTVerification(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		ctx := context.Background()

		sagaHolderPublicKey := "FhWtLQZ7Fefy6Mp7Yp9CnFgQjfw6N4a3Y8r5qw888DkB"
		seekerPreorderHolderPublicKey := "JBSmKmcTMRSM7mmLZd2do6PMpd9eAtKgKC9a4UtYwLuE"
		seekerGenesisHolderPublicKey := "CwrNT8btVQNbb2ob1k81sV652wdmJB6CgWTVBNT5GJjC"
		nonHolderPublicKey := "D8e7nNaqdkMymmD3uNLp1KB2Y9K7PUdYYXrRXauvn94N"

		/**
		 * Verify Saga NFT Holder
		 */
		result, err := heliusSearchAssetsSaga(ctx, sagaHolderPublicKey)
		connect.AssertEqual(t, err, nil)
		isHolder := isSagaNftHolder(result.Result.Items)
		connect.AssertEqual(t, isHolder, true)

		/**
		 * Verify non Saga NFT Holder
		 */
		result, err = heliusSearchAssetsSaga(ctx, nonHolderPublicKey)
		connect.AssertEqual(t, err, nil)
		isHolder = isSagaNftHolder(result.Result.Items)
		connect.AssertEqual(t, isHolder, false)

		/**
		 * Verify Seeker Preorder NFT Holder
		 */
		items, err := heliusSearchAssets(ctx, seekerPreorderHolderPublicKey)
		connect.AssertEqual(t, err, nil)

		isHolder = isSeekerNftHolder(items)
		connect.AssertEqual(t, isHolder, true)

		/**
		 * Verify Seeker Genesis Holder
		 */
		items, err = heliusSearchAssets(ctx, seekerGenesisHolderPublicKey)
		connect.AssertEqual(t, err, nil)

		isHolder = isSeekerNftHolder(items)
		connect.AssertEqual(t, isHolder, true)

		/**
		 * Verify non Seeker Preorder or Genesis NFT Holder
		 */

		items, err = heliusSearchAssets(ctx, nonHolderPublicKey)
		connect.AssertEqual(t, err, nil)
		isHolder = isSeekerNftHolder(result.Result.Items)
		connect.AssertEqual(t, isHolder, false)

	})
}
