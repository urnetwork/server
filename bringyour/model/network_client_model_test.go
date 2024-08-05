package model

import (
	"context"
	"testing"
	"time"

	"github.com/go-playground/assert/v2"

	"bringyour.com/bringyour"
)

func TestNetworkClientHandlerLifecycle(t *testing.T) {
	bringyour.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		clientId := bringyour.NewId()

		handlerId := CreateNetworkClientHandler(ctx)
		connectionId := ConnectNetworkClient(
			ctx,
			clientId,
			"0.0.0.0:0",
			handlerId,
		)

		err := HeartbeatNetworkClientHandler(ctx, handlerId)
		assert.Equal(t, err, nil)

		select {
		case <-time.After(1 * time.Second):
		}

		connected := IsNetworkClientConnected(ctx, connectionId)
		assert.Equal(t, connected, true)

		CloseExpiredNetworkClientHandlers(ctx, time.Duration(0))

		connected = IsNetworkClientConnected(ctx, connectionId)
		assert.Equal(t, connected, false)

		select {
		case <-time.After(1 * time.Second):
		}

		DeleteDisconnectedNetworkClients(ctx, time.Duration(0))

		err = DisconnectNetworkClient(ctx, connectionId)
		assert.NotEqual(t, err, nil)

		err = HeartbeatNetworkClientHandler(ctx, handlerId)
		assert.NotEqual(t, err, nil)
	})
}

func TestNetworkClientLifecycle(t *testing.T) {
	bringyour.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		clientId := bringyour.NewId()

		handlerId := CreateNetworkClientHandler(ctx)
		connectionId := ConnectNetworkClient(
			ctx,
			clientId,
			"0.0.0.0:0",
			handlerId,
		)

		select {
		case <-time.After(1 * time.Second):
		}

		connected := IsNetworkClientConnected(ctx, connectionId)
		assert.Equal(t, connected, true)

		err := DisconnectNetworkClient(ctx, connectionId)
		assert.Equal(t, err, nil)

		connected = IsNetworkClientConnected(ctx, connectionId)
		assert.Equal(t, connected, false)

		DeleteDisconnectedNetworkClients(ctx, time.Duration(0))

		err = DisconnectNetworkClient(ctx, connectionId)
		assert.NotEqual(t, err, nil)
	})
}