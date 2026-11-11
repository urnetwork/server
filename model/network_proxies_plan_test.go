package model

import (
	"context"
	"encoding/json"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sdk"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// The apps' "Pro features in use" notice reads the proxies list: whether the
// network holds Pro now, whether enforce_features is on, and which Pro-only
// features each proxy holds. These are the list's JSON keys, read as an app
// reads them: before this the list said none of it, and an app could tell
// neither a free network from a Pro one (the jwt claim is stale after an
// upgrade or a lapse) nor that enforcement had turned on.
func TestNetworkProxiesReportThePlanAndEachProxysProFeatures(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		newNetwork := func(name string) *session.ClientSession {
			networkId := server.NewId()
			userId := server.NewId()
			Testing_CreateNetwork(ctx, networkId, name, userId)
			return session.Testing_CreateClientSession(ctx, &session.ByJwt{
				NetworkId: networkId,
				UserId:    userId,
			})
		}
		createProxy := func(userSession *session.ClientSession, description string, enableWg bool) server.Id {
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
			return *result.ClientId
		}
		// the list as an app decodes it
		type wireProxy struct {
			ClientId    server.Id `json:"client_id"`
			ProFeatures []string  `json:"pro_features"`
		}
		type wirePlan struct {
			Pro                 *bool `json:"pro"`
			ProFeaturesEnforced *bool `json:"pro_features_enforced"`
		}
		type wireList struct {
			Clients []*wireProxy `json:"clients"`
			Plan    *wirePlan    `json:"plan"`
		}
		read := func(result *NetworkClientsResult) (*wireList, map[server.Id][]string) {
			data, err := json.Marshal(result)
			connect.AssertEqual(t, err, nil)
			var list wireList
			connect.AssertEqual(t, json.Unmarshal(data, &list), nil)
			features := map[server.Id][]string{}
			for _, proxy := range list.Clients {
				features[proxy.ClientId] = proxy.ProFeatures
			}
			return &list, features
		}
		proxies := func(userSession *session.ClientSession) (*wireList, map[server.Id][]string) {
			result, err := GetNetworkProxies(userSession)
			connect.AssertEqual(t, err, nil)
			return read(result)
		}
		assertPlan := func(list *wireList, pro bool, enforced bool) {
			if list.Plan == nil || list.Plan.Pro == nil || list.Plan.ProFeaturesEnforced == nil {
				t.Fatalf("the proxies list reports no plan: %+v", list.Plan)
			}
			connect.AssertEqual(t, *list.Plan.Pro, pro)
			connect.AssertEqual(t, *list.Plan.ProFeaturesEnforced, enforced)
		}

		// WireGuard addresses for the proxies, as TestCreateProxyClient lays
		// them out: a tail of the sequence every random allocation start reaches
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

		// enforcement dark, as pro.yml ships today
		connect.AssertEqual(t, Pro().EnforceFeatures, false)

		freeSession := newNetwork("profeaturesfree")
		httpsProxyId := createProxy(freeSession, "https proxy", false)
		wgProxyId := createProxy(freeSession, "wireguard proxy", true)

		list, features := proxies(freeSession)
		assertPlan(list, false, false)
		// every proxy issued while enforcement is dark holds a SOCKS url
		connect.AssertEqual(t, features[httpsProxyId], []string{FeatureSocksProxy})
		connect.AssertEqual(t, features[wgProxyId], []string{FeatureSocksProxy, FeatureWireguardProxy})

		// the devices list carries no plan
		devicesResult, err := GetNetworkClients(freeSession)
		connect.AssertEqual(t, err, nil)
		devices, _ := read(devicesResult)
		connect.AssertEqual(t, devices.Plan == nil, true)

		// Pro, read live: the same proxies on a network that now holds Pro
		now := server.NowUtc()
		server.Tx(ctx, func(tx server.PgTx) {
			err := AddProTransferBalanceInTx(
				tx, ctx, freeSession.ByJwt.NetworkId, ByteCount(10*1024*1024*1024*1024), now.Add(-time.Minute), now.Add(30*24*time.Hour),
			)
			connect.AssertEqual(t, err, nil)
		})
		connect.AssertEqual(t, UpdateProNetwork(ctx, freeSession.ByJwt.NetworkId), true)
		list, features = proxies(freeSession)
		assertPlan(list, true, false)
		connect.AssertEqual(t, features[wgProxyId], []string{FeatureSocksProxy, FeatureWireguardProxy})

		// enforcement on: the list says so, and a free network's new proxy is
		// issued neither, so it holds no Pro feature
		restore := Testing_SetEnforceFeatures(true)
		defer restore()
		enforcedSession := newNetwork("profeaturesenforced")
		enforcedProxyId := createProxy(enforcedSession, "enforced proxy", true)
		list, features = proxies(enforcedSession)
		assertPlan(list, false, true)
		connect.AssertEqual(t, len(list.Clients), 1)
		connect.AssertEqual(t, len(features[enforcedProxyId]), 0)
		// the proxies issued before it keep their credentials, so they still
		// say what enforcement now refuses them
		_, features = proxies(freeSession)
		connect.AssertEqual(t, slices.Equal(features[httpsProxyId], []string{FeatureSocksProxy}), true)
	})
}
