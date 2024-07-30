package model

import (
	"context"
	"testing"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/jwt"
	"bringyour.com/bringyour/session"
	"github.com/go-playground/assert/v2"
)

func TestCircleUC(t *testing.T) { bringyour.DefaultTestEnv().Run(func() {
	var circleUserIdWithWallet = bringyour.RequireParseId("018c4b12-1a76-aaca-acce-72ddae03f60d")
	ctx := context.Background()

	session := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
		NetworkId: bringyour.NewId(),
		NetworkName: "test",
		UserId: bringyour.NewId(),
	})

	// set used for testing
	SetCircleUserId(
		ctx,
		session.ByJwt.NetworkId,
		session.ByJwt.UserId,
		circleUserIdWithWallet,
	)

	// fetch circle_uc row by circle_uc_user_id
	circleUC := GetCircleUCByCircleUCUserId(ctx, circleUserIdWithWallet)
	assert.Equal(t, circleUC.CircleUCUserId, circleUserIdWithWallet)

	// attempt to fetch with incorrect circle_uc_user_id
	failId := bringyour.NewId()
	circleUC = GetCircleUCByCircleUCUserId(ctx, failId)
	assert.Equal(t, circleUC, nil)
})}