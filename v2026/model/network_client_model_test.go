package model

import (
	"context"
	mathrand "math/rand"
	"testing"
	"time"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/server/v2026"
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

		CloseExpiredNetworkClientHandlers(ctx, server.NowUtc())

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

		CloseExpiredNetworkClientHandlers(ctx, server.NowUtc())

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

func TestSetProvide(t *testing.T) {
	server.DefaultTestEnv().Run(func() {

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		newSecretKeys := func() map[ProvideMode][]byte {
			k := make([]byte, 32)
			mathrand.Read(k)
			return map[ProvideMode][]byte{
				ProvideModePublic: k,
			}
		}

		clientId := server.NewId()
		secretKeys := newSecretKeys()

		startTime := server.NowUtc()

		changeCount, provideModes := GetProvideKeyChanges(ctx, clientId, startTime)
		assert.Equal(t, changeCount, 0)
		assert.Equal(t, provideModes, map[ProvideMode]bool{})

		for provideMode, _ := range secretKeys {
			_, err := GetProvideSecretKey(ctx, clientId, provideMode)
			assert.NotEqual(t, err, nil)
		}

		SetProvide(ctx, clientId, secretKeys)

		for provideMode, secretKey := range secretKeys {
			k, err := GetProvideSecretKey(ctx, clientId, provideMode)
			assert.Equal(t, err, nil)
			assert.Equal(t, k, secretKey)
		}

		changeCount, provideModes = GetProvideKeyChanges(ctx, clientId, startTime)
		assert.Equal(t, changeCount, 1)
		assert.Equal(t, provideModes, map[ProvideMode]bool{
			ProvideModePublic: true,
		})

		n := 32
		for range n {
			secretKeys = newSecretKeys()
			SetProvide(ctx, clientId, secretKeys)
		}

		changeCount, provideModes = GetProvideKeyChanges(ctx, clientId, startTime)
		assert.Equal(t, changeCount, n+1)
		assert.Equal(t, provideModes, map[ProvideMode]bool{
			ProvideModePublic: true,
		})

		RemoveOldProvideKeyChanges(ctx, server.NowUtc())

		changeCount, provideModes = GetProvideKeyChanges(ctx, clientId, startTime)
		assert.Equal(t, changeCount, 0)
		assert.Equal(t, provideModes, map[ProvideMode]bool{
			ProvideModePublic: true,
		})

	})
}
