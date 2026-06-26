package model

import (
	"context"
	"encoding/json"
	mathrand "math/rand"
	"slices"
	"strings"
	"testing"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/glog/v2026"
	"github.com/urnetwork/server/v2026"
)

func TestSignProxyId(t *testing.T) {

	proxyId := server.NewId()

	signedProxyId := SignProxyId(proxyId)

	// ensure the signed proxy id can be used as a hostname part
	assert.Equal(t, len(signedProxyId) < 63, true)

	proxyId2, err := ParseSignedProxyId(signedProxyId)
	assert.Equal(t, err, nil)
	assert.Equal(t, proxyId, proxyId2)

	// try fuzzed values and make sure they don't parse
	for range 32 {
		b := []byte(signedProxyId)
		var i int
		var j int
		for {
			i = mathrand.Intn(16)
			j = (i + 1 + mathrand.Intn(15)) % 16
			if b[i] != b[j] {
				break
			}
		}
		assert.NotEqual(t, b[i], b[j])
		b[i], b[j] = b[j], b[i]

		_, err := ParseSignedProxyId(string(b))
		assert.NotEqual(t, err, nil)
	}

}

func TestSignProxyIdHosts(t *testing.T) {

	host := "06ds11j8v14jm3kuoig10h95j8tt7gdefb33jnap47jbiq1paapoheo8e8.connect.bringyour.com"
	hostProxyId := strings.SplitN(host, ".", 2)[0]
	_, err := ParseSignedProxyId(hostProxyId)
	assert.Equal(t, err, nil)

}

func TestCreateProxyClient(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		ResetProxyClientIpv4(ctx)

		// create n proxy clients
		n := 1024
		for i := range n {
			proxyDeviceConfig := &ProxyDeviceConfig{}
			proxyDeviceConfig.ClientId = server.NewId()
			err := CreateProxyDeviceConfig(ctx, proxyDeviceConfig)
			assert.Equal(t, err, nil)

			proxyClient, err := CreateProxyClient(
				ctx,
				proxyDeviceConfig.ProxyId,
				proxyDeviceConfig.ClientId,
				proxyDeviceConfig.InstanceId,
				CreateProxyClientOptions{
					EnableWg: true,
				},
			)
			assert.Equal(t, err, nil)
			assert.NotEqual(t, proxyClient, nil)

			// the client config must keep an idle client sending so it detects a
			// dead session (e.g. proxy instance restart) and re-handshakes
			assert.Equal(t, strings.Contains(proxyClient.WgConfig.Config, "PersistentKeepalive = 25"), true)

			glog.Infof("[ncpm][%d/%d]ip=%s\n", i+1, n, proxyClient.WgConfig.ClientIpv4)
		}
	})
}

// GetProxyDeviceConfig must serve from redis when warm and fall back to postgres
// when cold; an explicit remove must clear both stores.
func TestProxyDeviceConfigCacheAndFallback(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		proxyDeviceConfig := &ProxyDeviceConfig{}
		proxyDeviceConfig.ClientId = server.NewId()
		err := CreateProxyDeviceConfig(ctx, proxyDeviceConfig)
		assert.Equal(t, err, nil)
		proxyId := proxyDeviceConfig.ProxyId

		// cache hit
		got := GetProxyDeviceConfig(ctx, proxyId)
		assert.Equal(t, got != nil, true)
		assert.Equal(t, got.ClientId, proxyDeviceConfig.ClientId)

		// cache cold -> db fallback
		server.Redis(ctx, func(r server.RedisClient) {
			r.Del(ctx, proxyDeviceConfigKey(proxyId))
		})
		got = GetProxyDeviceConfig(ctx, proxyId)
		assert.Equal(t, got != nil, true)
		assert.Equal(t, got.ClientId, proxyDeviceConfig.ClientId)

		// explicit remove clears both stores
		RemoveProxyDeviceConfig(ctx, proxyId)
		assert.Equal(t, GetProxyDeviceConfig(ctx, proxyId) == nil, true)
		server.Redis(ctx, func(r server.RedisClient) {
			v, _ := r.Get(ctx, proxyDeviceConfigKey(proxyId)).Result()
			assert.Equal(t, v, "")
		})
	})
}

// The maintenance cascade reaps proxy_device_config rows whose network_client is
// gone; it must clear the (no-ttl) redis entry too, otherwise GetProxyDeviceConfig
// keeps serving the stale config forever.
func TestRemoveDisconnectedClearsProxyConfigRedis(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		// a client_id with no backing network_client row
		proxyDeviceConfig := &ProxyDeviceConfig{}
		proxyDeviceConfig.ClientId = server.NewId()
		err := CreateProxyDeviceConfig(ctx, proxyDeviceConfig)
		assert.Equal(t, err, nil)
		proxyId := proxyDeviceConfig.ProxyId

		RemoveDisconnectedNetworkClients(ctx, server.NowUtc(), server.NowUtc())

		assert.Equal(t, GetProxyDeviceConfig(ctx, proxyId) == nil, true)
		server.Redis(ctx, func(r server.RedisClient) {
			v, _ := r.Get(ctx, proxyDeviceConfigKey(proxyId)).Result()
			assert.Equal(t, v, "")
		})
	})
}

// The reap cascade must remove proxy_client rows (and their change rows) whose
// proxy_device_config is gone, while keeping clients with a live network_client,
// so that the wg peer restore at instance startup stays bounded by the live set.
func TestRemoveDisconnectedReapsProxyClients(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		newProxyClient := func(withNetworkClient bool) *ProxyClient {
			clientId := server.NewId()
			if withNetworkClient {
				Testing_CreateDevice(ctx, server.NewId(), server.NewId(), clientId, "test", "test")
			}

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
			return proxyClient
		}

		liveProxyClient := newProxyClient(true)
		// no network_client row: the device config is reaped, and then the
		// proxy_client row must be reaped too
		staleProxyClient := newProxyClient(false)

		host := liveProxyClient.ProxyHost
		block := liveProxyClient.Block
		assert.Equal(t, host, staleProxyClient.ProxyHost)
		assert.Equal(t, block, staleProxyClient.Block)

		// before the reap the startup sync sees both
		proxyClients, _, err := GetProxyClientsSince(ctx, host, block, 0)
		assert.Equal(t, err, nil)
		_, ok := proxyClients[liveProxyClient.ProxyId]
		assert.Equal(t, ok, true)
		_, ok = proxyClients[staleProxyClient.ProxyId]
		assert.Equal(t, ok, true)

		RemoveDisconnectedNetworkClients(ctx, server.NowUtc(), server.NowUtc())

		// after the reap the live client remains and the stale client is gone
		proxyClients, _, err = GetProxyClientsSince(ctx, host, block, 0)
		assert.Equal(t, err, nil)
		_, ok = proxyClients[liveProxyClient.ProxyId]
		assert.Equal(t, ok, true)
		_, ok = proxyClients[staleProxyClient.ProxyId]
		assert.Equal(t, ok, false)

		got, err := GetProxyClient(ctx, staleProxyClient.ProxyId)
		assert.Equal(t, err, nil)
		assert.Equal(t, got == nil, true)

		// the stale client's change rows are pruned too
		proxyIds, _ := GetProxyIdsSince(ctx, host, block, 0)
		assert.Equal(t, slices.Contains(proxyIds, liveProxyClient.ProxyId), true)
		assert.Equal(t, slices.Contains(proxyIds, staleProxyClient.ProxyId), false)
	})
}

// MigrateProxyDeviceConfig backfills redis for proxies whose proxy_device_config
// rows predate the redis layer. CreateProxyDeviceConfig writes both stores;
// dropping the redis key simulates the pre-redis state, then the migration must
// rebuild it from the db row.
func TestMigrateProxyDeviceConfig(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		proxyDeviceConfig := &ProxyDeviceConfig{}
		proxyDeviceConfig.ClientId = server.NewId()
		err := CreateProxyDeviceConfig(ctx, proxyDeviceConfig)
		assert.Equal(t, err, nil)
		proxyId := proxyDeviceConfig.ProxyId

		// drop the redis key, leaving only the db row for the migration to read
		server.Redis(ctx, func(r server.RedisClient) {
			r.Del(ctx, proxyDeviceConfigKey(proxyId))
		})

		MigrateProxyDeviceConfig(ctx, 50000)

		server.Redis(ctx, func(r server.RedisClient) {
			configJson, err := r.Get(ctx, proxyDeviceConfigKey(proxyId)).Result()
			assert.Equal(t, err, nil)
			assert.NotEqual(t, configJson, "")

			var got ProxyDeviceConfig
			err = json.Unmarshal([]byte(configJson), &got)
			assert.Equal(t, err, nil)
			assert.Equal(t, got.ClientId, proxyDeviceConfig.ClientId)
			assert.Equal(t, got.ProxyId, proxyId)
		})
	})
}
