// Exercises the actual hosted SDK constructor settings and ProxyDevice.Run
// reader. Only provider enumeration is empty; no TUN or send is substituted.
package proxy

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/sdk/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// The real multi-client has no available destination and never dials a host.
type privateProxyNativeGenerator struct{}

// Empty enumeration leaves remote admission unavailable without a fake sender.
func (self *privateProxyNativeGenerator) NextDestinations(int, []connect.MultiHopId, string) (map[connect.MultiHopId]connect.DestinationStats, error) {
	return map[connect.MultiHopId]connect.DestinationStats{}, nil
}

// Generated identifiers are test-local and contain no credentials.
func (self *privateProxyNativeGenerator) NewClientArgs() (*connect.MultiClientGeneratorClientArgs, error) {
	return &connect.MultiClientGeneratorClientArgs{ClientId: connect.NewId()}, nil
}

// Empty enumeration creates no argument resources.
func (self *privateProxyNativeGenerator) RemoveClientArgs(*connect.MultiClientGeneratorClientArgs) {}

// Keep the ordinary ownership rule if enumeration ever changes unexpectedly.
func (self *privateProxyNativeGenerator) RemoveClientWithArgs(client *connect.Client, _ *connect.MultiClientGeneratorClientArgs) {
	client.Close()
}

// Retain the real carrier settings even though no carrier can be created.
func (self *privateProxyNativeGenerator) NewClientSettings() *connect.ClientSettings {
	return connect.DefaultClientSettings()
}

// A constructor call here is a fixture error rather than an external dial.
func (self *privateProxyNativeGenerator) NewClient(context.Context, *connect.MultiClientGeneratorClientArgs, *connect.ClientSettings) (*connect.Client, error) {
	return nil, errors.New("empty native routing fixture attempted a carrier")
}

// Use the ordinary expanding window, containing zero destinations.
func (self *privateProxyNativeGenerator) FixedDestinationSize() (int, bool) { return 0, false }

// Observe two exact native read boundaries and preserve every policy decision.
// Neither this wrapper nor its barriers retain borrowed packet bytes.
type privateProxyNativeReadPolicy struct {
	connect.SecurityPolicy
	entered, control chan struct{}
	release          <-chan struct{}
	first, second    sync.Once
}

// The first actual TCP SYN is held before the later UDP control is emitted.
func (self *privateProxyNativeReadPolicy) InspectEgress(mode protocol.ProvideMode, path *connect.IpPath, payload []byte) (connect.SecurityPolicyResult, error) {
	if path.Protocol == connect.IpProtocolTcp && path.Syn && !path.Ack && path.DestinationPort == 443 && path.DestinationIp.Equal(net.IPv4(203, 0, 113, 44)) {
		self.first.Do(func() {
			close(self.entered)
			<-self.release
		})
	}
	if path.Protocol == connect.IpProtocolUdp && path.DestinationPort == 53 && path.DestinationIp.Equal(net.IPv4(198, 51, 100, 53)) {
		self.second.Do(func() { close(self.control) })
	}
	return self.SecurityPolicy.InspectEgress(mode, path, payload)
}

// A real Proxy.Run must reach its next native batch before unavailable remote
// admission uses the generic five-second budget. This is a routing control;
// both remote offers are refused, so no successful DNS transaction is claimed.
func TestPrivateProxyNativeRunNextReadPrecedesRemoteReadiness(t *testing.T) {
	for _, size := range []int{60, 256, 2048} {
		connect.MessagePoolReturn(connect.MessagePoolGet(size))
	}
	baselineRoots, baselineBytes := connect.MessagePoolOutstandingCount(), connect.MessagePoolOutstandingByteCount()
	previousViolation := connect.SetMessagePoolViolationHandler(func(err error, _ []byte) {
		t.Errorf("proxy native routing pool custody: %v", err)
	})
	t.Cleanup(func() { connect.SetMessagePoolViolationHandler(previousViolation) })
	ctx, cancel := context.WithCancel(context.Background())
	entered, controlSeen, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unpark := func() { releaseOnce.Do(func() { close(release) }) }
	var device *ProxyDevice
	var sdkDevice *sdk.DeviceLocal
	var networkSpace *sdk.NetworkSpace
	var runDone, dialDone <-chan struct{}
	t.Cleanup(func() {
		unpark()
		cancel()
		if device != nil {
			_ = device.tun.Close()
		}
		for _, done := range []<-chan struct{}{runDone, dialDone} {
			if done != nil {
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Error("actual proxy native reader or dial did not join")
				}
			}
		}
		if sdkDevice != nil {
			joinCtx, joinCancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := sdkDevice.CloseAndWait(joinCtx); err != nil {
				t.Errorf("join actual hosted SDK: %v", err)
			}
			joinCancel()
		}
		if networkSpace != nil {
			networkSpace.Close()
		}
		if roots, bytes := connect.MessagePoolOutstandingCount(), connect.MessagePoolOutstandingByteCount(); roots != baselineRoots || bytes != baselineBytes {
			t.Errorf("proxy native routing retained pooled roots after joins: roots=%d/%d bytes=%d/%d", roots, baselineRoots, bytes, baselineBytes)
		}
	})
	proxySettings := DefaultProxyDeviceSettings()
	proxySettings.DisableWindowIdentityPersistence = true
	settings := newProxyDeviceLocalSettings(ctx, &model.ProxyDeviceConfig{}, proxySettings)
	if !settings.HostedIncompatible || settings.SendTimeout != 5*time.Second {
		t.Fatal("actual hosted constructor lost its hosted or generic five-second contract")
	}
	networkSpace = sdk.NewNetworkSpaceWithUrls(ctx, "http://127.0.0.1:1", "ws://127.0.0.1:1", connect.DefaultClientStrategySettings())
	var err error
	sdkDevice, err = sdk.NewPlatformDeviceLocal(
		func([]*connect.ProviderSpec) connect.MultiClientGenerator { return &privateProxyNativeGenerator{} },
		networkSpace, proxyDeviceTransportTestJwt(connect.NewId()), "native routing test", "test", "test", sdk.NewId(), settings,
	)
	if err != nil {
		t.Fatal(err)
	}
	sdkDevice.SetUpgradeMuxSettings(nil)
	sdkDevice.SetClientSecurityPolicyGenerator(func(context.Context, *connect.SecurityPolicyStatsCollector) connect.SecurityPolicy {
		return &privateProxyNativeReadPolicy{SecurityPolicy: connect.DisableSecurityPolicy(), entered: entered, control: controlSeen, release: release}
	})
	if err := sdkDevice.SetConnectLocationChecked(&sdk.ConnectLocation{ConnectLocationId: &sdk.ConnectLocationId{BestAvailable: true}, Name: "native routing test"}); err != nil {
		t.Fatal(err)
	}
	tun, err := connect.CreateTunWithResolver(ctx, proxyDeviceTunSettings(proxySettings), nil)
	if err != nil {
		t.Fatal(err)
	}
	device = &ProxyDevice{ctx: ctx, cancel: cancel, deviceLocal: sdkDevice, deviceState: sdkDevice, tun: tun, settings: proxySettings, receiveMonitor: connect.NewMonitor()}
	running := make(chan struct{})
	runDone = running
	go func() { defer close(running); device.Run() }()
	dialing := make(chan struct{})
	dialDone = dialing
	go func() {
		defer close(dialing)
		connection, _ := tun.DialContext(ctx, "tcp4", "203.0.113.44:443")
		if connection != nil {
			_ = connection.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("actual proxy reader did not reach its first remote SYN")
	}
	control, err := tun.DialContext(ctx, "udp4", "198.51.100.53:53")
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	// A finite synthetic DNS header suffices: this control observes routing,
	// without asking a resolver, host network, or provider to answer it.
	question := []byte{0x7a, 0x69, 0x01, 0x00, 0, 0, 0, 0, 0, 0, 0, 0}
	if count, err := control.Write(question); err != nil || count != len(question) {
		t.Fatalf("actual later proxy control write: count=%d error=%v", count, err)
	}
	unpark()
	// The exact policy barrier forces separate native reads. The watchdog is
	// below the unchanged remote budget, not a virtual-time scheduling proof.
	select {
	case <-controlSeen:
	case <-time.After(2 * time.Second):
		t.Fatal("NATIVE_PROXY_RUN_NEXT_READ_WAITED_FOR_REMOTE: later native batch stayed behind generic remote admission")
	}
}
