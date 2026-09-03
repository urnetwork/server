package session_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
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

// TestClientSessionAcceptsLegacyJwtUntilGateFlips covers the credential
// migration: tokens minted before the registered-claims hardening (no
// exp/iss/aud/sub/jti) keep authenticating on the api and connect paths
// while the auth.yml reject_missing_expiration gate is off, and are
// rejected once it is flipped on.
func TestClientSessionAcceptsLegacyJwtUntilGateFlips(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		userId := server.NewId()
		deviceId := server.NewId()
		clientId := server.NewId()
		networkName := "jwt-legacy-test"
		model.Testing_CreateNetwork(ctx, networkId, networkName, userId)
		model.Testing_CreateDevice(ctx, networkId, deviceId, clientId, "jwt-legacy-device", "test")

		// legacy credentials: business claims and create_time only, exactly
		// what pre-hardening mints produced
		legacyUser := &jwt.ByJwt{
			NetworkId:   networkId,
			UserId:      userId,
			NetworkName: networkName,
			CreateTime:  server.CodecTime(server.NowUtc()),
		}
		legacyClient := &jwt.ByJwt{
			NetworkId:   networkId,
			UserId:      userId,
			NetworkName: networkName,
			CreateTime:  server.CodecTime(server.NowUtc()),
			DeviceId:    &deviceId,
			ClientId:    &clientId,
		}

		authenticate := func(credential string) error {
			request, err := http.NewRequest(http.MethodGet, "https://api.example.test/", nil)
			if err != nil {
				return err
			}
			request.Header.Set("Authorization", fmt.Sprintf("Bearer %s", credential))
			clientSession := session.NewLocalClientSession(ctx, "127.0.0.1:1", nil)
			defer clientSession.Cancel()
			return clientSession.Auth(request)
		}

		popOff := jwt.Testing_SetRejectMissingExpiration(false)
		connect.AssertEqual(t, authenticate(legacyUser.Sign()), nil)
		connect.AssertEqual(t, authenticate(legacyClient.Sign()), nil)

		// the connect-transport path: parse for the connect audience, then
		// bind to current client state
		parsedClient, err := jwt.ParseByJwtForAudience(ctx, legacyClient.Sign(), jwt.ByJwtAudienceConnect)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, jwt.ValidateByJwtState(ctx, parsedClient, true), nil)
		popOff()

		popOn := jwt.Testing_SetRejectMissingExpiration(true)
		connect.AssertEqual(t, authenticate(legacyUser.Sign()) != nil, true)
		connect.AssertEqual(t, authenticate(legacyClient.Sign()) != nil, true)
		popOn()
	})
}

// TestClientSessionAcceptsExpiredJwtUntilGateFlips covers the reject_expired
// migration gate on the api and connect paths: a credential whose exp has
// passed keeps authenticating while the gate is off, and is rejected once it
// is flipped on.
func TestClientSessionAcceptsExpiredJwtUntilGateFlips(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		userId := server.NewId()
		deviceId := server.NewId()
		clientId := server.NewId()
		networkName := "jwt-expired-test"
		model.Testing_CreateNetwork(ctx, networkId, networkName, userId)
		model.Testing_CreateDevice(ctx, networkId, deviceId, clientId, "jwt-expired-device", "test")

		// a fully modern client credential whose lifetime has passed, like the
		// 30-day-era mints still held by deployed clients
		expiredCredential := func() string {
			byClientJwt := jwt.NewByJwt(networkId, userId, networkName, false, false).
				Client(deviceId, clientId)
			byClientJwt.IssuedAt = gojwt.NewNumericDate(server.NowUtc().Add(-25 * time.Hour))
			byClientJwt.NotBefore = byClientJwt.IssuedAt
			byClientJwt.ExpiresAt = gojwt.NewNumericDate(server.NowUtc().Add(-time.Hour))
			return byClientJwt.Sign()
		}

		authenticate := func(credential string) error {
			request, err := http.NewRequest(http.MethodGet, "https://api.example.test/", nil)
			if err != nil {
				return err
			}
			request.Header.Set("Authorization", fmt.Sprintf("Bearer %s", credential))
			clientSession := session.NewLocalClientSession(ctx, "127.0.0.1:1", nil)
			defer clientSession.Cancel()
			return clientSession.Auth(request)
		}

		popOff := jwt.Testing_SetRejectExpired(false)
		connect.AssertEqual(t, authenticate(expiredCredential()), nil)

		// the connect-transport path: parse for the connect audience, then
		// bind to current client state
		parsedClient, err := jwt.ParseByJwtForAudience(ctx, expiredCredential(), jwt.ByJwtAudienceConnect)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, jwt.ValidateByJwtState(ctx, parsedClient, true), nil)
		popOff()

		popOn := jwt.Testing_SetRejectExpired(true)
		connect.AssertEqual(t, authenticate(expiredCredential()) != nil, true)
		popOn()
	})
}
