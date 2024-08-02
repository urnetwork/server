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
		sourceNetworkId := bringyour.NewId()
		sourceId := bringyour.NewId()

		session := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: sourceNetworkId,
			ClientId:  &sourceId,
		})

		// invalid chain
		result, err := CreateAccountWallet(&model.AccountWallet{
			Blockchain: "ETH",
		}, session)
		assert.Equal(t, result, nil)
		assert.Equal(t, err, ErrInvalidBlockchain)

		// invalid address
		result, err = CreateAccountWallet(&model.AccountWallet{
			Blockchain:    "MATIC",
			WalletAddress: "1234",
		}, session)
		assert.Equal(t, result, nil)
		assert.Equal(t, err, ErrInvalidWalletAddress)

		// success
		wallet := &model.AccountWallet{
			Blockchain:    "MATIC",
			WalletAddress: "0x6BC3631A507BD9f664998F4E7B039353Ce415756",
		}

		result, err = CreateAccountWallet(wallet, session)
		assert.Equal(t, err, nil)
		assert.Equal(t, result.WalletId, wallet.WalletId)

	})
}
