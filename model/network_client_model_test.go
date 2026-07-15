package model

import (
	"context"
	"encoding/json"
	"fmt"
	mathrand "math/rand"
	"testing"
	"time"

	"github.com/urnetwork/connect"

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
		connect.AssertEqual(t, err, nil)

		err = HeartbeatNetworkClientHandler(ctx, handlerId)
		connect.AssertEqual(t, err, nil)

		select {
		case <-time.After(1 * time.Second):
		}

		connected := GetNetworkClientConnectionStatus(ctx, connectionId).Connected
		connect.AssertEqual(t, connected, true)

		CloseExpiredNetworkClientHandlers(ctx, server.NowUtc())

		connected = GetNetworkClientConnectionStatus(ctx, connectionId).Connected
		connect.AssertEqual(t, connected, false)

		select {
		case <-time.After(1 * time.Second):
		}

		RemoveDisconnectedNetworkClients(ctx, time.Now(), time.Now(), time.Time{})

		err = DisconnectNetworkClient(ctx, connectionId)
		connect.AssertNotEqual(t, err, nil)

		err = HeartbeatNetworkClientHandler(ctx, handlerId)
		connect.AssertNotEqual(t, err, nil)
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
		connect.AssertEqual(t, err, nil)

		err = HeartbeatNetworkClientHandler(ctx, handlerId)
		connect.AssertEqual(t, err, nil)

		time.Sleep(1 * time.Second)

		connected := GetNetworkClientConnectionStatus(ctx, connectionId).Connected
		connect.AssertEqual(t, connected, true)

		CloseExpiredNetworkClientHandlers(ctx, server.NowUtc())

		connected = GetNetworkClientConnectionStatus(ctx, connectionId).Connected
		connect.AssertEqual(t, connected, false)

		time.Sleep(1 * time.Second)

		RemoveDisconnectedNetworkClients(ctx, time.Now(), time.Now(), time.Time{})

		err = DisconnectNetworkClient(ctx, connectionId)
		connect.AssertNotEqual(t, err, nil)

		err = HeartbeatNetworkClientHandler(ctx, handlerId)
		connect.AssertNotEqual(t, err, nil)
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
		connect.AssertEqual(t, err, nil)

		select {
		case <-time.After(1 * time.Second):
		}

		connected := GetNetworkClientConnectionStatus(ctx, connectionId).Connected
		connect.AssertEqual(t, connected, true)

		err = DisconnectNetworkClient(ctx, connectionId)
		connect.AssertEqual(t, err, nil)

		connected = GetNetworkClientConnectionStatus(ctx, connectionId).Connected
		connect.AssertEqual(t, connected, false)

		RemoveDisconnectedNetworkClients(ctx, time.Now(), time.Now(), time.Time{})

		err = DisconnectNetworkClient(ctx, connectionId)
		connect.AssertNotEqual(t, err, nil)
	})
}

// Round trip of the pending client connection marker through the real write
// (SetPendingNetworkClientConnection) and read (GetNetworkClients, which reads
// the per-client keys in a plain pipeline) paths, pinning the per-client key
// format (`{pcc_<clientId>}`) and the expiry. The test redis is standalone,
// not a cluster, so this proves functional equivalence of the per-client-tag
// keys and the pipelined per-key gets that replaced the cross-slot mget, not
// slot placement.
func TestPendingNetworkClientConnection(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()

		Testing_CreateNetwork(ctx, networkId, "test", userId)
		userSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			UserId:    userId,
		})
		authClientResult, err := AuthNetworkClient(
			&AuthNetworkClientArgs{
				Description: "test device",
				DeviceSpec:  "test spec",
			},
			userSession,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, authClientResult.Error, nil)
		clientId := *authClientResult.ClientId

		connect.AssertEqual(t, fmt.Sprintf("{pcc_%s}", clientId), pendingClientConnectionKey(clientId))

		// no pending connection yet
		clientsResult, err := GetNetworkClients(userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, 1, len(clientsResult.Clients))
		connect.AssertEqual(t, 0, len(clientsResult.Clients[0].Connections))

		expire := 1 * time.Second
		SetPendingNetworkClientConnection(ctx, clientId, expire)
		server.Redis(ctx, func(r server.RedisClient) {
			ttl := r.TTL(ctx, pendingClientConnectionKey(clientId)).Val()
			connect.AssertEqual(t, true, 0 < ttl && ttl <= expire)
		})

		clientsResult, err = GetNetworkClients(userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, 1, len(clientsResult.Clients))
		connect.AssertEqual(t, 1, len(clientsResult.Clients[0].Connections))
		pendingConnection := clientsResult.Clients[0].Connections[0]
		connect.AssertEqual(t, clientId, pendingConnection.ClientId)
		// a pending connection is marked with the client id as connection id
		connect.AssertEqual(t, clientId, pendingConnection.ConnectionId)

		// after the expiry the marker is gone (the pipelined read tolerates
		// the missing key)
		select {
		case <-time.After(expire + 500*time.Millisecond):
		}
		clientsResult, err = GetNetworkClients(userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, 1, len(clientsResult.Clients))
		connect.AssertEqual(t, 0, len(clientsResult.Clients[0].Connections))
	})
}

// Round trip of the client error counters through ClientError, asserting the
// four per-call increments behave exactly as before the key split (each call
// bumps all four counters by one) and that every key carries the ttl. Pins
// the split key formats: client-scoped `{ce_<clientId>}...` and
// network-scoped `{cen_<networkId>}...`. The test redis is standalone, not a
// cluster, so this proves functional equivalence of the split-tag keys and
// the plain (auto-routing) pipeline, not slot placement.
func TestClientErrorCounters(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		clientId := server.NewId()
		connectionId := server.NewId()

		errorMessage := "test error"
		connect.AssertEqual(t, fmt.Sprintf("{ce_%s}count", clientId), clientErrorCountKey(clientId))
		connect.AssertEqual(t, fmt.Sprintf("{ce_%s}message_%s", clientId, errorMessage), clientErrorMessageCountKey(clientId, errorMessage))
		connect.AssertEqual(t, fmt.Sprintf("{cen_%s}count", networkId), networkErrorCountKey(networkId))
		connect.AssertEqual(t, fmt.Sprintf("{cen_%s}message_%s", networkId, errorMessage), networkErrorMessageCountKey(networkId, errorMessage))

		ClientError(ctx, networkId, clientId, connectionId, "read", fmt.Errorf("%s", errorMessage))
		ClientError(ctx, networkId, clientId, connectionId, "read", fmt.Errorf("%s", errorMessage))

		server.Redis(ctx, func(r server.RedisClient) {
			for _, key := range []string{
				clientErrorCountKey(clientId),
				clientErrorMessageCountKey(clientId, errorMessage),
				networkErrorCountKey(networkId),
				networkErrorMessageCountKey(networkId, errorMessage),
			} {
				count, err := r.Get(ctx, key).Int64()
				connect.AssertEqual(t, nil, err)
				connect.AssertEqual(t, int64(2), count)

				ttl := r.TTL(ctx, key).Val()
				connect.AssertEqual(t, true, 0 < ttl && ttl <= 5*time.Minute)
			}
		})
	})
}

// The provide mirror keys (`{pm_<clientId>}pms`, `{pm_<clientId>}sk_<n>`,
// `{pm_<clientId>}rp`) are caches over postgres and must carry a ttl so idle
// clients' keys expire instead of accumulating without bound. Asserts the ttl
// after each real cache-write path: SetProvide for the provide modes list and
// secret keys, and the GetClientIdentity read-through refill for the identity.
func TestProvideMirrorTtl(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		clientId := server.NewId()
		secretKey := make([]byte, 32)
		mathrand.Read(secretKey)

		SetProvide(ctx, clientId, map[ProvideMode][]byte{
			ProvideModePublic: secretKey,
		})

		// the identity read-through caches the (empty) identity with a ttl
		identity := GetClientIdentity(ctx, clientId)
		connect.AssertNotEqual(t, nil, identity)

		server.Redis(ctx, func(r server.RedisClient) {
			for _, key := range []string{
				provideModesKey(clientId),
				provideModeSecretKeyKey(clientId, ProvideModePublic),
				clientIdentityKey(clientId),
			} {
				ttl := r.TTL(ctx, key).Val()
				connect.AssertEqual(t, true, 0 < ttl && ttl <= provideMirrorTtl)
			}
		})
	})
}

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
		connect.AssertEqual(t, changeCount, 0)
		connect.AssertEqual(t, provideModes, map[ProvideMode]bool{})

		for provideMode, _ := range secretKeys {
			_, err := GetProvideSecretKey(ctx, clientId, provideMode)
			connect.AssertNotEqual(t, err, nil)
		}

		SetProvide(ctx, clientId, secretKeys)

		for provideMode, secretKey := range secretKeys {
			k, err := GetProvideSecretKey(ctx, clientId, provideMode)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, k, secretKey)
		}

		changeCount, provideModes = GetProvideKeyChanges(ctx, clientId, startTime)
		connect.AssertEqual(t, changeCount, 1)
		connect.AssertEqual(t, provideModes, map[ProvideMode]bool{
			ProvideModePublic: true,
		})

		n := 32
		for range n {
			secretKeys = newSecretKeys()
			SetProvide(ctx, clientId, secretKeys)
		}

		changeCount, provideModes = GetProvideKeyChanges(ctx, clientId, startTime)
		connect.AssertEqual(t, changeCount, n+1)
		connect.AssertEqual(t, provideModes, map[ProvideMode]bool{
			ProvideModePublic: true,
		})

		RemoveOldProvideKeyChanges(ctx, server.NowUtc())

		changeCount, provideModes = GetProvideKeyChanges(ctx, clientId, startTime)
		connect.AssertEqual(t, changeCount, 0)
		connect.AssertEqual(t, provideModes, map[ProvideMode]bool{
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
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, provideModes, map[ProvideMode]bool{
			ProvideModePublic: true,
		})

		k, err := GetProvideSecretKey(ctx, clientId, ProvideModePublic)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, k, secretKey)
	})
}

func TestGetProvideModesNotSet(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		clientId := server.NewId()

		// a client that never provided returns an empty set and no error
		provideModes, err := GetProvideModes(ctx, clientId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, provideModes, map[ProvideMode]bool{})

		// the secret key for a never-provided client is an error
		_, err = GetProvideSecretKey(ctx, clientId, ProvideModePublic)
		connect.AssertNotEqual(t, err, nil)
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
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, provideModes, map[ProvideMode]bool{
			ProvideModePublic:  true,
			ProvideModeNetwork: true,
		})

		publicKey := key()
		SetProvide(ctx, clientId, map[ProvideMode][]byte{
			ProvideModePublic: publicKey,
		})

		provideModes, err = GetProvideModes(ctx, clientId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, provideModes, map[ProvideMode]bool{
			ProvideModePublic: true,
		})

		// the dropped mode is gone from the api and from redis
		_, err = GetProvideSecretKey(ctx, clientId, ProvideModeNetwork)
		connect.AssertNotEqual(t, err, nil)
		server.Redis(ctx, func(r server.RedisClient) {
			v, _ := r.Get(ctx, provideModeSecretKeyKey(clientId, ProvideModeNetwork)).Result()
			connect.AssertEqual(t, v, "")
		})

		k, err := GetProvideSecretKey(ctx, clientId, ProvideModePublic)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, k, publicKey)
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
			connect.AssertEqual(t, err, nil)
			err = SetConnectionLocation(ctx, connectionId, location.LocationId, &ConnectionLocationScores{})
			connect.AssertEqual(t, err, nil)
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
				connect.AssertEqual(t, countByConnection(table, orphanConnectionId), 0)
				connect.AssertEqual(t, countByConnection(table, liveConnectionId), 1)
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
			connect.AssertEqual(t, deviceCount(orphanDeviceId), 0)
			connect.AssertEqual(t, deviceCount(liveDeviceId), 1)
		})

		orphanPem, _, err := GetClientTlsCertificateAndSignature(ctx, orphanClientId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(orphanPem), 0)
		livePem, _, err := GetClientTlsCertificateAndSignature(ctx, liveClientId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, string(livePem), "live-pem")
	})
}

// The bounded cursor sweep must page the network-client dependent tables across
// many slices without skipping rows at slice boundaries. With more orphan + live
// rows than one slice, every orphan is removed and every live row survives,
// however the random keys interleave in primary-key order. This covers three key
// shapes at once: the generic single-uuid path (device) and the two bespoke
// inline cursor loops that carry pagination on a UNION sentinel row
// (proxy_device_config, single-uuid; provide_key, composite (client_id,
// provide_mode)). sliceSize=2 forces each table across several slices.
func TestSweepOrphanNetworkClientDataMultiSlice(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		newSecretKey := func() []byte {
			k := make([]byte, 32)
			mathrand.Read(k)
			return k
		}

		// live rows: each shares a client_id with a real network_client (created by
		// Testing_CreateDevice), so device/provide_key/proxy_device_config all have a
		// live parent and must survive.
		liveCount := 4
		liveDeviceIds := []server.Id{}
		liveProvideClientIds := []server.Id{}
		liveProxyIds := []server.Id{}
		for range liveCount {
			deviceId := server.NewId()
			clientId := server.NewId()
			Testing_CreateDevice(ctx, server.NewId(), deviceId, clientId, "test", "test")
			SetProvide(ctx, clientId, map[ProvideMode][]byte{ProvideModePublic: newSecretKey()})
			pdc := &ProxyDeviceConfig{}
			pdc.ClientId = clientId
			err := CreateProxyDeviceConfig(ctx, pdc)
			connect.AssertEqual(t, err, nil)
			liveDeviceIds = append(liveDeviceIds, deviceId)
			liveProvideClientIds = append(liveProvideClientIds, clientId)
			liveProxyIds = append(liveProxyIds, pdc.ProxyId)
		}

		// orphan rows: no network_client references them
		orphanCount := 5
		orphanDeviceIds := []server.Id{}
		orphanProvideClientIds := []server.Id{}
		orphanProxyIds := []server.Id{}
		for range orphanCount {
			deviceId := server.NewId()
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`INSERT INTO device (device_id, network_id, device_name, device_spec, create_time) VALUES ($1, $2, $3, $4, now())`,
					deviceId,
					server.NewId(),
					"test",
					"test",
				))
			})
			orphanDeviceIds = append(orphanDeviceIds, deviceId)

			provideClientId := server.NewId()
			SetProvide(ctx, provideClientId, map[ProvideMode][]byte{ProvideModePublic: newSecretKey()})
			orphanProvideClientIds = append(orphanProvideClientIds, provideClientId)

			pdc := &ProxyDeviceConfig{}
			pdc.ClientId = server.NewId()
			err := CreateProxyDeviceConfig(ctx, pdc)
			connect.AssertEqual(t, err, nil)
			orphanProxyIds = append(orphanProxyIds, pdc.ProxyId)
		}

		SweepOrphanNetworkClientData(ctx, 2)

		server.Db(ctx, func(conn server.PgConn) {
			exists := func(sql string, id server.Id) bool {
				found := false
				result, err := conn.Query(ctx, sql, id)
				server.WithPgResult(result, err, func() {
					found = result.Next()
				})
				return found
			}
			// every orphan row is gone
			for _, id := range orphanDeviceIds {
				connect.AssertEqual(t, exists(`SELECT 1 FROM device WHERE device_id = $1`, id), false)
			}
			for _, id := range orphanProvideClientIds {
				connect.AssertEqual(t, exists(`SELECT 1 FROM provide_key WHERE client_id = $1`, id), false)
			}
			for _, id := range orphanProxyIds {
				connect.AssertEqual(t, exists(`SELECT 1 FROM proxy_device_config WHERE proxy_id = $1`, id), false)
			}
			// every live row survives
			for _, id := range liveDeviceIds {
				connect.AssertEqual(t, exists(`SELECT 1 FROM device WHERE device_id = $1`, id), true)
			}
			for _, id := range liveProvideClientIds {
				connect.AssertEqual(t, exists(`SELECT 1 FROM provide_key WHERE client_id = $1`, id), true)
			}
			for _, id := range liveProxyIds {
				connect.AssertEqual(t, exists(`SELECT 1 FROM proxy_device_config WHERE proxy_id = $1`, id), true)
			}
		})
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
			connect.AssertNotEqual(t, v, "")
		})

		SweepOrphanNetworkClientData(ctx, 1000)

		server.Redis(ctx, func(r server.RedisClient) {
			pm, _ := r.Get(ctx, provideModesKey(clientId)).Result()
			connect.AssertEqual(t, pm, "")
			sk, _ := r.Get(ctx, provideModeSecretKeyKey(clientId, ProvideModePublic)).Result()
			connect.AssertEqual(t, sk, "")
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
		connect.AssertEqual(t, err, nil)
		proxyClient, err := CreateProxyClient(
			ctx,
			proxyDeviceConfig.ProxyId,
			proxyDeviceConfig.ClientId,
			proxyDeviceConfig.InstanceId,
			CreateProxyClientOptions{},
		)
		connect.AssertEqual(t, err, nil)

		// a disconnected connection with location/latency/speed rows, which
		// must be cascaded with the connection delete
		connectionId, _, _, _, err := ConnectNetworkClient(ctx, clientId, "10.7.8.9:20000", server.NewId())
		connect.AssertEqual(t, err, nil)
		location := &Location{
			City:        "foo",
			Region:      "bar",
			Country:     "United States",
			CountryCode: "us",
		}
		CreateLocation(ctx, location)
		err = SetConnectionLocation(ctx, connectionId, location.LocationId, &ConnectionLocationScores{})
		connect.AssertEqual(t, err, nil)
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
		connect.AssertEqual(t, err, nil)

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
					connect.AssertEqual(t, result.Next(), true)
					var c int
					server.Raise(result.Scan(&c))
					connect.AssertEqual(t, c, 0)
				})
			}
		})

		// the reaped client's tls certificate is gone
		tlsCertificatePem, _, err := GetClientTlsCertificateAndSignature(ctx, clientId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(tlsCertificatePem), 0)

		// the reaped client's provide keys and redis mirrors are gone
		provideModes, err := GetProvideModes(ctx, clientId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(provideModes), 0)
		server.Redis(ctx, func(r server.RedisClient) {
			pm, _ := r.Get(ctx, provideModesKey(clientId)).Result()
			connect.AssertEqual(t, pm, "")
		})

		// the proxy config chain is gone
		connect.AssertEqual(t, GetProxyDeviceConfig(ctx, proxyDeviceConfig.ProxyId) == nil, true)
		proxyClients, _, err := GetProxyClientsSince(ctx, proxyClient.ProxyHost, proxyClient.Block, 0)
		connect.AssertEqual(t, err, nil)
		_, ok := proxyClients[proxyClient.ProxyId]
		connect.AssertEqual(t, ok, false)

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
			connect.AssertEqual(t, remainingDeviceIds[deviceId], false)
			connect.AssertEqual(t, remainingDeviceIds[sharedDeviceId], true)
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
			connect.AssertEqual(t, err, nil)
			var provideModesList []ProvideMode
			err = json.Unmarshal([]byte(provideModesListJson), &provideModesList)
			connect.AssertEqual(t, err, nil)
			provideModes := map[ProvideMode]bool{}
			for _, provideMode := range provideModesList {
				provideModes[provideMode] = true
			}
			connect.AssertEqual(t, provideModes, map[ProvideMode]bool{
				ProvideModePublic:  true,
				ProvideModeNetwork: true,
			})

			publicSk, _ := r.Get(ctx, provideModeSecretKeyKey(clientId, ProvideModePublic)).Result()
			connect.AssertEqual(t, []byte(publicSk), publicKey)
			networkSk, _ := r.Get(ctx, provideModeSecretKeyKey(clientId, ProvideModeNetwork)).Result()
			connect.AssertEqual(t, []byte(networkSk), networkKey)
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
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, foundNetworkId, networkId)

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
		connect.AssertEqual(t, err, nil)
		_, err = FindActiveClientNetwork(ctx, clientId)
		connect.AssertNotEqual(t, err, nil)

		// deleted client
		_, err = FindActiveClientNetwork(ctx, server.NewId())
		connect.AssertNotEqual(t, err, nil)
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
		connect.AssertEqual(t, err, nil)
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
		connect.AssertEqual(t, exists, true)
		connect.AssertEqual(t, active, false)
		connect.AssertNotEqual(t, deactivateTime, nil)

		// marking makes the refresh lookup fail (the app logs out on this)
		_, err = FindActiveClientNetwork(ctx, idleClientId)
		connect.AssertNotEqual(t, err, nil)

		_, active, _ = clientState(connectedClientId)
		connect.AssertEqual(t, active, true)
		_, active, _ = clientState(freshClientId)
		connect.AssertEqual(t, active, true)

		// the idle child client was reaped by the child pass (auth_time based),
		// not marked inactive
		exists, _, _ = clientState(childClientId)
		connect.AssertEqual(t, exists, false)

		// within the grace window the marked client is retained
		exists, _, _ = clientState(idleClientId)
		connect.AssertEqual(t, exists, true)

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
		connect.AssertEqual(t, exists, false)

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
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, removeResult.Error, nil)
		exists, active, deactivateTime = clientState(removedClientId)
		connect.AssertEqual(t, exists, true)
		connect.AssertEqual(t, active, false)
		connect.AssertNotEqual(t, deactivateTime, nil)
	})
}

// `ConnectNetworkClient` refreshes `network_client.auth_time` at most once per
// `clientAuthTimeRefreshMinInterval`: auth_time keys the reap partial indexes,
// so every refresh is a non-HOT update that maintains all of the table's
// indexes, and its consumers are 30d/90d retention thresholds that do not
// need sub-hour freshness.
func TestConnectNetworkClientAuthTimeThrottle(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		clientId := server.NewId()
		Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "test", "test")

		authTime := func() time.Time {
			var authTime time.Time
			server.Db(ctx, func(conn server.PgConn) {
				result, err := conn.Query(
					ctx,
					`SELECT auth_time FROM network_client WHERE client_id = $1`,
					clientId,
				)
				server.WithPgResult(result, err, func() {
					connect.AssertEqual(t, result.Next(), true)
					server.Raise(result.Scan(&authTime))
				})
			})
			return authTime
		}
		setAuthTime := func(authTime time.Time) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`UPDATE network_client SET auth_time = $2 WHERE client_id = $1`,
					clientId,
					authTime,
				))
			})
		}

		initialAuthTime := authTime()

		// a fresh auth_time is not refreshed on connect, and the throttled
		// connect still succeeds
		_, _, _, _, err := ConnectNetworkClient(ctx, clientId, "10.0.0.1:20000", server.NewId())
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, authTime().Equal(initialAuthTime), true)

		// a stale auth_time (older than `clientAuthTimeRefreshMinInterval`) is
		// refreshed to ~now on connect
		staleAuthTime := server.NowUtc().Add(-2 * clientAuthTimeRefreshMinInterval)
		setAuthTime(staleAuthTime)
		_, _, _, _, err = ConnectNetworkClient(ctx, clientId, "10.0.0.1:20001", server.NewId())
		connect.AssertEqual(t, err, nil)
		refreshedAuthTime := authTime()
		connect.AssertEqual(t, staleAuthTime.Before(refreshedAuthTime), true)
		age := server.NowUtc().Sub(refreshedAuthTime)
		connect.AssertEqual(t, 0 <= age && age < time.Minute, true)
	})
}

// the child reap's stale-auth_time band must not accumulate long-connected
// children: `RemoveDisconnectedNetworkClients` bumps a connected child's
// auth_time to now (removing it from the band for another
// `NetworkClientReapAfterDeactivate`) instead of LEFT-JOIN probing it on
// every run, while stale children without a connection are still reaped and
// fresh children are untouched.
func TestRemoveDisconnectedChildReapBumpsConnected(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()

		newClient := func(sourceClientId *server.Id) server.Id {
			clientId := server.NewId()
			Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "test", "test")
			if sourceClientId != nil {
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(
						ctx,
						`UPDATE network_client SET source_client_id = $2 WHERE client_id = $1`,
						clientId,
						sourceClientId,
					))
				})
			}
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
		clientAuthTime := func(clientId server.Id) (exists bool, authTime time.Time) {
			server.Db(ctx, func(conn server.PgConn) {
				result, err := conn.Query(
					ctx,
					`SELECT auth_time FROM network_client WHERE client_id = $1`,
					clientId,
				)
				server.WithPgResult(result, err, func() {
					if result.Next() {
						exists = true
						server.Raise(result.Scan(&authTime))
					}
				})
			})
			return
		}

		parentClientId := newClient(nil)

		now := server.NowUtc()
		staleAuthTime := now.Add(-NetworkClientReapAfterDeactivate - 24*time.Hour)

		// stale child with a live connection: bumped out of the band, not reaped
		connectedChildId := newClient(&parentClientId)
		_, _, _, _, err := ConnectNetworkClient(ctx, connectedChildId, "10.0.0.2:20000", server.NewId())
		connect.AssertEqual(t, err, nil)
		setAuthTime(connectedChildId, staleAuthTime)

		// stale child without a connection: reaped
		staleChildId := newClient(&parentClientId)
		setAuthTime(staleChildId, staleAuthTime)

		// fresh child without a connection: untouched
		freshChildId := newClient(&parentClientId)
		_, freshAuthTimeBefore := clientAuthTime(freshChildId)

		minConnectionTime := now.Add(-8 * time.Hour)
		minClientTime := now.Add(-NetworkClientReapAfterDeactivate)
		minTopLevelAuthTime := now.Add(-TopLevelClientIdleExpiration)
		RemoveDisconnectedNetworkClients(ctx, minConnectionTime, minClientTime, minTopLevelAuthTime)

		// the connected child survives with auth_time bumped to ~now
		exists, bumpedAuthTime := clientAuthTime(connectedChildId)
		connect.AssertEqual(t, exists, true)
		connect.AssertEqual(t, minClientTime.Before(bumpedAuthTime), true)
		age := server.NowUtc().Sub(bumpedAuthTime)
		connect.AssertEqual(t, 0 <= age && age < time.Minute, true)

		// the stale disconnected child is reaped
		exists, _ = clientAuthTime(staleChildId)
		connect.AssertEqual(t, exists, false)

		// the fresh child is untouched
		exists, freshAuthTimeAfter := clientAuthTime(freshChildId)
		connect.AssertEqual(t, exists, true)
		connect.AssertEqual(t, freshAuthTimeAfter.Equal(freshAuthTimeBefore), true)

		// a second run is stable: the bumped child is out of the band and stays
		RemoveDisconnectedNetworkClients(ctx, minConnectionTime, minClientTime, minTopLevelAuthTime)
		exists, _ = clientAuthTime(connectedChildId)
		connect.AssertEqual(t, exists, true)
	})
}
