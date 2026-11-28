// A client in post-quantum mode connects to its own devices
// (readFindProviders2NamedModern; FP2-MODERN.md "Connect multi client").
package model

import (
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

// A device that provides only to its network is never measured, since no
// probe can reach it, so it is never modern. A require_modern_provider
// request still answers the caller's own such device by name, and keeps
// refusing another network's: the caller's window client requires a sealed
// session with its device and sends it nothing in plaintext, so the platform
// only stops hiding it. The same holds named alone and beside a discovery
// spec, in legacy and in selector mode, before and after the post-quantum
// marker. A request that does not require modern providers answers both, and
// a caller without a jwt has no own network.
func TestRequireModernProviderAnswersTheCallersOwnDevices(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		t.Cleanup(clientScorePostQuantumReadyState.reset)
		ctx := t.Context()
		city := egressTestCity(ctx, "Own Device City", "Own Device Region", "United States", "us")
		callerNetworkId := server.NewId()
		ownDevice := egressTestConnectOn(ctx, t, callerNetworkId, city, egressTestFast, modernTestNetworkOnlyKeys, nil).clientId
		otherDevice := egressTestConnect(ctx, t, city, egressTestFast, modernTestNetworkOnlyKeys, nil).clientId
		egressTestPasses(ctx, t)

		clientSession := testingCreateProviderSearchSession(ctx, session.NewByJwt(callerNetworkId, server.NewId(), "own-devices", false, false))
		anonymousSession := testingCreateProviderSearchSession(ctx, nil)
		for _, marked := range []bool{false, true} {
			if marked {
				// an export that published the named sets, no device a member,
				// and the marker that makes the requests read them
				if err := publishProviderCapabilitySets(ctx, map[server.Id]*clientScoreCapability{}, map[server.Id]uint8{}, time.Hour); err != nil {
					t.Fatal(err)
				}
				server.Redis(ctx, func(r server.RedisClient) {
					server.Raise(r.Set(ctx, ClientScorePostQuantumReadyKey, "1", time.Hour).Err())
				})
				clientScorePostQuantumReadyState.reset()
			}
			for _, providerSelect := range []*FindProviders2Select{nil, capabilityTestSelect("ipv4", "any", 1, 0, 0, 0)} {
				for _, clientId := range []server.Id{ownDevice, otherDevice} {
					for _, specs := range [][]*ProviderSpec{
						{{ClientId: &clientId}},
						{{ClientId: &clientId}, {LocationId: &city.LocationId}},
					} {
						result, err := FindProviders2(&FindProviders2Args{Specs: specs, RankMode: RankModeSpeed, RequireModernProvider: true, Select: providerSelect}, clientSession)
						connect.AssertEqual(t, err, nil)
						if answered := modernTestAnswered(result.Providers, clientId); answered != (clientId == ownDevice) {
							t.Errorf("marked %t selector %t specs %d: own device %t answered %t", marked, providerSelect != nil, len(specs), clientId == ownDevice, answered)
						}
					}
					result, err := FindProviders2(&FindProviders2Args{Specs: []*ProviderSpec{{ClientId: &clientId}}, RankMode: RankModeSpeed, Select: providerSelect}, clientSession)
					connect.AssertEqual(t, err, nil)
					if !modernTestAnswered(result.Providers, clientId) {
						t.Errorf("marked %t selector %t: own device %t not answered without the requirement", marked, providerSelect != nil, clientId == ownDevice)
					}
				}
				result, err := FindProviders2(&FindProviders2Args{Specs: []*ProviderSpec{{ClientId: &ownDevice}}, RankMode: RankModeSpeed, RequireModernProvider: true, Select: providerSelect}, anonymousSession)
				connect.AssertEqual(t, err, nil)
				if modernTestAnswered(result.Providers, ownDevice) {
					t.Errorf("marked %t selector %t: a caller without a jwt was answered a network's device", marked, providerSelect != nil)
				}
			}
		}
	})
}
