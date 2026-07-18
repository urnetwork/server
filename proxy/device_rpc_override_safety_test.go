package proxy

// Safety test for hosted block-action overrides over the device rpc: a hosted
// (cloud proxy) device must never route traffic locally, so a RouteOverride with
// Local=true pushed from a DeviceRemote (as the browser would) is neutralized to
// remote routing on the hosted device — via both SetBlockActionOverrides and
// AddBlockActionOverride. A block override is preserved, proving the
// neutralization is surgical (local route only), not a blanket rejection.
//
// The hosted device's stored state is read directly for an authoritative
// assertion. This complements the sdk-level unit tests
// (TestConnectBlockActionOverridesHostedForcesRemote, TestHostedSafeBlockActionOverride)
// by exercising the full DeviceRemote -> device rpc -> hosted DeviceLocal path.
//
// Requires the standard local test environment (WARP_ENV=local + local
// postgres/redis/vault) plus outbound internet, like the rest of this package.
// Skipped under -short.

import (
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sdk"
	"github.com/urnetwork/server"
)

func TestProxyDeviceRpcLocalRouteOverrideNeutralized(t *testing.T) {
	if testing.Short() {
		return
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		opts := defaultProxyTestOptions()
		opts.enableDeviceRpc = true
		opts.disableSecurityPolicies = true
		h := setupProxyTestWithOptions(t, opts)
		defer h.cancel()

		pd, err := h.proxyDeviceManager.OpenProxyDevice(h.proxyId)
		connect.AssertEqual(t, err, nil)
		hosted := pd.deviceLocal

		instanceId := sdk.RequireIdFromBytes(h.pdInstanceId.Bytes())
		remote, err := sdk.NewPlatformDeviceRemote(
			h.networkSpace,
			h.pdByClientJwt,
			h.deviceRpcUrl,
			h.signedProxyId,
			instanceId,
		)
		connect.AssertEqual(t, err, nil)
		defer remote.Close()

		waitFor(t, 60*time.Second, "remote connected", remote.GetRemoteConnected)

		localHosts := sdk.NewStringList()
		localHosts.Add("example.com")
		blockHosts := sdk.NewStringList()
		blockHosts.Add("blocked.example.com")

		overrides := sdk.NewBlockActionOverrideList()
		overrides.Add(&sdk.BlockActionOverride{
			OverrideId:    sdk.NewId(),
			Hosts:         localHosts,
			RouteOverride: &sdk.RouteOverride{Local: true},
		})
		overrides.Add(&sdk.BlockActionOverride{
			OverrideId:    sdk.NewId(),
			Hosts:         blockHosts,
			BlockOverride: &sdk.BlockOverride{Block: true},
		})

		// SetBlockActionOverrides: the local route is neutralized on the hosted
		// device (recorded, but with Local=false).
		remote.SetBlockActionOverrides(overrides)
		waitFor(t, 30*time.Second, "overrides synced to hosted device", func() bool {
			return hosted.GetBlockActionOverrides().Len() == 2
		})
		assertNoLocalRoute(t, hosted.GetBlockActionOverrides())

		// the block override survived — neutralization is surgical, not a blanket
		// rejection of all overrides.
		if !hasBlockOverride(hosted.GetBlockActionOverrides()) {
			t.Fatal("hosted device dropped the block override; only the local route must be neutralized")
		}

		// AddBlockActionOverride: same neutralization on the add path.
		addHosts := sdk.NewStringList()
		addHosts.Add("added.example.com")
		remote.AddBlockActionOverride(&sdk.BlockActionOverride{
			OverrideId:    sdk.NewId(),
			Hosts:         addHosts,
			RouteOverride: &sdk.RouteOverride{Local: true},
		})
		waitFor(t, 30*time.Second, "added override synced", func() bool {
			return hosted.GetBlockActionOverrides().Len() == 3
		})
		assertNoLocalRoute(t, hosted.GetBlockActionOverrides())
	})
}

// assertNoLocalRoute fails if any override on the hosted device routes locally.
func assertNoLocalRoute(t testing.TB, overrides *sdk.BlockActionOverrideList) {
	for i := 0; i < overrides.Len(); i += 1 {
		o := overrides.Get(i)
		if o.RouteOverride != nil && o.RouteOverride.Local {
			t.Fatalf("hosted device holds a local route override (id=%v): local egress must be impossible", o.OverrideId)
		}
	}
}

func hasBlockOverride(overrides *sdk.BlockActionOverrideList) bool {
	for i := 0; i < overrides.Len(); i += 1 {
		if o := overrides.Get(i); o.BlockOverride != nil && o.BlockOverride.Block {
			return true
		}
	}
	return false
}
