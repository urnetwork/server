package controller_test

import (
	"context"
	"testing"

	"github.com/go-playground/assert/v2"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

func TestNetworkPoints(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		networkId := server.NewId()
		clientId := server.NewId()
		userId := server.NewId()
		networkName := "abcdef"

		model.Testing_CreateNetwork(ctx, networkId, networkName, userId)

		userSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			ClientId:  &clientId,
			UserId:    userId,
		})

		// assert no network points
		result, err := controller.GetNetworkPoints(userSession)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(result.NetworkPoints), 0)

		//  apply points
		model.ApplyNetworkPoints(ctx, networkId, "referral")

		// assert network points
		result, err = controller.GetNetworkPoints(userSession)
		assert.Equal(t, err, nil)
		assert.NotEqual(t, result.NetworkPoints, nil)
		assert.Equal(t, len(result.NetworkPoints), 1)
		assert.Equal(t, result.NetworkPoints[0].NetworkId, networkId)
		assert.Equal(t, result.NetworkPoints[0].Event, "referral")
		assert.NotEqual(t, result.NetworkPoints[0].PointValue, 0)
		assert.Equal(t, result.NetworkPoints[0].CreateTime.IsZero(), false)
		assert.NotEqual(t, result.NetworkPoints[0].PointValue, 0)

		// increment
		model.ApplyNetworkPoints(ctx, networkId, "referral")

		result, err = controller.GetNetworkPoints(userSession)
		assert.Equal(t, err, nil)
		assert.NotEqual(t, result.NetworkPoints, nil)
		assert.Equal(t, len(result.NetworkPoints), 2)

	})
}
