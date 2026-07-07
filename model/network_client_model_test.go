package model

import (
	"context"
	"encoding/json"
	mathrand "math/rand"
	"testing"
	"time"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/session"
)

func TestNetworkClientHandlerLifecycle(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
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

		RemoveDisconnectedNetworkClients(ctx, time.Now(), time.Now())

		err = DisconnectNetworkClient(ctx, connectionId)
		assert.NotEqual(t, err, nil)

		err = HeartbeatNetworkClientHandler(ctx, handlerId)
		assert.NotEqual(t, err, nil)
	})
}

func TestNetworkClientHandlerLifecycleIPV6(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
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

		RemoveDisconnectedNetworkClients(ctx, time.Now(), time.Now())

		err = DisconnectNetworkClient(ctx, connectionId)
		assert.NotEqual(t, err, nil)

		err = HeartbeatNetworkClientHandler(ctx, handlerId)
		assert.NotEqual(t, err, nil)
	})
}

func TestNetworkClientLifecycle(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
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

		RemoveDisconnectedNetworkClients(ctx, time.Now(), time.Now())

		err = DisconnectNetworkClient(ctx, connectionId)
		assert.NotEqual(t, err, nil)
	})
}

// FIXME test GetNetworkClients, SetPendingNetworkClientConnection

func TestSetProvide(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

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

// GetProvideModes / GetProvideSecretKey must fall back to postgres when the
// redis cache is cold (data written before the redis layer existed, or evicted).
// Regression: a redis miss used to leak a non-nil error even though the db
// fallback found the data, which callers treat as "no permission".
func TestGetProvideFallsBackToDb(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		clientId := server.NewId()
		secretKey := make([]byte, 32)
		mathrand.Read(secretKey)
		secretKeys := map[ProvideMode][]byte{
			ProvideModePublic: secretKey,
		}

		SetProvide(ctx, clientId, secretKeys)

		// drop the cache so the reads are forced through the db fallback
		server.Redis(ctx, func(r server.RedisClient) {
			r.Del(ctx, provideModesKey(clientId))
			r.Del(ctx, provideModeSecretKeyKey(clientId, ProvideModePublic))
		})

		provideModes, err := GetProvideModes(ctx, clientId)
		assert.Equal(t, err, nil)
		assert.Equal(t, provideModes, map[ProvideMode]bool{
			ProvideModePublic: true,
		})

		k, err := GetProvideSecretKey(ctx, clientId, ProvideModePublic)
		assert.Equal(t, err, nil)
		assert.Equal(t, k, secretKey)
	})
}

func TestGetProvideModesNotSet(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		clientId := server.NewId()

		// a client that never provided returns an empty set and no error
		provideModes, err := GetProvideModes(ctx, clientId)
		assert.Equal(t, err, nil)
		assert.Equal(t, provideModes, map[ProvideMode]bool{})

		// the secret key for a never-provided client is an error
		_, err = GetProvideSecretKey(ctx, clientId, ProvideModePublic)
		assert.NotEqual(t, err, nil)
	})
}

// Re-providing with fewer modes must drop the removed mode's secret key from
// both postgres and redis. Exercises the multi-mode json round-trip and the
// removedProvideModes cleanup, neither of which the single-mode TestSetProvide
// covers.
func TestSetProvideRemovesStaleModes(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		clientId := server.NewId()
		key := func() []byte {
			k := make([]byte, 32)
			mathrand.Read(k)
			return k
		}

		SetProvide(ctx, clientId, map[ProvideMode][]byte{
			ProvideModePublic:  key(),
			ProvideModeNetwork: key(),
		})

		provideModes, err := GetProvideModes(ctx, clientId)
		assert.Equal(t, err, nil)
		assert.Equal(t, provideModes, map[ProvideMode]bool{
			ProvideModePublic:  true,
			ProvideModeNetwork: true,
		})

		publicKey := key()
		SetProvide(ctx, clientId, map[ProvideMode][]byte{
			ProvideModePublic: publicKey,
		})

		provideModes, err = GetProvideModes(ctx, clientId)
		assert.Equal(t, err, nil)
		assert.Equal(t, provideModes, map[ProvideMode]bool{
			ProvideModePublic: true,
		})

		// the dropped mode is gone from the api and from redis
		_, err = GetProvideSecretKey(ctx, clientId, ProvideModeNetwork)
		assert.NotEqual(t, err, nil)
		server.Redis(ctx, func(r server.RedisClient) {
			v, _ := r.Get(ctx, provideModeSecretKeyKey(clientId, ProvideModeNetwork)).Result()
			assert.Equal(t, v, "")
		})

		k, err := GetProvideSecretKey(ctx, clientId, ProvideModePublic)
		assert.Equal(t, err, nil)
		assert.Equal(t, k, publicKey)
	})
}

// The maintenance cascade reaps provide_key rows whose network_client is gone;
// it must clear the redis entries too, not just postgres.
func TestRemoveDisconnectedClearsProvideRedis(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		// a client_id with no backing network_client row
		clientId := server.NewId()
		secretKey := make([]byte, 32)
		mathrand.Read(secretKey)
		SetProvide(ctx, clientId, map[ProvideMode][]byte{
			ProvideModePublic: secretKey,
		})

		server.Redis(ctx, func(r server.RedisClient) {
			v, _ := r.Get(ctx, provideModesKey(clientId)).Result()
			assert.NotEqual(t, v, "")
		})

		RemoveDisconnectedNetworkClients(ctx, server.NowUtc(), server.NowUtc())

		server.Redis(ctx, func(r server.RedisClient) {
			pm, _ := r.Get(ctx, provideModesKey(clientId)).Result()
			assert.Equal(t, pm, "")
			sk, _ := r.Get(ctx, provideModeSecretKeyKey(clientId, ProvideModePublic)).Result()
			assert.Equal(t, sk, "")
		})
	})
}

// MigrateProvideMode backfills redis for clients whose provide_key rows predate
// the redis layer. Seed via SetProvide (which writes both stores), drop the
// redis keys to simulate the pre-redis state, then assert the migration rebuilds
// them from the db. Two modes exercise the provideModesKey json-list round-trip.
func TestMigrateProvideMode(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		key := func() []byte {
			k := make([]byte, 32)
			mathrand.Read(k)
			return k
		}

		clientId := server.NewId()
		publicKey := key()
		networkKey := key()
		SetProvide(ctx, clientId, map[ProvideMode][]byte{
			ProvideModePublic:  publicKey,
			ProvideModeNetwork: networkKey,
		})

		// drop the redis keys, leaving only the db rows for the migration to read
		server.Redis(ctx, func(r server.RedisClient) {
			r.Del(ctx, provideModesKey(clientId))
			r.Del(ctx, provideModeSecretKeyKey(clientId, ProvideModePublic))
			r.Del(ctx, provideModeSecretKeyKey(clientId, ProvideModeNetwork))
		})

		MigrateProvideMode(ctx, 50000)

		server.Redis(ctx, func(r server.RedisClient) {
			provideModesListJson, err := r.Get(ctx, provideModesKey(clientId)).Result()
			assert.Equal(t, err, nil)
			var provideModesList []ProvideMode
			err = json.Unmarshal([]byte(provideModesListJson), &provideModesList)
			assert.Equal(t, err, nil)
			provideModes := map[ProvideMode]bool{}
			for _, provideMode := range provideModesList {
				provideModes[provideMode] = true
			}
			assert.Equal(t, provideModes, map[ProvideMode]bool{
				ProvideModePublic:  true,
				ProvideModeNetwork: true,
			})

			publicSk, _ := r.Get(ctx, provideModeSecretKeyKey(clientId, ProvideModePublic)).Result()
			assert.Equal(t, []byte(publicSk), publicKey)
			networkSk, _ := r.Get(ctx, provideModeSecretKeyKey(clientId, ProvideModeNetwork)).Result()
			assert.Equal(t, []byte(networkSk), networkKey)
		})
	})
}

func TestAuthNetworkClientReuse(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()
		networkName := "test-reuse"

		Testing_CreateNetwork(ctx, networkId, networkName, userId)

		authSessionId := server.NewId()
		byJwt := jwt.NewByJwt(networkId, userId, networkName, false, false, authSessionId)
		signed := byJwt.Sign()
		parsedByJwt, parseErr := jwt.ParseByJwt(ctx, signed)
		assert.Equal(t, parseErr, nil)

		cancelCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		sess := &session.ClientSession{
			Ctx:    cancelCtx,
			Cancel: cancel,
			ByJwt:  parsedByJwt,
		}

		// Mint a fresh client (ClientId == nil)
		createResult, createErr := AuthNetworkClient(
			&AuthNetworkClientArgs{
				Description: "test-client",
				ClientId:    nil,
			},
			sess,
		)
		assert.Equal(t, createErr, nil)
		assert.NotEqual(t, createResult, nil)
		assert.NotEqual(t, createResult.ByClientJwt, nil)
		assert.NotEqual(t, createResult.ClientId, nil)
		createdId := *createResult.ClientId

		// Reuse the client (ClientId != nil) — this was the broken path
		reuseResult, reuseErr := AuthNetworkClient(
			&AuthNetworkClientArgs{
				Description: "test-client-reuse",
				ClientId:    &createdId,
			},
			sess,
		)
		assert.Equal(t, reuseErr, nil)
		assert.NotEqual(t, reuseResult, nil)
		assert.NotEqual(t, reuseResult.ByClientJwt, nil)
		assert.NotEqual(t, reuseResult.ClientId, nil)
		assert.Equal(t, *reuseResult.ClientId, createdId)
	})
}
