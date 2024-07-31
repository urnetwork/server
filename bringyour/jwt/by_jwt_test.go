package jwt

import (
	"context"
	"testing"

	"github.com/go-playground/assert/v2"

	"bringyour.com/bringyour"
)

func TestLegacyByJwt(t *testing.T) {
	bringyour.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		networkId := bringyour.NewId()
		userId := bringyour.NewId()
		networkName := "test"
		byJwt := NewByJwt(networkId, userId, networkName)
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
	bringyour.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		networkId := bringyour.NewId()
		userId := bringyour.NewId()
		networkName := "test"
		sessionIds := []bringyour.Id{
			bringyour.NewId(),
			bringyour.NewId(),
			bringyour.NewId(),
		}
		byJwt := NewByJwt(networkId, userId, networkName, sessionIds...)
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
	bringyour.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		networkId := bringyour.NewId()
		userId := bringyour.NewId()
		networkName := "test"
		sessionIds := []bringyour.Id{
			bringyour.NewId(),
			bringyour.NewId(),
			bringyour.NewId(),
		}
		byJwt := NewByJwt(networkId, userId, networkName, sessionIds...)

		deviceId := bringyour.NewId()
		clientId := bringyour.NewId()
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
