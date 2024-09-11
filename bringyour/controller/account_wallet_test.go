package controller

import (
	"context"
	"testing"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/jwt"
	"bringyour.com/bringyour/model"
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

		// invalid chain
		result, err := CreateAccountWalletExternal(&model.CreateAccountWalletExternalArgs{
			Blockchain: "ETH",
		}, session)
		assert.Equal(t, result, nil)
		assert.Equal(t, err, ErrInvalidBlockchain)

		// invalid address
		result, err = CreateAccountWalletExternal(&model.CreateAccountWalletExternalArgs{
			Blockchain:    "MATIC",
			WalletAddress: "1234",
		}, session)
		assert.Equal(t, result, nil)
		assert.Equal(t, err, ErrInvalidWalletAddress)

		// should have 0 wallets associated with this session
		walletResults, err := GetAccountWallets(session)

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

		_, err = CreateAccountWalletExternal(wallet, session)
		assert.Equal(t, err, nil)

		// should have 1 wallets associated with this session
		walletResults, err = GetAccountWallets(session)

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

		_, err = CreateAccountWalletExternal(wallet2, session)
		assert.Equal(t, err, nil)

		walletResults, err = GetAccountWallets(session)

		assert.Equal(t, err, nil)
		assert.Equal(t, len(walletResults.Wallets), 2)

		// payout wallet should still be the first wallet
		payoutWalletId = model.GetPayoutWalletId(ctx, networkId)
		assert.Equal(t, err, nil)
		assert.Equal(t, payoutWalletId, firstWalletId)

	})
}
