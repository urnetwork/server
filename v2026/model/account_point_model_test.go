package model

import (
	"context"
	"testing"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
)

func TestAccountPoints(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		ctx := context.Background()
		networkId := server.NewId()

		applyAccountPointsArgs := ApplyAccountPointsArgs{
			NetworkId:  networkId,
			Event:      AccountPointEventPayout,
			PointValue: 10,
		}

		err := ApplyAccountPoints(ctx, applyAccountPointsArgs)
		connect.AssertEqual(t, err, nil)

		networkPoints := FetchAccountPoints(ctx, networkId)
		connect.AssertNotEqual(t, networkPoints, nil)
		connect.AssertEqual(t, len(networkPoints), 1)
		connect.AssertEqual(t, networkPoints[0].NetworkId, networkId)
		connect.AssertEqual(t, networkPoints[0].Event, string(AccountPointEventPayout))
		connect.AssertNotEqual(t, networkPoints[0].PointValue, 0)

		applyAccountPointsArgs = ApplyAccountPointsArgs{
			NetworkId:  networkId,
			Event:      AccountPointEventPayout,
			PointValue: 5,
		}

		err = ApplyAccountPoints(ctx, applyAccountPointsArgs)
		connect.AssertEqual(t, err, nil)

		networkPoints = FetchAccountPoints(ctx, networkId)
		connect.AssertEqual(t, len(networkPoints), 2)

		totalPoints := NanoPoints(0)
		for _, point := range networkPoints {
			totalPoints += point.PointValue
		}
		connect.AssertEqual(t, totalPoints, NanoPoints(15))

	})
}
