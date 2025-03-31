package jwt

import (
	"context"
	"testing"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/server"
)

func TestLegacyByJwt(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()
		networkName := "test"
		guestMode := false
		byJwt := NewByJwt(networkId, userId, networkName, guestMode)
		jwtSigned := byJwt.Sign()

		parsedByJwt, err := ParseByJwt(jwtSigned)
		assert.Equal(t, err, nil)
		assert.NotEqual(t, parsedByJwt, nil)

		assert.Equal(t, byJwt.NetworkId, parsedByJwt.NetworkId)
		assert.Equal(t, byJwt.UserId, parsedByJwt.UserId)
		assert.Equal(t, byJwt.NetworkName, parsedByJwt.NetworkName)

		assert.Equal(t, true, IsByJwtActive(ctx, byJwt))
		assert.Equal(t, true, IsByJwtActive(ctx, parsedByJwt))
	})
}

func TestFullByJwt(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()
		networkName := "test"
		guestMode := false
		sessionIds := []server.Id{
			server.NewId(),
			server.NewId(),
			server.NewId(),
		}
		byJwt := NewByJwt(networkId, userId, networkName, guestMode, sessionIds...)
		jwtSigned := byJwt.Sign()

		parsedByJwt, err := ParseByJwt(jwtSigned)
		assert.Equal(t, err, nil)
		assert.NotEqual(t, parsedByJwt, nil)

		assert.Equal(t, byJwt.NetworkId, parsedByJwt.NetworkId)
		assert.Equal(t, byJwt.UserId, parsedByJwt.UserId)
		assert.Equal(t, byJwt.NetworkName, parsedByJwt.NetworkName)
		assert.Equal(t, byJwt.CreateTime, parsedByJwt.CreateTime)
		assert.Equal(t, byJwt.AuthSessionIds, parsedByJwt.AuthSessionIds)

		assert.Equal(t, true, IsByJwtActive(ctx, byJwt))
		assert.Equal(t, true, IsByJwtActive(ctx, parsedByJwt))
	})
}

func TestFullByJwtWithClientId(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()
		networkName := "test"
		guestMode := false
		sessionIds := []server.Id{
			server.NewId(),
			server.NewId(),
			server.NewId(),
		}
		byJwt := NewByJwt(networkId, userId, networkName, guestMode, sessionIds...)

		deviceId := server.NewId()
		clientId := server.NewId()
		byClientJwt := byJwt.Client(deviceId, clientId)

		clientJwtSigned := byClientJwt.Sign()

		parsedByClientJwt, err := ParseByJwt(clientJwtSigned)
		assert.Equal(t, err, nil)
		assert.NotEqual(t, parsedByClientJwt, nil)

		assert.Equal(t, byJwt.NetworkId, parsedByClientJwt.NetworkId)
		assert.Equal(t, byJwt.UserId, parsedByClientJwt.UserId)
		assert.Equal(t, byJwt.NetworkName, parsedByClientJwt.NetworkName)
		assert.Equal(t, byJwt.CreateTime, parsedByClientJwt.CreateTime)
		assert.Equal(t, byJwt.AuthSessionIds, parsedByClientJwt.AuthSessionIds)
		assert.Equal(t, byClientJwt.DeviceId, parsedByClientJwt.DeviceId)
		assert.Equal(t, byClientJwt.ClientId, parsedByClientJwt.ClientId)

		assert.Equal(t, true, IsByJwtActive(ctx, byClientJwt))
		assert.Equal(t, true, IsByJwtActive(ctx, parsedByClientJwt))
	})
}
