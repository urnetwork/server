package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	// "maps"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// these were set up manually on main
// var circleUserIdWithWallet = server.RequireParseId("018c3c3c-8265-1b71-e827-902beb3233c4")
var circleUserIdWithWallet = server.RequireParseId("018c4b12-1a76-aaca-acce-72ddae03f60d")
var circleUserIdWithWalletAndBalance = server.RequireParseId("018c3c7f-82f3-341b-6fd9-fe8d180c366c")

func TestWalletValidateAddressMatchesDeclaredChain(t *testing.T) {
	tests := []struct {
		name    string
		chain   string
		address string
		valid   bool
	}{
		{
			name:    "solana",
			chain:   "SOL",
			address: "DgTYzxzYRpkGQ8e3Un71GoQf494VLDBnyqXNXB38MP73",
			valid:   true,
		},
		{
			name:    "polygon",
			chain:   "MATIC",
			address: "0xB3f448b9C395F9833BE866577254799c23BBa682",
			valid:   true,
		},
		{
			name:    "production regression - solana key labeled polygon",
			chain:   "MATIC",
			address: "DgTYzxzYRpkGQ8e3Un71GoQf494VLDBnyqXNXB38MP73",
			valid:   false,
		},
		{
			name:    "polygon address labeled solana",
			chain:   "SOL",
			address: "0xB3f448b9C395F9833BE866577254799c23BBa682",
			valid:   false,
		},
		{
			name:    "zero polygon address",
			chain:   "MATIC",
			address: "0x0000000000000000000000000000000000000000",
			valid:   false,
		},
		{
			name:    "solana usdc mint is not a wallet",
			chain:   "SOL",
			address: solanaUsdcMint,
			valid:   false,
		},
		{
			name:    "unknown chain",
			chain:   "BTC",
			address: "DgTYzxzYRpkGQ8e3Un71GoQf494VLDBnyqXNXB38MP73",
			valid:   false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := WalletValidateAddress(&WalletValidateAddressArgs{
				Address: test.address,
				Chain:   test.chain,
			}, nil)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, result.Valid, test.valid)
		})
	}
}

func TestCircleInvalidDestinationErrorClassification(t *testing.T) {
	invalidDestination := &server.HttpStatusError{
		StatusCode:   http.StatusBadRequest,
		Status:       "400 Bad Request",
		ResponseBody: `{"code":155219,"message":"Invalid destination address."}`,
	}
	tests := []struct {
		name  string
		err   error
		match bool
	}{
		{name: "exact", err: invalidDestination, match: true},
		{name: "wrapped", err: fmt.Errorf("submit: %w", invalidDestination), match: true},
		{name: "rate limit", err: &server.HttpStatusError{StatusCode: http.StatusTooManyRequests, ResponseBody: invalidDestination.ResponseBody}, match: false},
		{name: "other bad request", err: &server.HttpStatusError{StatusCode: http.StatusBadRequest, ResponseBody: `{"code":123,"message":"bad amount"}`}, match: false},
		{name: "matching text without typed status", err: errors.New("Invalid destination address."), match: false},
		{name: "malformed body", err: &server.HttpStatusError{StatusCode: http.StatusBadRequest, ResponseBody: `not-json`}, match: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			connect.AssertEqual(t, isCircleInvalidDestinationError(test.err), test.match)
		})
	}
}

func TestWalletCircleInit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		session := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId:   server.NewId(),
			NetworkName: "test",
			UserId:      server.NewId(),
		})
		result, err := WalletCircleInit(session)

		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error, nil)
		connect.AssertNotEqual(t, result.UserToken, nil)
		connect.AssertNotEqual(t, result.ChallengeId, "")

		// a second init should not create an error
		result, err = WalletCircleInit(session)

		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error, nil)
		connect.AssertNotEqual(t, result.UserToken, nil)
		connect.AssertNotEqual(t, result.ChallengeId, "")
	})
}

func TestWalletValidateAddress(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		session := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId:   server.NewId(),
			NetworkName: "test",
			UserId:      server.NewId(),
		})
		result, err := WalletCircleInit(session)

		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error, nil)
		connect.AssertNotEqual(t, result.UserToken, nil)
		connect.AssertNotEqual(t, result.ChallengeId, "")

		// test valid SOL address
		validateResult, err := WalletValidateAddress(
			&WalletValidateAddressArgs{
				Address: "DgTYzxzYRpkGQ8e3Un71GoQf494VLDBnyqXNXB38MP73",
				Chain:   model.SOL.String(),
			},
			session,
		)

		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, validateResult.Valid, true)

		// test invalid address
		validateResult, err = WalletValidateAddress(
			&WalletValidateAddressArgs{
				// BringYour USDC Polygon
				Address: "0xB3f448b9C395F9833BE866577254799c23BBa682",
				Chain:   model.SOL.String(),
			},
			session,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, validateResult.Valid, false)

		// test passing USDC mint address as wallet address
		validateResult, err = WalletValidateAddress(
			&WalletValidateAddressArgs{
				Address: solanaUSDCAddress(),
				Chain:   model.SOL.String(),
			},
			session,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, validateResult.Valid, false)
	})
}

func TestWalletBalance(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
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

		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, result.WalletInfo, nil)
		connect.AssertNotEqual(t, result.WalletInfo.WalletId, "")
		connect.AssertNotEqual(t, result.WalletInfo.CreateDate, time.Time{})
		// the wallet is empty so these are the defaults
		connect.AssertEqual(t, result.WalletInfo.Blockchain, "Polygon")
		connect.AssertEqual(t, result.WalletInfo.BlockchainSymbol, model.MATIC.String())
		connect.AssertEqual(t, result.WalletInfo.TokenId, "")
		connect.AssertEqual(t, result.WalletInfo.BalanceUsdcNanoCents, model.UsdToNanoCents(0.0))

		model.SetCircleUserId(
			ctx,
			session.ByJwt.NetworkId,
			session.ByJwt.UserId,
			circleUserIdWithWalletAndBalance,
		)

		result, err = WalletBalance(session)

		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, result.WalletInfo, nil)
		connect.AssertNotEqual(t, result.WalletInfo.WalletId, "")
		connect.AssertNotEqual(t, result.WalletInfo.CreateDate, time.Time{})
		connect.AssertEqual(t, result.WalletInfo.Blockchain, model.MATIC.String())
		connect.AssertEqual(t, result.WalletInfo.BlockchainSymbol, "USDC")
		connect.AssertNotEqual(t, result.WalletInfo.TokenId, "")
		connect.AssertEqual(t, result.WalletInfo.BalanceUsdcNanoCents, model.UsdToNanoCents(1.0))
	})
}

func TestWalletCircleTransferOut(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
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

		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error, nil)
		connect.AssertNotEqual(t, result.UserToken, nil)
		connect.AssertNotEqual(t, result.ChallengeId, "")
	})
}

func TestCircleWalletIdParsing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		circleWalletId := "02201362-9c27-5793-ad74-994c8bac4ccf" // this is an ID generated by Circle
		walletId, err := server.ParseId("02201362-9c27-5793-ad74-994c8bac4ccf")
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, circleWalletId, walletId.String())
	})
}

func TestCircleWebhookVerifySignature(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		// values pulled from example docs at https://developers.circle.com/w3s/docs/web3-services-notifications-quickstart
		publicKeyBase64 := "MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAESl76SZPBJemW0mJNN4KTvYkLT8bOT4UGhFhzNk3fJqf6iuPlLQLq533FelXwczJbjg2U1PHTvQTK7qOQnDL2Tg=="
		signatureBase64 := "MEQCIBlJPX7t0FDOcozsRK6qIQwik5Fq6mhAtCSSgIB/yQO7AiB9U5lVpdufKvPhk3cz4TH2f5MP7ArnmPRBmhPztpsIFQ=="
		responseBodyBytes := []byte("{\n\"subscriptionId\":\"00000000-0000-0000-0000-000000000000\",\"notificationId\":\"00000000-0000-0000-0000-000000000000\",\"notificationType\":\"webhooks.test\",\"notification\":{\"hello\":\"world\"},\"timestamp\":\"2024-01-26T18:22:19.779834211Z\",\"version\":2}")

		err := verifySignature(
			publicKeyBase64,
			signatureBase64,
			responseBodyBytes,
		)

		connect.AssertEqual(t, err, nil)

	})
}
