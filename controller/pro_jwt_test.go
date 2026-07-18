package controller_test

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// TestProJwtRefreshTokenRederives pins that RefreshToken re-derives Pro from the source of
// truth on every refresh: a network that turned Pro after the client token was minted is
// reflected on the next refresh, without a re-login. (The app refreshes on a schedule and
// after an upgrade — this is the path that heals a stale Pro claim.)
func TestProJwtRefreshTokenRederives(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		userId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "test", userId)

		// create a client while the network is not Pro -> its token is Pro=false
		userSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId, UserId: userId, NetworkName: "test", Pro: false,
		})
		authResult, err := model.AuthNetworkClient(
			&model.AuthNetworkClientArgs{Description: "d", DeviceSpec: "s"},
			userSession,
		)
		connect.AssertEqual(t, err, nil)
		clientByJwt, err := jwt.ParseByJwt(ctx, *authResult.ByClientJwt)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, clientByJwt.Pro, false)

		// the network becomes Pro
		now := server.NowUtc()
		server.Tx(ctx, func(tx server.PgTx) {
			err := model.AddProTransferBalanceInTx(
				tx, ctx, networkId, model.ByteCount(1024*1024*1024), now, now.Add(30*24*time.Hour),
			)
			connect.AssertEqual(t, err, nil)
		})
		model.UpdateProNetwork(ctx, networkId)

		// refreshing the (still Pro=false) client token re-derives Pro=true
		clientSession := session.Testing_CreateClientSession(ctx, clientByJwt)
		refreshResult, err := controller.RefreshToken(clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, refreshResult.Error, nil)
		refreshed, err := jwt.ParseByJwt(ctx, refreshResult.ByJwt)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, refreshed.Pro, true)
	})
}
