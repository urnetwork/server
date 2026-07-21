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

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/sdk/v2026"
	"github.com/urnetwork/server/v2026"
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

// A hosted (cloud proxy) device must block every hosted-incompatible parameter
// pushed over the device rpc: provide (in any form), local routing, tunnel
// stop/start, vpn-interface-while-offline, and direct mode in the performance
// profile. Direct mode would leak that the proxy client is hosted, and where it
// is hosted, via the host addresses in the direct connection setup; the others
// would egress or provide from the proxy host's real interface.
//
// Each blocked parameter is pushed from a DeviceRemote (as the browser would)
// with a value that differs from the hosted baseline. The performance profile —
// an allowed rpc whose AllowDirect field is hard-limited — is pushed last and
// used as the sync barrier. The hosted device's stored state must be unchanged
// for every blocked parameter, and the stored profile must have direct mode
// forced off with the rest of the profile preserved.
//
// Complements TestProxyDeviceRpcLocalRouteOverrideNeutralized (block action
// overrides) and the sdk-level hosted guard unit tests
// (TestHostedSafePerformanceProfile, TestHostedSafeBlockActionOverride) and the
// multi client hard limit (connect TestMultiClientNeverAllowDirect).
func TestProxyDeviceRpcHostedIncompatibleBlocked(t *testing.T) {
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

		baselineTunnelStarted := hosted.GetTunnelStarted()
		baselineRouteLocal := hosted.GetRouteLocal()
		baselineProvideMode := hosted.GetProvideMode()
		baselineProvidePaused := hosted.GetProvidePaused()
		baselineProvideControlMode := hosted.GetProvideControlMode()
		baselineProvideNetworkMode := hosted.GetProvideNetworkMode()
		baselineVpnInterfaceWhileOffline := hosted.GetVpnInterfaceWhileOffline()

		// each pushed value must differ from the baseline,
		// so an accepted push would be visible
		connect.AssertEqual(t, false, baselineRouteLocal)
		connect.AssertEqual(t, true, baselineProvideMode != sdk.ProvideModePublic)
		connect.AssertEqual(t, true, baselineProvideControlMode != sdk.ProvideControlModeAlways)
		connect.AssertEqual(t, true, baselineProvideNetworkMode != sdk.ProvideNetworkModeAll)

		// push hosted-incompatible parameters
		remote.SetTunnelStarted(!baselineTunnelStarted)
		remote.SetRouteLocal(true)
		remote.SetProvideMode(sdk.ProvideModePublic)
		remote.SetProvidePaused(!baselineProvidePaused)
		remote.SetProvideControlMode(sdk.ProvideControlModeAlways)
		remote.SetProvideNetworkMode(sdk.ProvideNetworkModeAll)
		remote.SetVpnInterfaceWhileOffline(!baselineVpnInterfaceWhileOffline)

		// the performance profile is an allowed rpc; direct mode is hard-limited
		remote.SetPerformanceProfile(&sdk.PerformanceProfile{
			WindowType:  sdk.WindowTypeQuality,
			AllowDirect: true,
		})
		waitFor(t, 30*time.Second, "performance profile synced to hosted device", func() bool {
			return hosted.GetPerformanceProfile() != nil
		})

		// direct mode is forced off; the rest of the profile is preserved
		performanceProfile := hosted.GetPerformanceProfile()
		if performanceProfile.AllowDirect {
			t.Fatal("hosted device holds AllowDirect: direct mode must be impossible")
		}
		connect.AssertEqual(t, sdk.WindowTypeQuality, performanceProfile.WindowType)

		// every blocked parameter is unchanged on the hosted device
		connect.AssertEqual(t, baselineTunnelStarted, hosted.GetTunnelStarted())
		connect.AssertEqual(t, baselineRouteLocal, hosted.GetRouteLocal())
		connect.AssertEqual(t, baselineProvideMode, hosted.GetProvideMode())
		connect.AssertEqual(t, baselineProvidePaused, hosted.GetProvidePaused())
		connect.AssertEqual(t, baselineProvideControlMode, hosted.GetProvideControlMode())
		connect.AssertEqual(t, baselineProvideNetworkMode, hosted.GetProvideNetworkMode())
		connect.AssertEqual(t, baselineVpnInterfaceWhileOffline, hosted.GetVpnInterfaceWhileOffline())
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
