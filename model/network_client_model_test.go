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

		RemoveDisconnectedNetworkClients(ctx, time.Now(), time.Now(), time.Time{})

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

		RemoveDisconnectedNetworkClients(ctx, time.Now(), time.Now(), time.Time{})

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

		RemoveDisconnectedNetworkClients(ctx, time.Now(), time.Now(), time.Time{})

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

// The orphan safety-net sweep reaps location/latency/speed rows whose
// connection is gone, tls certificates and devices whose network_client is
// gone — while leaving rows with live parents in place.
func TestSweepOrphanConnectionAndClientData(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		location := &Location{
			City:        "foo",
			Region:      "bar",
			Country:     "United States",
			CountryCode: "us",
		}
		CreateLocation(ctx, location)

		// a connection with location/latency/speed rows
		newConnectionData := func(clientId server.Id, clientAddress string) server.Id {
			connectionId, _, _, _, err := ConnectNetworkClient(ctx, clientId, clientAddress, server.NewId())
			assert.Equal(t, err, nil)
			err = SetConnectionLocation(ctx, connectionId, location.LocationId, &ConnectionLocationScores{})
			assert.Equal(t, err, nil)
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`
					INSERT INTO network_client_latency (connection_id, latency_ms)
					VALUES ($1, $2)
					`,
					connectionId,
					42,
				))
				server.RaisePgResult(tx.Exec(
					ctx,
					`
					INSERT INTO network_client_speed (connection_id, bytes_per_second)
					VALUES ($1, $2)
					`,
					connectionId,
					1024,
				))
			})
			return connectionId
		}

		liveClientId := server.NewId()
		liveDeviceId := server.NewId()
		Testing_CreateDevice(ctx, server.NewId(), liveDeviceId, liveClientId, "test", "test")
		liveConnectionId := newConnectionData(liveClientId, "10.1.1.1:20000")
		SetClientTlsCertificateWithSignature(ctx, liveClientId, []byte("live-pem"), nil)

		// orphan the dependent rows: delete the connection row directly,
		// simulating a deletion path that did not cascade
		orphanConnectionId := newConnectionData(server.NewId(), "10.2.2.2:20000")
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`DELETE FROM network_client_connection WHERE connection_id = $1`,
				orphanConnectionId,
			))
		})

		// a tls certificate and a device with no network_client
		orphanClientId := server.NewId()
		SetClientTlsCertificateWithSignature(ctx, orphanClientId, []byte("orphan-pem"), nil)
		orphanDeviceId := server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				INSERT INTO device (device_id, network_id, device_name, device_spec, create_time)
				VALUES ($1, $2, $3, $4, now())
				`,
				orphanDeviceId,
				server.NewId(),
				"test",
				"test",
			))
		})

		SweepOrphanNetworkClientData(ctx, 1000)

		server.Db(ctx, func(conn server.PgConn) {
			countByConnection := func(table string, connectionId server.Id) int {
				c := 0
				result, err := conn.Query(
					ctx,
					`SELECT COUNT(*) FROM `+table+` WHERE connection_id = $1`,
					connectionId,
				)
				server.WithPgResult(result, err, func() {
					if result.Next() {
						server.Raise(result.Scan(&c))
					}
				})
				return c
			}
			for _, table := range []string{
				"network_client_location",
				"network_client_latency",
				"network_client_speed",
			} {
				assert.Equal(t, countByConnection(table, orphanConnectionId), 0)
				assert.Equal(t, countByConnection(table, liveConnectionId), 1)
			}

			deviceCount := func(deviceId server.Id) int {
				c := 0
				result, err := conn.Query(
					ctx,
					`SELECT COUNT(*) FROM device WHERE device_id = $1`,
					deviceId,
				)
				server.WithPgResult(result, err, func() {
					if result.Next() {
						server.Raise(result.Scan(&c))
					}
				})
				return c
			}
			assert.Equal(t, deviceCount(orphanDeviceId), 0)
			assert.Equal(t, deviceCount(liveDeviceId), 1)
		})

		orphanPem, _, err := GetClientTlsCertificateAndSignature(ctx, orphanClientId)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(orphanPem), 0)
		livePem, _, err := GetClientTlsCertificateAndSignature(ctx, liveClientId)
		assert.Equal(t, err, nil)
		assert.Equal(t, string(livePem), "live-pem")
	})
}

// The orphan safety-net sweep reaps provide_key rows whose network_client is
// gone; it must clear the redis entries too, not just postgres.
func TestSweepOrphanClearsProvideRedis(t *testing.T) {
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

		SweepOrphanNetworkClientData(ctx, 1000)

		server.Redis(ctx, func(r server.RedisClient) {
			pm, _ := r.Get(ctx, provideModesKey(clientId)).Result()
			assert.Equal(t, pm, "")
			sk, _ := r.Get(ctx, provideModeSecretKeyKey(clientId, ProvideModePublic)).Result()
			assert.Equal(t, sk, "")
		})
	})
}

// The reap cascades the dependent rows of exactly the reaped clients: the
// client's device, provide keys (+ redis mirrors), tls certificate, and proxy
// device config chain must all be removed together with the network_client
// row, while another network_client sharing the device keeps the device alive.
func TestRemoveDisconnectedCascadesReapedClients(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		deviceId := server.NewId()
		clientId := server.NewId()
		sharedDeviceId := server.NewId()
		sharedClientId := server.NewId()
		liveClientId := server.NewId()

		// the reaped client, with a device of its own
		Testing_CreateDevice(ctx, networkId, deviceId, clientId, "test", "test")
		// a reaped client that shares a device with a live client
		Testing_CreateDevice(ctx, networkId, sharedDeviceId, sharedClientId, "test", "test")
		server.Tx(ctx, func(tx server.PgTx) {
			// a second network_client on the same device (the device row
			// already exists)
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				INSERT INTO network_client (
					client_id,
					network_id,
					device_id,
					description,
					create_time,
					auth_time
				)
				VALUES ($1, $2, $3, $4, now(), now())
				`,
				liveClientId,
				networkId,
				sharedDeviceId,
				"test",
			))
		})

		secretKey := make([]byte, 32)
		mathrand.Read(secretKey)
		SetProvide(ctx, clientId, map[ProvideMode][]byte{
			ProvideModePublic: secretKey,
		})

		SetClientTlsCertificateWithSignature(ctx, clientId, []byte("test-pem"), []byte("test-sig"))

		proxyDeviceConfig := &ProxyDeviceConfig{}
		proxyDeviceConfig.ClientId = clientId
		err := CreateProxyDeviceConfig(ctx, proxyDeviceConfig)
		assert.Equal(t, err, nil)
		proxyClient, err := CreateProxyClient(
			ctx,
			proxyDeviceConfig.ProxyId,
			proxyDeviceConfig.ClientId,
			proxyDeviceConfig.InstanceId,
			CreateProxyClientOptions{},
		)
		assert.Equal(t, err, nil)

		// a disconnected connection with location/latency/speed rows, which
		// must be cascaded with the connection delete
		connectionId, _, _, _, err := ConnectNetworkClient(ctx, clientId, "10.7.8.9:20000", server.NewId())
		assert.Equal(t, err, nil)
		location := &Location{
			City:        "foo",
			Region:      "bar",
			Country:     "United States",
			CountryCode: "us",
		}
		CreateLocation(ctx, location)
		err = SetConnectionLocation(ctx, connectionId, location.LocationId, &ConnectionLocationScores{})
		assert.Equal(t, err, nil)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				INSERT INTO network_client_latency (connection_id, latency_ms)
				VALUES ($1, $2)
				`,
				connectionId,
				42,
			))
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				INSERT INTO network_client_speed (connection_id, bytes_per_second)
				VALUES ($1, $2)
				`,
				connectionId,
				1024,
			))
		})
		err = DisconnectNetworkClient(ctx, connectionId)
		assert.Equal(t, err, nil)

		// make clientId and sharedClientId reapable: created in the past and
		// inactive. liveClientId stays active.
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				UPDATE network_client
				SET active = false
				WHERE client_id = ANY($1::uuid[])
				`,
				[]string{clientId.String(), sharedClientId.String()},
			))
		})

		RemoveDisconnectedNetworkClients(ctx, server.NowUtc().Add(time.Minute), server.NowUtc().Add(time.Minute), time.Time{})

		// the connection and its location/latency/speed rows are gone
		server.Db(ctx, func(conn server.PgConn) {
			for _, table := range []string{
				"network_client_connection",
				"network_client_location",
				"network_client_latency",
				"network_client_speed",
			} {
				result, err := conn.Query(
					ctx,
					`SELECT COUNT(*) FROM `+table+` WHERE connection_id = $1`,
					connectionId,
				)
				server.WithPgResult(result, err, func() {
					assert.Equal(t, result.Next(), true)
					var c int
					server.Raise(result.Scan(&c))
					assert.Equal(t, c, 0)
				})
			}
		})

		// the reaped client's tls certificate is gone
		tlsCertificatePem, _, err := GetClientTlsCertificateAndSignature(ctx, clientId)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(tlsCertificatePem), 0)

		// the reaped client's provide keys and redis mirrors are gone
		provideModes, err := GetProvideModes(ctx, clientId)
		assert.Equal(t, err, nil)
		assert.Equal(t, len(provideModes), 0)
		server.Redis(ctx, func(r server.RedisClient) {
			pm, _ := r.Get(ctx, provideModesKey(clientId)).Result()
			assert.Equal(t, pm, "")
		})

		// the proxy config chain is gone
		assert.Equal(t, GetProxyDeviceConfig(ctx, proxyDeviceConfig.ProxyId) == nil, true)
		proxyClients, _, err := GetProxyClientsSince(ctx, proxyClient.ProxyHost, proxyClient.Block, 0)
		assert.Equal(t, err, nil)
		_, ok := proxyClients[proxyClient.ProxyId]
		assert.Equal(t, ok, false)

		// the reaped client's own device is gone; the shared device survives
		// because the live client still references it
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`
				SELECT device_id FROM device
				WHERE device_id = ANY($1::uuid[])
				`,
				[]string{deviceId.String(), sharedDeviceId.String()},
			)
			remainingDeviceIds := map[server.Id]bool{}
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var remainingDeviceId server.Id
					server.Raise(result.Scan(&remainingDeviceId))
					remainingDeviceIds[remainingDeviceId] = true
				}
			})
			assert.Equal(t, remainingDeviceIds[deviceId], false)
			assert.Equal(t, remainingDeviceIds[sharedDeviceId], true)
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

// the jwt refresh path rejects removed (inactive) and deleted clients via
// FindActiveClientNetwork, so the app logs out instead of refreshing a dead
// client. FindClientNetwork keeps existence-only semantics for other callers.
func TestFindActiveClientNetwork(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		clientId := server.NewId()
		Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")

		foundNetworkId, err := FindActiveClientNetwork(ctx, clientId)
		assert.Equal(t, err, nil)
		assert.Equal(t, foundNetworkId, networkId)

		// removed (inactive) client: existence-only lookup still resolves,
		// the active lookup does not
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE network_client SET active = false WHERE client_id = $1`,
				clientId,
			))
		})
		_, err = FindClientNetwork(ctx, clientId)
		assert.Equal(t, err, nil)
		_, err = FindActiveClientNetwork(ctx, clientId)
		assert.NotEqual(t, err, nil)

		// deleted client
		_, err = FindActiveClientNetwork(ctx, server.NewId())
		assert.NotEqual(t, err, nil)
	})
}

// Abandoned top-level clients (no auth/connect for TopLevelClientIdleExpiration,
// no live connection) are marked inactive — which makes the jwt refresh fail so
// the app logs out — and hard deleted NetworkClientReapAfterDeactivate after
// deactivation. Connected or recently seen clients, and child clients, are
// never marked by this pass.
func TestRemoveDisconnectedNetworkClientsTopLevelReap(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()

		newClient := func() server.Id {
			clientId := server.NewId()
			Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")
			return clientId
		}
		setAuthTime := func(clientId server.Id, authTime time.Time) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`UPDATE network_client SET auth_time = $2 WHERE client_id = $1`,
					clientId,
					authTime,
				))
			})
		}
		clientState := func(clientId server.Id) (exists bool, active bool, deactivateTime *time.Time) {
			server.Db(ctx, func(conn server.PgConn) {
				result, err := conn.Query(
					ctx,
					`SELECT active, deactivate_time FROM network_client WHERE client_id = $1`,
					clientId,
				)
				server.WithPgResult(result, err, func() {
					if result.Next() {
						exists = true
						server.Raise(result.Scan(&active, &deactivateTime))
					}
				})
			})
			return
		}

		now := server.NowUtc()
		idleAuthTime := now.Add(-TopLevelClientIdleExpiration - 24*time.Hour)

		// abandoned: idle past the expiration, no connection
		idleClientId := newClient()
		setAuthTime(idleClientId, idleAuthTime)

		// stale auth time but currently connected: must not be marked
		connectedClientId := newClient()
		_, _, _, _, err := ConnectNetworkClient(ctx, connectedClientId, "127.0.0.1:20000", server.NewId())
		assert.Equal(t, err, nil)
		setAuthTime(connectedClientId, idleAuthTime)

		// recently seen
		freshClientId := newClient()

		// idle child client: handled by the child reap, not the marker
		childClientId := newClient()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE network_client SET source_client_id = $2 WHERE client_id = $1`,
				childClientId,
				freshClientId,
			))
		})
		setAuthTime(childClientId, idleAuthTime)

		minConnectionTime := now.Add(-8 * time.Hour)
		minClientTime := now.Add(-NetworkClientReapAfterDeactivate)
		minTopLevelAuthTime := now.Add(-TopLevelClientIdleExpiration)
		RemoveDisconnectedNetworkClients(ctx, minConnectionTime, minClientTime, minTopLevelAuthTime)

		// only the abandoned top-level client is marked
		exists, active, deactivateTime := clientState(idleClientId)
		assert.Equal(t, exists, true)
		assert.Equal(t, active, false)
		assert.NotEqual(t, deactivateTime, nil)

		// marking makes the refresh lookup fail (the app logs out on this)
		_, err = FindActiveClientNetwork(ctx, idleClientId)
		assert.NotEqual(t, err, nil)

		_, active, _ = clientState(connectedClientId)
		assert.Equal(t, active, true)
		_, active, _ = clientState(freshClientId)
		assert.Equal(t, active, true)

		// the idle child client was reaped by the child pass (auth_time based),
		// not marked inactive
		exists, _, _ = clientState(childClientId)
		assert.Equal(t, exists, false)

		// within the grace window the marked client is retained
		exists, _, _ = clientState(idleClientId)
		assert.Equal(t, exists, true)

		// after the grace window it is hard deleted
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE network_client SET deactivate_time = $2 WHERE client_id = $1`,
				idleClientId,
				now.Add(-NetworkClientReapAfterDeactivate-24*time.Hour),
			))
		})
		RemoveDisconnectedNetworkClients(ctx, minConnectionTime, minClientTime, minTopLevelAuthTime)
		exists, _, _ = clientState(idleClientId)
		assert.Equal(t, exists, false)

		// user removal stamps deactivate_time, so removed clients also reap 30
		// days after removal
		removedClientId := newClient()
		userSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			ClientId:  &removedClientId,
		})
		removeResult, err := RemoveNetworkClient(&RemoveNetworkClientArgs{
			ClientId: removedClientId,
		}, userSession)
		assert.Equal(t, err, nil)
		assert.Equal(t, removeResult.Error, nil)
		exists, active, deactivateTime = clientState(removedClientId)
		assert.Equal(t, exists, true)
		assert.Equal(t, active, false)
		assert.NotEqual(t, deactivateTime, nil)
	})
}
