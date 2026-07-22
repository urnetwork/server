package proxy

// Safety test for the hosted device rpc: the mutations that must never take
// effect on a hosted proxy device (route local, provide settings, tunnel/vpn)
// must be no-ops when driven over the device rpc. A DeviceRemote issues each
// setter; the hosted DeviceLocalRpc drops it (DisableHostedIncompatible), so the
// hosted DeviceLocal's state is unchanged. An allowed setter (SetOffline) is the
// control: it must apply, proving the rpc channel actually delivers sets — so
// the no-ops are the guard at work, not a dead channel.
//
// The DeviceRemote setters are synchronous rpc calls, so once a setter returns
// the hosted side has already processed (and dropped) it; the hosted state is
// then read directly for an authoritative assertion.
//
// Requires the standard local test environment plus outbound internet, like the
// rest of this package. Skipped under -short.

import (
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/sdk/v2026"
	"github.com/urnetwork/server/v2026"
)

// assertRpcNoop drives a hosted-incompatible setter from the remote with a value
// different from the hosted device's current one (alt), then asserts the hosted
// device did not change.
func assertRpcNoop[T comparable](t testing.TB, name string, get func() T, set func(T), alt func(T) T) {
	before := get()
	set(alt(before))
	if got := get(); got != before {
		t.Fatalf("%s mutated the hosted device: %v -> %v (must no-op on a hosted device)", name, before, got)
	}
}

func TestProxyDeviceRpcHostedIncompatibleNoop(t *testing.T) {
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

		// control: an allowed setter DOES apply, proving the rpc channel delivers
		// sets. Toggle relative to the current state so it is a real change.
		offlineBefore := hosted.GetOffline()
		remote.SetOffline(!offlineBefore)
		waitFor(t, 30*time.Second, "allowed setter (SetOffline) applied on hosted device", func() bool {
			return hosted.GetOffline() == !offlineBefore
		})

		// every hosted-incompatible setter, driven from the remote, must leave the
		// hosted device unchanged
		assertRpcNoop(t, "SetRouteLocal",
			hosted.GetRouteLocal, remote.SetRouteLocal,
			func(b bool) bool { return !b })
		assertRpcNoop(t, "SetProvidePaused",
			hosted.GetProvidePaused, remote.SetProvidePaused,
			func(b bool) bool { return !b })
		assertRpcNoop(t, "SetTunnelStarted",
			hosted.GetTunnelStarted, remote.SetTunnelStarted,
			func(b bool) bool { return !b })
		assertRpcNoop(t, "SetVpnInterfaceWhileOffline",
			hosted.GetVpnInterfaceWhileOffline, remote.SetVpnInterfaceWhileOffline,
			func(b bool) bool { return !b })
		assertRpcNoop(t, "SetProvideMode",
			hosted.GetProvideMode, remote.SetProvideMode,
			func(m sdk.ProvideMode) sdk.ProvideMode {
				if m == sdk.ProvideModePublic {
					return sdk.ProvideModeNone
				}
				return sdk.ProvideModePublic
			})
		assertRpcNoop(t, "SetProvideControlMode",
			hosted.GetProvideControlMode, remote.SetProvideControlMode,
			func(m sdk.ProvideControlMode) sdk.ProvideControlMode {
				if m == sdk.ProvideControlModeAuto {
					return sdk.ProvideControlModeNever
				}
				return sdk.ProvideControlModeAuto
			})
		assertRpcNoop(t, "SetProvideNetworkMode",
			hosted.GetProvideNetworkMode, remote.SetProvideNetworkMode,
			func(m sdk.ProvideNetworkMode) sdk.ProvideNetworkMode {
				if m == sdk.ProvideNetworkModeAll {
					return sdk.ProvideNetworkModeWiFi
				}
				return sdk.ProvideNetworkModeAll
			})
	})
}
