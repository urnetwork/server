package session_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

func TestClientSessionRejectsInactiveAndRotatedJwt(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		userId := server.NewId()
		deviceId := server.NewId()
		clientId := server.NewId()
		networkName := "jwt-state-test"
		model.Testing_CreateNetwork(ctx, networkId, networkName, userId)
		model.Testing_CreateDevice(ctx, networkId, deviceId, clientId, "jwt-state-device", "test")

		credential := jwt.NewByJwt(networkId, userId, networkName, false, false).
			Client(deviceId, clientId).
			Sign()
		authenticate := func() error {
			request, err := http.NewRequest(http.MethodGet, "https://api.example.test/", nil)
			if err != nil {
				return err
			}
			request.Header.Set("Authorization", fmt.Sprintf("Bearer %s", credential))
			clientSession := session.NewLocalClientSession(ctx, "127.0.0.1:1", nil)
			defer clientSession.Cancel()
			return clientSession.Auth(request)
		}

		connect.AssertEqual(t, authenticate(), nil)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET active = false WHERE client_id = $1`, clientId))
		})
		connect.AssertEqual(t, authenticate() != nil, true)

		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET active = true WHERE client_id = $1`, clientId))
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE network_user SET credential_change_time = $2 WHERE user_id = $1`,
				userId,
				server.NowUtc().Add(time.Minute),
			))
		})
		connect.AssertEqual(t, authenticate() != nil, true)
	})
}
