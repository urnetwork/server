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

func TestAccountPoints(t *testing.T) {
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
		result, err := controller.GetAccountPoints(userSession)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(result.AccountPoints), 0)

		applyAccountPointsArgs := model.ApplyAccountPointsArgs{
			NetworkId:  networkId,
			Event:      model.AccountPointEventPayoutLinkedAccount,
			PointValue: 10,
		}

		//  apply points
		model.ApplyAccountPoints(ctx, applyAccountPointsArgs)

		// assert network points
		result, err = controller.GetAccountPoints(userSession)
		assert.Equal(t, err, nil)
		assert.NotEqual(t, result.AccountPoints, nil)
		assert.Equal(t, len(result.AccountPoints), 1)
		assert.Equal(t, result.AccountPoints[0].NetworkId, networkId)
		assert.Equal(t, result.AccountPoints[0].Event, string(model.AccountPointEventPayoutLinkedAccount))
		assert.NotEqual(t, result.AccountPoints[0].PointValue, model.NanoPoints(0))
		assert.Equal(t, result.AccountPoints[0].CreateTime.IsZero(), false)
		assert.Equal(t, result.AccountPoints[0].PointValue, model.NanoPoints(10))

		applyAccountPointsArgs = model.ApplyAccountPointsArgs{
			NetworkId:  networkId,
			Event:      model.AccountPointEventPayoutLinkedAccount,
			PointValue: 5,
		}

		// increment
		model.ApplyAccountPoints(ctx, applyAccountPointsArgs)

		result, err = controller.GetAccountPoints(userSession)
		assert.Equal(t, err, nil)
		assert.NotEqual(t, result.AccountPoints, nil)
		assert.Equal(t, len(result.AccountPoints), 2)

	})
}
