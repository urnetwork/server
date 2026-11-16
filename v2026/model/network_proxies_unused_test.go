package model

import (
	"context"
	"encoding/json"
	"net/netip"
	"testing"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/sdk/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

// The proxies-list fixtures the Pro features notice tests share. Only APIs
// that predate the feature use marks, so the root cause test below builds
// against the code before them too.

func testProFeaturesNetwork(t testing.TB, ctx context.Context, name string) *session.ClientSession {
	networkId := server.NewId()
	userId := server.NewId()
	Testing_CreateNetwork(ctx, networkId, name, userId)
	return session.Testing_CreateClientSession(ctx, &session.ByJwt{
		NetworkId: networkId,
		UserId:    userId,
	})
}

// Creates a hosted proxy as the apps do, returning its client id and proxy id.
func testProFeaturesProxy(t testing.TB, userSession *session.ClientSession, description string, enableWg bool) (server.Id, server.Id) {
	result, err := AuthNetworkClient(
		&AuthNetworkClientArgs{
			Description: description,
			DeviceSpec:  "proxy",
			ProxyConfig: &ProxyConfig{
				EnableWg: enableWg,
				InitialDeviceState: &ExtendedProxyDeviceState{
					ProxyDeviceState: ProxyDeviceState{
						Location: &sdk.ConnectLocation{
							ConnectLocationId: &sdk.ConnectLocationId{BestAvailable: true},
						},
					},
				},
			},
		},
		userSession,
	)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, result.Error, nil)
	connect.AssertEqual(t, result.ProxyConfigResult != nil, true)
	return *result.ClientId, result.ProxyConfigResult.ProxyId
}

// WireGuard addresses for the proxies, as TestCreateProxyClient lays them out:
// a tail of the sequence every random allocation start reaches.
func testProFeaturesWgAddresses(t testing.TB, ctx context.Context) {
	const ipv4Count = 64
	firstSequence := ProxyClientIpv4Count - ipv4Count
	if firstSequence < (31*ProxyClientIpv4Count)/32 {
		t.Fatal("fixture must cover every allocation start")
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `DELETE FROM proxy_client_ipv4`))
		server.RaisePgResult(tx.Exec(ctx, `
			INSERT INTO proxy_client_ipv4 (sequence_id, client_ipv4)
			SELECT $1::bigint + i, $2::bigint + i
			FROM generate_series(0, $3::integer - 1) AS fixture(i)
		`, firstSequence, Ipv4ToInt(netip.MustParseAddr("10.255.0.1")), ipv4Count))
	})
}

// pro.yml's proxy plans as they ship: SOCKS and WireGuard in Pro and not in
// free, enforcement dark. Pro() parses pro.yml once per process, so a test
// that depends on them must not depend on which test parsed it first; this
// sets them on the parsed config until the test ends.
func testProFeaturesProOnlyPlans(t testing.TB) {
	c := Pro()
	free, pro, enforce := c.Free, c.Pro, c.EnforceFeatures
	c.Free.SocksProxy, c.Free.WireguardProxy = false, false
	c.Pro.SocksProxy, c.Pro.WireguardProxy = true, true
	c.EnforceFeatures = false
	t.Cleanup(func() {
		c.Free.SocksProxy, c.Free.WireguardProxy = free.SocksProxy, free.WireguardProxy
		c.Pro.SocksProxy, c.Pro.WireguardProxy = pro.SocksProxy, pro.WireguardProxy
		c.EnforceFeatures = enforce
	})
}

// The proxies list as JSON, decoded into the sdk's own types and read by the
// sdk's own rule (sdk pro_features_status.go): the notice exactly as the apps
// and ur.io decide it, and each proxy's pro_features by client id.
func testProFeaturesNotice(t testing.TB, userSession *session.ClientSession) (*sdk.ProFeaturesStatus, map[server.Id][]string) {
	result, err := GetNetworkProxies(userSession)
	connect.AssertEqual(t, err, nil)
	data, err := json.Marshal(result)
	connect.AssertEqual(t, err, nil)
	var list sdk.NetworkClientsResult
	connect.AssertEqual(t, json.Unmarshal(data, &list), nil)

	var wire struct {
		Clients []struct {
			ClientId    server.Id `json:"client_id"`
			ProFeatures []string  `json:"pro_features"`
		} `json:"clients"`
	}
	connect.AssertEqual(t, json.Unmarshal(data, &wire), nil)
	features := map[server.Id][]string{}
	for _, client := range wire.Clients {
		features[client.ClientId] = client.ProFeatures
	}
	return sdk.ProFeaturesStatusFor(&list), features
}

// Root cause (owner, 2026-11-11, "only affected networks"): a free network
// whose proxies hold SOCKS and WireGuard credentials but never signed in over
// SOCKS or connected over WireGuard loses nothing when enforcement turns on,
// and must not see the upgrade notice. Before the feature use marks, every
// proxy listed the Pro features it held credentials for, and every proxy
// issued while enforcement was dark holds a SOCKS url, so this network saw the
// notice (43,510 free networks did, about 1,500 of them HTTPS only).
func TestNetworkProxiesNeverUsedOverSocksOrWireguardShowNoNotice(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testProFeaturesWgAddresses(t, ctx)
		// enforcement dark, as pro.yml ships today
		testProFeaturesProOnlyPlans(t)
		connect.AssertEqual(t, Pro().EnforceFeatures, false)
		connect.AssertEqual(t, Pro().ProOnlyFeature(FeatureSocksProxy), true)
		connect.AssertEqual(t, Pro().ProOnlyFeature(FeatureWireguardProxy), true)

		freeSession := testProFeaturesNetwork(t, ctx, "profeaturesunused")
		httpsClientId, _ := testProFeaturesProxy(t, freeSession, "https proxy", false)
		wgClientId, _ := testProFeaturesProxy(t, freeSession, "wireguard proxy", true)

		status, features := testProFeaturesNotice(t, freeSession)
		connect.AssertEqual(t, status.PlanKnown, true)
		connect.AssertEqual(t, status.Pro, false)
		connect.AssertEqual(t, len(features[httpsClientId]), 0)
		connect.AssertEqual(t, len(features[wgClientId]), 0)
		connect.AssertEqual(t, status.SocksProxyCount, 0)
		connect.AssertEqual(t, status.WireguardProxyCount, 0)
		connect.AssertEqual(t, status.Prompt, sdk.ProFeaturesPromptNone)
	})
}
