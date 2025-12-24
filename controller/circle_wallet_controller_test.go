package controller

import (
	"context"
	"testing"
	"time"

	// "golang.org/x/exp/maps"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// these were set up manually on main
// var circleUserIdWithWallet = server.RequireParseId("018c3c3c-8265-1b71-e827-902beb3233c4")
var circleUserIdWithWallet = server.RequireParseId("018c4b12-1a76-aaca-acce-72ddae03f60d")
var circleUserIdWithWalletAndBalance = server.RequireParseId("018c3c7f-82f3-341b-6fd9-fe8d180c366c")

func TestWalletCircleInit(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		session := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId:   server.NewId(),
			NetworkName: "test",
			UserId:      server.NewId(),
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
	server.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		session := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId:   server.NewId(),
			NetworkName: "test",
			UserId:      server.NewId(),
		})
		result, err := WalletCircleInit(session)

		assert.Equal(t, err, nil)
		assert.Equal(t, result.Error, nil)
		assert.NotEqual(t, result.UserToken, nil)
		assert.NotEqual(t, result.ChallengeId, "")

		// test valid SOL address
		validateResult, err := WalletValidateAddress(
			&WalletValidateAddressArgs{
				Address: "DgTYzxzYRpkGQ8e3Un71GoQf494VLDBnyqXNXB38MP73",
				Chain:   model.SOL.String(),
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
				Chain:   model.SOL.String(),
			},
			session,
		)
		assert.Equal(t, err, nil)
		assert.Equal(t, validateResult.Valid, false)

		// test passing USDC mint address as wallet address
		validateResult, err = WalletValidateAddress(
			&WalletValidateAddressArgs{
				Address: solanaUSDCAddress(),
				Chain:   model.SOL.String(),
			},
			session,
		)
		assert.Equal(t, err, nil)
		assert.Equal(t, validateResult.Valid, false)
	})
}

func TestWalletBalance(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		session := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId:   server.NewId(),
			NetworkName: "test",
			UserId:      server.NewId(),
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
		assert.Equal(t, result.WalletInfo.BlockchainSymbol, model.MATIC.String())
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
		assert.Equal(t, result.WalletInfo.Blockchain, model.MATIC.String())
		assert.Equal(t, result.WalletInfo.BlockchainSymbol, "USDC")
		assert.NotEqual(t, result.WalletInfo.TokenId, "")
		assert.Equal(t, result.WalletInfo.BalanceUsdcNanoCents, model.UsdToNanoCents(1.0))
	})
}

func TestWalletCircleTransferOut(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		session := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId:   server.NewId(),
			NetworkName: "test",
			UserId:      server.NewId(),
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
	server.DefaultTestEnv().Run(func() {
		circleWalletId := "02201362-9c27-5793-ad74-994c8bac4ccf" // this is an ID generated by Circle
		walletId, err := server.ParseId("02201362-9c27-5793-ad74-994c8bac4ccf")
		assert.Equal(t, err, nil)
		assert.Equal(t, circleWalletId, walletId.String())
	})
}

func TestCircleWebhookVerifySignature(t *testing.T) {
	server.DefaultTestEnv().Run(func() {

		// values pulled from example docs at https://developers.circle.com/w3s/docs/web3-services-notifications-quickstart
		publicKeyBase64 := "MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAESl76SZPBJemW0mJNN4KTvYkLT8bOT4UGhFhzNk3fJqf6iuPlLQLq533FelXwczJbjg2U1PHTvQTK7qOQnDL2Tg=="
		signatureBase64 := "MEQCIBlJPX7t0FDOcozsRK6qIQwik5Fq6mhAtCSSgIB/yQO7AiB9U5lVpdufKvPhk3cz4TH2f5MP7ArnmPRBmhPztpsIFQ=="
		responseBodyBytes := []byte("{\n\"subscriptionId\":\"00000000-0000-0000-0000-000000000000\",\"notificationId\":\"00000000-0000-0000-0000-000000000000\",\"notificationType\":\"webhooks.test\",\"notification\":{\"hello\":\"world\"},\"timestamp\":\"2024-01-26T18:22:19.779834211Z\",\"version\":2}")

		err := verifySignature(
			publicKeyBase64,
			signatureBase64,
			responseBodyBytes,
		)

		assert.Equal(t, err, nil)

	})
}
