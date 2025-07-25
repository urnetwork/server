package model

import (
	"context"
	"testing"
	"time"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/server"
)

func TestNetworkClientHandlerLifecycle(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		clientId := server.NewId()

		handlerId := CreateNetworkClientHandler(ctx)
		connectionId, _, _, _, err := ConnectNetworkClient(
			ctx,
			clientId,
			"0.0.0.0:0",
			handlerId,
		)
		assert.Equal(t, err, nil)

		err = HeartbeatNetworkClientHandler(ctx, handlerId)
		assert.Equal(t, err, nil)

		select {
		case <-time.After(1 * time.Second):
		}

		connected := GetNetworkClientConnectionStatus(ctx, connectionId).Connected
		assert.Equal(t, connected, true)

		CloseExpiredNetworkClientHandlers(ctx, time.Duration(0))

		connected = GetNetworkClientConnectionStatus(ctx, connectionId).Connected
		assert.Equal(t, connected, false)

		select {
		case <-time.After(1 * time.Second):
		}

		RemoveDisconnectedNetworkClients(ctx, time.Now())

		err = DisconnectNetworkClient(ctx, connectionId)
		assert.NotEqual(t, err, nil)

		err = HeartbeatNetworkClientHandler(ctx, handlerId)
		assert.NotEqual(t, err, nil)
	})
}

func TestNetworkClientHandlerLifecycleIPV6(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		clientId := server.NewId()

		handlerId := CreateNetworkClientHandler(ctx)
		connectionId, _, _, _, err := ConnectNetworkClient(
			ctx,
			clientId,
			"2001:5a8:4683:4e00:3a76:dcec:7cb:f180:40894",
			handlerId,
		)
		assert.Equal(t, err, nil)

		err = HeartbeatNetworkClientHandler(ctx, handlerId)
		assert.Equal(t, err, nil)

		time.Sleep(1 * time.Second)

		connected := GetNetworkClientConnectionStatus(ctx, connectionId).Connected
		assert.Equal(t, connected, true)

		CloseExpiredNetworkClientHandlers(ctx, time.Duration(0))

		connected = GetNetworkClientConnectionStatus(ctx, connectionId).Connected
		assert.Equal(t, connected, false)

		time.Sleep(1 * time.Second)

		RemoveDisconnectedNetworkClients(ctx, time.Now())

		err = DisconnectNetworkClient(ctx, connectionId)
		assert.NotEqual(t, err, nil)

		err = HeartbeatNetworkClientHandler(ctx, handlerId)
		assert.NotEqual(t, err, nil)
	})
}

func TestNetworkClientLifecycle(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		clientId := server.NewId()

		handlerId := CreateNetworkClientHandler(ctx)
		connectionId, _, _, _, err := ConnectNetworkClient(
			ctx,
			clientId,
			"0.0.0.0:0",
			handlerId,
		)
		assert.Equal(t, err, nil)

		select {
		case <-time.After(1 * time.Second):
		}

		connected := GetNetworkClientConnectionStatus(ctx, connectionId).Connected
		assert.Equal(t, connected, true)

		err = DisconnectNetworkClient(ctx, connectionId)
		assert.Equal(t, err, nil)

		connected = GetNetworkClientConnectionStatus(ctx, connectionId).Connected
		assert.Equal(t, connected, false)

		RemoveDisconnectedNetworkClients(ctx, time.Now())

		err = DisconnectNetworkClient(ctx, connectionId)
		assert.NotEqual(t, err, nil)
	})
}

// FIXME test GetNetworkClients, SetPendingNetworkClientConnection
