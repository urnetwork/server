package proxy

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sdk"

	"github.com/urnetwork/server/model"
)

// The hosted window policy (owner, 2026-11-13: "hosted proxies should use
// main window as the limited window, so that dualproto and dualstack
// requirements are not applied on the main window for hosted proxies, and
// there is no need for a separate limited window").

// Records each find-providers2 request a device's windows send and holds it
// until its context ends, as a platform that has not answered yet.
type hostedWindowSelectorsTestDiscovery struct {
	stateLock sync.Mutex
	requests  []connect.FindProviders2Args
	selects   []*connect.FindProvidersSelect
	asked     *connect.Monitor
}

func (self *hostedWindowSelectorsTestDiscovery) FindProviders2(ctx context.Context, _ string, args *connect.FindProviders2Args) (*connect.FindProviders2Result, error) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.requests = append(self.requests, *args)
		var providerSelect *connect.FindProvidersSelect
		if args.Select != nil {
			copied := *args.Select
			providerSelect = &copied
		}
		self.selects = append(self.selects, providerSelect)
		self.asked.NotifyAll()
	}()
	<-ctx.Done()
	return nil, ctx.Err()
}

func (self *hostedWindowSelectorsTestDiscovery) recorded() ([]connect.FindProviders2Args, []*connect.FindProvidersSelect) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]connect.FindProviders2Args{}, self.requests...), append([]*connect.FindProvidersSelect{}, self.selects...)
}

// Every hosted device's windows ask for ipv4 and any, and it has no limited
// window.
func TestHostedDevicesAskEveryProviderWithoutALimitedWindow(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	proxyDeviceSettings := DefaultProxyDeviceSettings()
	proxyDeviceSettings.DisableWindowIdentityPersistence = true
	deviceLocalSettings := newProxyDeviceLocalSettings(ctx, &model.ProxyDeviceConfig{}, proxyDeviceSettings)
	if deviceLocalSettings.NormalWindowSelectors == nil || *deviceLocalSettings.NormalWindowSelectors != (connect.ProviderSelectors{
		Ip:       connect.ProviderIpCapabilityIpv4,
		Protocol: connect.ProviderProtocolCapabilityAny,
	}) {
		t.Fatalf("hosted normal windows ask %v, want ipv4 and any", deviceLocalSettings.NormalWindowSelectors)
	}
	if !deviceLocalSettings.DisableLimitedWindow {
		t.Fatal("a hosted device keeps a limited window")
	}
	// the policy is the proxy's: the low-memory profile it starts from, which
	// iOS shares, keeps connect's windows
	lowMemory := sdk.DefaultLowMemoryDeviceLocalSettings()
	if lowMemory.NormalWindowSelectors != nil || lowMemory.DisableLimitedWindow {
		t.Fatal("the shared low-memory profile carries the hosted window policy")
	}
}

// A hosted device built as the proxy builds it asks find-providers2 from each
// normal window with ipv4 and any and no ipv6 minimum, and reads a limited
// window fraction of 0 that the developer override's set and reset do not
// change, so no limited window fills.
func TestHostedDeviceWindowsAskWithIpv4AndAny(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	discovery := &hostedWindowSelectorsTestDiscovery{asked: connect.NewMonitor()}
	proxyDeviceSettings := DefaultProxyDeviceSettings()
	proxyDeviceSettings.DisableWindowIdentityPersistence = true
	deviceLocalSettings := newProxyDeviceLocalSettings(ctx, &model.ProxyDeviceConfig{}, proxyDeviceSettings)
	deviceLocalSettings.ProviderDiscovery = discovery
	networkSpace := sdk.NewNetworkSpaceWithUrls(
		ctx,
		"http://127.0.0.1:1",
		"ws://127.0.0.1:1",
		connect.DefaultClientStrategySettings(),
	)
	defer networkSpace.Close()
	deviceLocal, err := sdk.NewPlatformDeviceLocal(
		nil,
		networkSpace,
		proxyDeviceTransportTestJwt(connect.NewId()),
		"hosted window selectors test",
		"hosted window selectors test",
		"test",
		sdk.NewId(),
		deviceLocalSettings,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = deviceLocal.CloseAndWait(context.Background())
	}()

	if fraction := deviceLocal.GetLimitedWindowSizeFraction(); fraction != 0 {
		t.Fatalf("a disconnected hosted device reads a limited window fraction of %v", fraction)
	}
	deviceLocal.SetConnectLocation(&sdk.ConnectLocation{ConnectLocationId: &sdk.ConnectLocationId{BestAvailable: true}})
	deviceLocal.SetLimitedWindowSizeFraction(1)
	if fraction := deviceLocal.GetLimitedWindowSizeFraction(); fraction != 0 {
		t.Fatalf("the developer override set a hosted device's limited window fraction to %v", fraction)
	}
	deviceLocal.ResetLimitedWindowSizeFraction()
	if fraction := deviceLocal.GetLimitedWindowSizeFraction(); fraction != 0 {
		t.Fatalf("the developer override's reset gave a hosted device a limited window fraction of %v", fraction)
	}

	watchdog, stop := context.WithTimeout(ctx, 45*time.Second)
	defer stop()
	for {
		update := discovery.asked.NotifyChannel()
		requests, _ := discovery.recorded()
		rankModes := map[string]bool{}
		for _, request := range requests {
			rankModes[request.RankMode] = true
		}
		if rankModes["quality"] && rankModes["speed"] {
			break
		}
		select {
		case <-update:
		case <-watchdog.Done():
			t.Fatalf("the hosted windows asked with rank modes %v, want quality and speed", rankModes)
		}
	}
	requests, selects := discovery.recorded()
	for i, request := range requests {
		providerSelect := selects[i]
		if providerSelect == nil {
			t.Errorf("request %d (%s) asked in legacy mode", i, request.RankMode)
			continue
		}
		if providerSelect.Ip != connect.ProviderIpCapabilityIpv4 || providerSelect.Protocol != connect.ProviderProtocolCapabilityAny || providerSelect.DualstackTargetCount != 0 {
			t.Errorf("request %d (%s) asked %+v, want ipv4 and any with no ipv6 minimum", i, request.RankMode, *providerSelect)
		}
	}
	if windowStatus := deviceLocal.GetWindowStatus(); windowStatus == nil || windowStatus.LimitedWindowSize != 0 {
		t.Errorf("a connected hosted device reports window status %+v, want no limited clients", windowStatus)
	}
}
