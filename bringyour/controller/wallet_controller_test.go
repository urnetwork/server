package controller

import (
    "context"
    "testing"
    "time"

    // "golang.org/x/exp/maps"

    "github.com/go-playground/assert/v2"

    "bringyour.com/bringyour"
    "bringyour.com/bringyour/model"
    "bringyour.com/bringyour/jwt"
    "bringyour.com/bringyour/session"
)


// FIXME use a lowercase uuid
// this was set up manually on main
var circleUserIdWithWallet = bringyour.RequireParseId("43BCBBC9-B774-4E02-B721-612B23562152")



func TestWalletCircleInit(t *testing.T) { bringyour.DefaultTestEnv().Run(func() {
	ctx := context.Background()

	session := session.NewLocalClientSession(ctx, &jwt.ByJwt{
		NetworkId: bringyour.NewId(),
		NetworkName: "test",
		UserId: bringyour.NewId(),
	})
	result, err := WalletCircleInit(session)

	assert.Equal(t, err, nil)
	assert.Equal(t, result.Error, nil)
	assert.NotEqual(t, result.UserToken, nil)
	assert.NotEqual(t, result.ChallengeId, "")
})}


func TestWalletValidateAddress(t *testing.T) { bringyour.DefaultTestEnv().Run(func() {
	ctx := context.Background()

	session := session.NewLocalClientSession(ctx, &jwt.ByJwt{
		NetworkId: bringyour.NewId(),
		NetworkName: "test",
		UserId: bringyour.NewId(),
	})
	result, err := WalletCircleInit(session)

	assert.Equal(t, err, nil)
	assert.Equal(t, result.Error, nil)
	assert.NotEqual(t, result.UserToken, nil)
	assert.NotEqual(t, result.ChallengeId, "")


	validateResult, err := WalletValidateAddress(
		&WalletValidateAddressArgs{
			// BringYour USDC Polygon
			Address: "0xB3f448b9C395F9833BE866577254799c23BBa682",
		},
		session,
	)

	assert.Equal(t, err, nil)
	assert.Equal(t, validateResult.Valid, true)


	validateResult, err = WalletValidateAddress(
		&WalletValidateAddressArgs{
			// BringYour USDC Solana
			Address: "DgTYzxzYRpkGQ8e3Un71GoQf494VLDBnyqXNXB38MP73",
		},
		session,
	)

	assert.Equal(t, err, nil)
	assert.Equal(t, validateResult.Valid, false)
})}


func TestWalletBalance(t *testing.T) { bringyour.DefaultTestEnv().Run(func() {
	ctx := context.Background()

	session := session.NewLocalClientSession(ctx, &jwt.ByJwt{
		NetworkId: bringyour.NewId(),
		NetworkName: "test",
		UserId: bringyour.NewId(),
	})

	model.SetCircleUserId(
		ctx,
		session.ByJwt.NetworkId,
		session.ByJwt.UserId,
		circleUserIdWithWallet,
	)
	TEMP_ForceUpperUserId = true
	
	result, err := WalletBalance(session)

	assert.Equal(t, err, nil)
	assert.NotEqual(t, result.WalletInfo, nil)
	assert.NotEqual(t, result.WalletInfo.WalletId, "")
	assert.NotEqual(t, result.WalletInfo.TokenId, "")
	// FIXME to Polygon when the test userid is updated
	assert.Equal(t, result.WalletInfo.Blockchain, "Ethereum")
	assert.Equal(t, result.WalletInfo.BlockchainSymbol, "ETH")
	assert.NotEqual(t, result.WalletInfo.CreateDate, time.Time{})
	assert.Equal(t, result.WalletInfo.BalanceUsdcNanoCents, model.UsdToNanoCents(1.0))
})}


func TestWalletCircleTransferOut(t *testing.T) { bringyour.DefaultTestEnv().Run(func() {
	ctx := context.Background()

	session := session.NewLocalClientSession(ctx, &jwt.ByJwt{
		NetworkId: bringyour.NewId(),
		NetworkName: "test",
		UserId: bringyour.NewId(),
	})

	model.SetCircleUserId(
		ctx,
		session.ByJwt.NetworkId,
		session.ByJwt.UserId,
		circleUserIdWithWallet,
	)
	TEMP_ForceUpperUserId = true

	result, err := WalletCircleTransferOut(
		&WalletCircleTransferOutArgs{
			// BringYour USDC Eth
			ToAddress: "0xB3f448b9C395F9833BE866577254799c23BBa682",
			AmountUsdcNanoCents: model.UsdToNanoCents(1.0),
		},
		session,
	)

	assert.Equal(t, err, nil)
	assert.Equal(t, result.Error, nil)
	assert.NotEqual(t, result.UserToken, nil)
	assert.NotEqual(t, result.ChallengeId, "")
})}

