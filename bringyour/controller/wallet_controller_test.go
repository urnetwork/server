package controller

import (
	"context"
	"testing"
	"time"

	// "golang.org/x/exp/maps"

	"github.com/go-playground/assert/v2"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/jwt"
	"bringyour.com/bringyour/model"
	"bringyour.com/bringyour/session"
)

// these were set up manually on main
// var circleUserIdWithWallet = bringyour.RequireParseId("018c3c3c-8265-1b71-e827-902beb3233c4")
var circleUserIdWithWallet = bringyour.RequireParseId("018c4b12-1a76-aaca-acce-72ddae03f60d")
var circleUserIdWithWalletAndBalance = bringyour.RequireParseId("018c3c7f-82f3-341b-6fd9-fe8d180c366c")

func TestWalletCircleInit(t *testing.T) {
	bringyour.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		session := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId:   bringyour.NewId(),
			NetworkName: "test",
			UserId:      bringyour.NewId(),
		})
		result, err := WalletCircleInit(session)

		assert.Equal(t, err, nil)
		assert.Equal(t, result.Error, nil)
		assert.NotEqual(t, result.UserToken, nil)
		assert.NotEqual(t, result.ChallengeId, "")

		// a second init should not create an error
		result, err = WalletCircleInit(session)

		assert.Equal(t, err, nil)
		assert.Equal(t, result.Error, nil)
		assert.NotEqual(t, result.UserToken, nil)
		assert.NotEqual(t, result.ChallengeId, "")
	})
}

func TestWalletValidateAddress(t *testing.T) {
	bringyour.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		session := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId:   bringyour.NewId(),
			NetworkName: "test",
			UserId:      bringyour.NewId(),
		})
		result, err := WalletCircleInit(session)

		assert.Equal(t, err, nil)
		assert.Equal(t, result.Error, nil)
		assert.NotEqual(t, result.UserToken, nil)
		assert.NotEqual(t, result.ChallengeId, "")

		// test valid MATIC address
		validateResult, err := WalletValidateAddress(
			&WalletValidateAddressArgs{
				// BringYour USDC Polygon
				Address: "0xB3f448b9C395F9833BE866577254799c23BBa682",
				Chain:   "MATIC",
			},
			session,
		)

		assert.Equal(t, err, nil)
		assert.Equal(t, validateResult.Valid, true)

		// test valid SOL address
		validateResult, err = WalletValidateAddress(
			&WalletValidateAddressArgs{
				Address: "DgTYzxzYRpkGQ8e3Un71GoQf494VLDBnyqXNXB38MP73",
				Chain:   "SOL",
			},
			session,
		)

		assert.Equal(t, err, nil)
		assert.Equal(t, validateResult.Valid, true)

		// test invalid address
		validateResult, err = WalletValidateAddress(
			&WalletValidateAddressArgs{
				// BringYour USDC Polygon
				Address: "0xB3f448b9C395F9833BE866577254799c23BBa682",
				Chain:   "SOL",
			},
			session,
		)
		assert.Equal(t, err, nil)
		assert.Equal(t, validateResult.Valid, false)
	})
}

func TestWalletBalance(t *testing.T) {
	bringyour.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		session := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId:   bringyour.NewId(),
			NetworkName: "test",
			UserId:      bringyour.NewId(),
		})

		model.SetCircleUserId(
			ctx,
			session.ByJwt.NetworkId,
			session.ByJwt.UserId,
			circleUserIdWithWallet,
		)

		result, err := WalletBalance(session)

		assert.Equal(t, err, nil)
		assert.NotEqual(t, result.WalletInfo, nil)
		assert.NotEqual(t, result.WalletInfo.WalletId, "")
		assert.NotEqual(t, result.WalletInfo.CreateDate, time.Time{})
		// the wallet is empty so these are the defaults
		assert.Equal(t, result.WalletInfo.Blockchain, "Polygon")
		assert.Equal(t, result.WalletInfo.BlockchainSymbol, "MATIC")
		assert.Equal(t, result.WalletInfo.TokenId, "")
		assert.Equal(t, result.WalletInfo.BalanceUsdcNanoCents, model.UsdToNanoCents(0.0))

		model.SetCircleUserId(
			ctx,
			session.ByJwt.NetworkId,
			session.ByJwt.UserId,
			circleUserIdWithWalletAndBalance,
		)

		result, err = WalletBalance(session)

		assert.Equal(t, err, nil)
		assert.NotEqual(t, result.WalletInfo, nil)
		assert.NotEqual(t, result.WalletInfo.WalletId, "")
		assert.NotEqual(t, result.WalletInfo.CreateDate, time.Time{})
		assert.Equal(t, result.WalletInfo.Blockchain, "MATIC")
		assert.Equal(t, result.WalletInfo.BlockchainSymbol, "USDC")
		assert.NotEqual(t, result.WalletInfo.TokenId, "")
		assert.Equal(t, result.WalletInfo.BalanceUsdcNanoCents, model.UsdToNanoCents(1.0))
	})
}

func TestWalletCircleTransferOut(t *testing.T) {
	bringyour.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		session := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId:   bringyour.NewId(),
			NetworkName: "test",
			UserId:      bringyour.NewId(),
		})

		model.SetCircleUserId(
			ctx,
			session.ByJwt.NetworkId,
			session.ByJwt.UserId,
			circleUserIdWithWalletAndBalance,
		)

		result, err := WalletCircleTransferOut(
			&WalletCircleTransferOutArgs{
				Terms: true,
				// BringYour USDC Polygon
				ToAddress:           "0xB3f448b9C395F9833BE866577254799c23BBa682",
				AmountUsdcNanoCents: model.UsdToNanoCents(1.0),
			},
			session,
		)

		assert.Equal(t, err, nil)
		assert.Equal(t, result.Error, nil)
		assert.NotEqual(t, result.UserToken, nil)
		assert.NotEqual(t, result.ChallengeId, "")
	})
}

func TestCircleWalletIdParsing(t *testing.T) {
	bringyour.DefaultTestEnv().Run(func() {
		circleWalletId := "02201362-9c27-5793-ad74-994c8bac4ccf" // this is an ID generated by Circle
		walletId, err := bringyour.ParseId("02201362-9c27-5793-ad74-994c8bac4ccf")
		assert.Equal(t, err, nil)
		assert.Equal(t, circleWalletId, walletId.String())
	})
}
