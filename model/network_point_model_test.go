package model_test

import (
	"context"
	"testing"

	"github.com/go-playground/assert/v2"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

func TestNetworkPoints(t *testing.T) {
	server.DefaultTestEnv().Run(func() {

		ctx := context.Background()
		networkId := server.NewId()

		err := model.ApplyNetworkPoints(ctx, networkId, model.NetworkPointEventReferral, 10)
		assert.Equal(t, err, nil)

		networkPoints := model.FetchNetworkPoints(ctx, networkId)
		assert.NotEqual(t, networkPoints, nil)
		assert.Equal(t, len(networkPoints), 1)
		assert.Equal(t, networkPoints[0].NetworkId, networkId)
		assert.Equal(t, networkPoints[0].Event, "referral")
		assert.NotEqual(t, networkPoints[0].PointValue, 0)

		err = model.ApplyNetworkPoints(ctx, networkId, model.NetworkPointEventReferral, 5)
		assert.Equal(t, err, nil)

		networkPoints = model.FetchNetworkPoints(ctx, networkId)
		assert.Equal(t, len(networkPoints), 2)

		totalPoints := 0
		for _, point := range networkPoints {
			totalPoints += point.PointValue
		}
		assert.Equal(t, totalPoints, 15)

	})
}
