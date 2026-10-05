package controller

import (
	"bytes"
	"context"
	"net/netip"
	"testing"

	"github.com/urnetwork/sdk"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// A freshly allocated WireGuard egress enters the same keyed namespace the
// /verify reader uses. This catches an unkeyed model-default feed before the
// periodic refresh can turn one real address into two reverse-index entries.
func TestAuthNetworkClientFeedsConfiguredProxyEgressNamespace(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientID := server.NewId()
		ip := netip.MustParseAddr("2001:db8:31::1")
		settings := model.DefaultVerifySettings()
		settings.EgressHashKey = bytes.Repeat([]byte{0x31}, 32)
		result := &model.AuthNetworkClientResult{
			ClientId: &clientID,
			ProxyConfigResult: &model.ProxyConfigResult{
				ProxyClient: model.ProxyClient{
					WgConfig: &model.WgConfig{ClientIpv4: ip},
				},
			},
		}

		feedAuthNetworkClientVerifyEgress(ctx, result, settings)
		defer model.RemoveVerifyEgressForClient(ctx, clientID)
		got := model.ResolveVerifyEgress(ctx, ip, settings)
		if got == nil || *got != clientID {
			t.Fatalf("configured proxy namespace resolved %v, want %s", got, clientID)
		}
		if wrong := model.ResolveVerifyEgress(ctx, ip, model.DefaultVerifySettings()); wrong != nil {
			t.Fatalf("unkeyed namespace attributed configured proxy %s", *wrong)
		}
	})
}

// The verify egress feed runs after auth-client committed the client and its
// wireguard proxy. A failed feed is left to the RefreshVerifyProxyEgress task,
// and the call still returns the client. The feed used to panic out of the
// call, which answered a 500 and handed out no credentials for a proxy that
// stayed allocated. Here the feed's redis writes fail because the caller has
// gone away, and the refresh then feeds the allocation.
func TestAuthNetworkClientVerifyEgressFeedFailureKeepsClient(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "test", userId)
		userSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			UserId:    userId,
		})

		// free wireguard client addresses at the top of the sequence, so that
		// every random start of the allocation finds one
		const ipv4Count = 8
		firstIpv4 := model.Ipv4ToInt(netip.MustParseAddr("198.51.100.1"))
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				INSERT INTO proxy_client_ipv4 (sequence_id, client_ipv4)
				SELECT $1::bigint + i, $2::bigint + i
				FROM generate_series(0, $3::integer - 1) AS fixture(i)
				`,
				model.ProxyClientIpv4Count-ipv4Count,
				firstIpv4,
				ipv4Count,
			))
		})

		result, err := model.AuthNetworkClient(
			&model.AuthNetworkClientArgs{
				Description: "proxy",
				DeviceSpec:  "proxy",
				ProxyConfig: &model.ProxyConfig{
					EnableWg: true,
					InitialDeviceState: &model.ExtendedProxyDeviceState{
						ProxyDeviceState: model.ProxyDeviceState{
							Location: &sdk.ConnectLocation{
								ConnectLocationId: &sdk.ConnectLocationId{BestAvailable: true},
							},
						},
					},
				},
			},
			userSession,
		)
		if err != nil || result.Error != nil || result.ClientId == nil || result.ProxyConfigResult == nil || result.ProxyConfigResult.WgConfig == nil {
			t.Fatalf("could not create a wireguard proxy: %v %+v", err, result)
		}
		clientId := *result.ClientId
		ip := result.ProxyConfigResult.WgConfig.ClientIpv4
		defer model.RemoveVerifyEgressForClient(ctx, clientId)

		settings := model.DefaultVerifySettings()
		settings.EgressHashKey = bytes.Repeat([]byte{0x32}, 32)

		// the caller has gone away
		endedCtx, cancel := context.WithCancel(ctx)
		cancel()
		panicValue := func() (panicValue any) {
			defer func() {
				panicValue = recover()
			}()
			feedAuthNetworkClientVerifyEgress(endedCtx, result, settings)
			return
		}()
		if panicValue != nil {
			t.Fatalf("the failed feed failed the call: %v", panicValue)
		}
		if resolved := model.ResolveVerifyEgress(ctx, ip, settings); resolved != nil {
			t.Fatalf("the failed feed resolved %s", *resolved)
		}

		model.RefreshVerifyProxyEgress(ctx, settings)
		resolved := model.ResolveVerifyEgress(ctx, ip, settings)
		if resolved == nil || *resolved != clientId {
			t.Fatalf("the refresh resolved %v, want %s", resolved, clientId)
		}
	})
}
