package controller_test

import (
	"context"
	"testing"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

func TestAccountPoints(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
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
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(result.AccountPoints), 0)

		applyAccountPointsArgs := model.ApplyAccountPointsArgs{
			NetworkId:  networkId,
			Event:      model.AccountPointEventPayoutLinkedAccount,
			PointValue: 10,
		}

		//  apply points
		model.ApplyAccountPoints(ctx, applyAccountPointsArgs)

		// assert network points
		result, err = controller.GetAccountPoints(userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, result.AccountPoints, nil)
		connect.AssertEqual(t, len(result.AccountPoints), 1)
		connect.AssertEqual(t, result.AccountPoints[0].NetworkId, networkId)
		connect.AssertEqual(t, result.AccountPoints[0].Event, string(model.AccountPointEventPayoutLinkedAccount))
		connect.AssertNotEqual(t, result.AccountPoints[0].PointValue, model.NanoPoints(0))
		connect.AssertEqual(t, result.AccountPoints[0].CreateTime.IsZero(), false)
		connect.AssertEqual(t, result.AccountPoints[0].PointValue, model.NanoPoints(10))

		applyAccountPointsArgs = model.ApplyAccountPointsArgs{
			NetworkId:  networkId,
			Event:      model.AccountPointEventPayoutLinkedAccount,
			PointValue: 5,
		}

		// increment
		model.ApplyAccountPoints(ctx, applyAccountPointsArgs)

		result, err = controller.GetAccountPoints(userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, result.AccountPoints, nil)
		connect.AssertEqual(t, len(result.AccountPoints), 2)

	})
}
