package model

import (
	"context"
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/session"
)

func TestCircleUC(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		var circleUserIdWithWallet = server.RequireParseId("018c4b12-1a76-aaca-acce-72ddae03f60d")
		ctx := context.Background()

		session := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId:   server.NewId(),
			NetworkName: "test",
			UserId:      server.NewId(),
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
		connect.AssertEqual(t, circleUC.CircleUCUserId, circleUserIdWithWallet)

		// attempt to fetch with incorrect circle_uc_user_id
		failId := server.NewId()
		circleUC = GetCircleUCByCircleUCUserId(ctx, failId)
		connect.AssertEqual(t, circleUC, nil)
	})
}
