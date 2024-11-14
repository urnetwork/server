package controller

import (
	"context"
	"testing"

	"github.com/go-playground/assert/v2"
	"github.com/urnetwork/server/bringyour"
	"github.com/urnetwork/server/bringyour/jwt"
	"github.com/urnetwork/server/bringyour/model"
	"github.com/urnetwork/server/bringyour/session"
)

func TestAccountWallet(t *testing.T) {
	bringyour.DefaultTestEnv().Run(func() {

		ctx := context.Background()

		networkId := bringyour.NewId()
		clientId := bringyour.NewId()

		networkIdB := bringyour.NewId()
		clientIdB := bringyour.NewId()

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
			Blockchain: "ETH",
		}, ownerSession)
		assert.Equal(t, result, nil)
		assert.Equal(t, err, ErrInvalidBlockchain)

		// invalid address
		result, err = CreateAccountWalletExternal(&model.CreateAccountWalletExternalArgs{
			Blockchain:    "MATIC",
			WalletAddress: "1234",
		}, ownerSession)
		assert.Equal(t, result, nil)
		assert.Equal(t, err, ErrInvalidWalletAddress)

		// should have 0 wallets associated with this session
		walletResults, err := GetAccountWallets(ownerSession)

		assert.Equal(t, err, nil)
		assert.Equal(t, len(walletResults.Wallets), 0)

		// payout wallet should be nil
		payoutWalletId := model.GetPayoutWalletId(ctx, networkId)
		assert.Equal(t, err, nil)
		assert.Equal(t, payoutWalletId, nil)

		// success
		wallet := &model.CreateAccountWalletExternalArgs{
			Blockchain:    "MATIC",
			WalletAddress: "0x6BC3631A507BD9f664998F4E7B039353Ce415756",
		}

		_, err = CreateAccountWalletExternal(wallet, ownerSession)
		assert.Equal(t, err, nil)

		// should have 1 wallets associated with this session
		walletResults, err = GetAccountWallets(ownerSession)

		assert.Equal(t, err, nil)
		assert.Equal(t, len(walletResults.Wallets), 1)

		firstWalletId := walletResults.Wallets[0].WalletId

		// check if a payout wallet has been created too
		payoutWalletId = model.GetPayoutWalletId(ctx, networkId)
		assert.Equal(t, err, nil)
		assert.Equal(t, payoutWalletId, firstWalletId)

		wallet2 := &model.CreateAccountWalletExternalArgs{
			Blockchain:    "MATIC",
			WalletAddress: "0x6BC3631A507BD9f664998F4E7B039353Ce415756",
		}

		_, err = CreateAccountWalletExternal(wallet2, ownerSession)
		assert.Equal(t, err, nil)

		walletResults, err = GetAccountWallets(ownerSession)

		assert.Equal(t, err, nil)
		assert.Equal(t, len(walletResults.Wallets), 2)

		// payout wallet should still be the first wallet
		payoutWalletId = model.GetPayoutWalletId(ctx, networkId)
		assert.Equal(t, err, nil)
		assert.Equal(t, payoutWalletId, firstWalletId)

		// fail with invalid wallet id string
		toRemoveArgs := &model.RemoveWalletArgs{
			WalletId: "abc",
		}

		removeResult, err := RemoveWallet(toRemoveArgs, ownerSession)
		assert.Equal(t, err, nil)
		assert.Equal(t, removeResult.Success, false)
		assert.NotEqual(t, removeResult.Error, nil)

		walletResults, err = GetAccountWallets(ownerSession)

		assert.Equal(t, err, nil)
		assert.Equal(t, len(walletResults.Wallets), 2)

		// fail removing another users wallet
		toRemoveId := walletResults.Wallets[0].WalletId
		toRemoveArgs = &model.RemoveWalletArgs{
			WalletId: toRemoveId.String(),
		}

		removeResult, err = RemoveWallet(toRemoveArgs, nonOwnerSession)
		assert.Equal(t, err, nil)
		assert.Equal(t, removeResult.Success, false)

		walletResults, err = GetAccountWallets(ownerSession)

		assert.Equal(t, err, nil)
		assert.Equal(t, len(walletResults.Wallets), 2)

		// successfully remove wallet (set active = false)
		toRemoveArgs = &model.RemoveWalletArgs{
			WalletId: toRemoveId.String(),
		}

		removeResult, err = RemoveWallet(toRemoveArgs, ownerSession)
		assert.Equal(t, err, nil)
		assert.Equal(t, removeResult.Success, true)
		assert.Equal(t, removeResult.Error, nil)

		walletResults, err = GetAccountWallets(ownerSession)

		assert.Equal(t, err, nil)
		assert.Equal(t, len(walletResults.Wallets), 1)

	})
}
