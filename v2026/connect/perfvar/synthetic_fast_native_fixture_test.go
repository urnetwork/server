// A bounded public-SDK fixture gives each consumer and Desktop128 provider
// its actual constructor policy. Existing seven-slot browsing fixtures stay separate.
package perfvar

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/sdk/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// The complete SDK64 default window has ten quality and two speed providers.
const syntheticFastProviderCapacity = 12

// Only physical fixture endpoints are retained here; reports use ordinals.
type syntheticFastNativeProvider struct {
	id       clientconnect.Id
	device   *sdk.DeviceLocal
	space    *sdk.NetworkSpace
	carrier  *clientconnect.Tun
	strategy *clientconnect.ClientStrategy
}

// A full endpoint tuple owns one upstream socket until its exact Close.
type syntheticFastNativeOriginConn struct {
	net.Conn
	fixture   *syntheticFastNativeFixture
	tuple     sdkBrowsingOriginTuple
	provider  int
	closeOnce sync.Once
	closeErr  error
}

// Close cannot remove a later socket that happens to reuse this tuple.
func (self *syntheticFastNativeOriginConn) Close() error {
	self.closeOnce.Do(func() {
		self.closeErr = self.Conn.Close()
		self.fixture.stateLock.Lock()
		if self.fixture.originOwners[self.tuple] == self {
			delete(self.fixture.originOwners, self.tuple)
		}
		self.fixture.stateLock.Unlock()
		self.fixture.originCloses[self.provider].Add(1)
	})
	return self.closeErr
}

// Every lifetime is bounded independently of the five retained request owners.
type syntheticFastNativeFixture struct {
	t                     testing.TB
	ctx                   context.Context
	cancel                context.CancelFunc
	setupCtx              context.Context
	cancelSetup           context.CancelFunc
	environment           *routeEnvironment
	profile               networkProfile
	providerProfile       networkProfile
	quality               int
	speed                 int
	target                int64
	providers             []syntheticFastNativeProvider
	device                *sdk.DeviceLocal
	space                 *sdk.NetworkSpace
	deviceCarrier         *clientconnect.Tun
	deviceStrategy        *clientconnect.ClientStrategy
	appTun                *clientconnect.Tun
	receiveUnsub          func()
	bridgeDone            chan struct{}
	bridgeStarted         atomic.Uint64
	bridgeCompleted       atomic.Uint64
	bridgeAccepted        atomic.Uint64
	bridgeRefused         atomic.Uint64
	bridgeInvalid         atomic.Bool
	closing               atomic.Bool
	receiveOfferedPackets atomic.Uint64
	receiveOfferedBytes   atomic.Uint64
	receiveAcceptedBytes  atomic.Uint64
	receiveInvalid        atomic.Bool
	receiveError          atomic.Pointer[string]
	limitedResponses      atomic.Uint64
	limitedOffered        atomic.Uint64
	stateLock             sync.Mutex
	originPorts           [4]int
	originOwners          map[sdkBrowsingOriginTuple]*syntheticFastNativeOriginConn
	originDials           [syntheticFastProviderCapacity]atomic.Uint64
	originCloses          [syntheticFastProviderCapacity]atomic.Uint64
	closeOnce             sync.Once
	closeErr              error
	afterJoin             *sdk.DeviceLocalMemoryUsage
	providerAfterJoin     []*sdk.DeviceLocalMemoryUsage
	ownedDoh              *syntheticFastOwnedDoh
}

// The public complete Connect factory avoids retaining desktop or fixed32
// provisional queue minima in the explicit Android64 consumer profile.
func syntheticFastConsumerSettings(target int64) (*sdk.DeviceLocalSettings, error) {
	if clientconnect.MemoryBudget() != 0 {
		return nil, errors.New("synthetic Fast requires the unchanged process-zero memory policy")
	}
	switch target {
	case 32 * 1024 * 1024:
		return sdk.DefaultLowMemoryDeviceLocalSettings(), nil
	case 64 * 1024 * 1024:
		settings := sdk.DefaultDeviceLocalSettings()
		settings.MemoryTargetByteCount, settings.LowMemoryProfile = target, true
		settings.ClientSettings = *clientconnect.DefaultClientSettingsWithBufferSizeAndMemoryTarget(settings.SequenceBufferSize, target)
		settings.WebRtcSettings.UseEgressOnlyIceInterfaces = true
		return settings, nil
	default:
		return nil, errors.New("synthetic Fast requires an explicit SDK32 or SDK64 consumer")
	}
}

// The application stack uses the same resource and route-readiness settings
// stage in setup and in the focused constructor regression.
func (self *syntheticFastNativeFixture) applicationTunSettings(resources tunResourceProfile) *clientconnect.TunSettings {
	settings := clientconnect.DefaultTunSettingsWithBufferSize(resources.ChannelSize)
	settings.Mtu = resolvedFullTunApplicationMtu(self.profile, resources)
	applyFullTunApplicationDialSettings(settings, &fullTunPath{ctx: self.ctx, environment: self.environment, route: fullTunRouteExchangeH1, p2pHopCount: 1})
	applyTunResourceProfile(settings, resources)
	return settings
}

// Construction never changes process memory, production queues, contracts,
// window sizes, flow policy, or an existing browsing fixture.
func newSyntheticFastNativeFixture(t testing.TB, target int64, profile, providerProfile networkProfile) (fixture *syntheticFastNativeFixture, resultErr error) {
	t.Helper()
	settings, err := syntheticFastConsumerSettings(target)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Second)
	setupCtx, cancelSetup := context.WithTimeout(ctx, 180*time.Second)
	fixture = &syntheticFastNativeFixture{t: t, ctx: ctx, cancel: cancel, setupCtx: setupCtx, cancelSetup: cancelSetup,
		profile: profile, providerProfile: providerProfile, target: target, quality: 5, speed: 1,
		originOwners: make(map[sdkBrowsingOriginTuple]*syntheticFastNativeOriginConn)}
	if target == 64*1024*1024 {
		fixture.quality, fixture.speed = 10, 2
	}
	fixture.providers = make([]syntheticFastNativeProvider, fixture.quality+fixture.speed)
	constructed := false
	defer func() {
		if !constructed {
			resultErr = errors.Join(resultErr, fixture.close())
		}
	}()
	t.Cleanup(func() {
		if err := fixture.close(); err != nil {
			t.Error(err)
		}
	})
	fixture.environment = newRouteEnvironmentWithNetworkPeers(ctx, t, profile, false)
	fixture.environment.deviceAccessProfile, fixture.environment.providerAccessProfile = profile, providerProfile
	fixture.ownedDoh, err = newSyntheticFastOwnedDoh(t)
	if err != nil {
		return fixture, err
	}
	providerNetworkId, providerUserId := server.NewId(), server.NewId()
	model.Testing_CreateNetwork(ctx, providerNetworkId, "synthetic-fast-providers", providerUserId)
	for ordinal := range fixture.providers {
		if err := fixture.buildProvider(ordinal, providerNetworkId, providerUserId); err != nil {
			return fixture, fmt.Errorf("synthetic Fast provider %d: %w", ordinal, err)
		}
	}
	fixture.deviceCarrier, fixture.deviceStrategy = fixture.environment.newClientNodeWithProfileAt(false, profile, fixture.environment.deviceEdgeName)
	fixture.space = fixture.newSpace(fixture.deviceStrategy, false)
	deviceId, clientId := server.NewId(), server.NewId()
	model.Testing_CreateDevice(ctx, fixture.environment.networkId, deviceId, clientId, "synthetic Fast consumer", "synthetic public SDK consumer")
	byJwt := session.NewByJwt(fixture.environment.networkId, fixture.environment.userId, "synthetic-fast-consumer", false, false).Client(deviceId, clientId).Testing_Sign()
	settings.AllowProvider, settings.ProvideExtenderEnabled, settings.DefaultProvideExtender, settings.EnableRpc = true, false, false, false
	settings.DefaultProvideControlMode = sdk.ProvideControlModeNever
	settings.DefaultRouteLocal, settings.DefaultOffline, settings.DefaultTunnelStarted = false, false, true
	settings.ProviderDiscovery = &syntheticFastNativeDiscovery{fixture: fixture, base: &sdkBrowsingSetupApiDiscovery{
		strategy: fixture.deviceStrategy, apiUrl: fmt.Sprintf("http://%s:%d", fixture.environment.edgeAddress, fixture.environment.apiPort)}}
	fixture.device, err = sdk.NewDeviceLocal(fixture.space, byJwt, "synthetic Fast consumer", "synthetic public SDK consumer", "perfvar", sdk.NewId(), settings)
	if err != nil {
		return fixture, err
	}
	fixture.device.SetClientSecurityPolicyGenerator(func(ctx context.Context, stats *clientconnect.SecurityPolicyStatsCollector) clientconnect.SecurityPolicy {
		return &syntheticFastNativePolicy{SecurityPolicy: clientconnect.DefaultSecurityPolicyWithStats(ctx, stats), fixture: fixture}
	})
	fixture.device.SetTransportSettings(&sdk.TransportSettings{Mode: sdk.TransportModeH1})
	fixture.device.SetProviderTransportSettings(&sdk.TransportSettings{Mode: sdk.TransportModeH1})
	fixture.device.SetRouteLocal(false)
	fixture.device.SetProvideMode(sdk.ProvideModeNone)
	fixture.device.SetProvidePaused(true)
	fixture.device.SetPerformanceProfile(&sdk.PerformanceProfile{WindowType: sdk.WindowTypeAuto, AllowDirect: false})
	// The isolated carrier has no Internet route. Keep the default resolver
	// identities/policy and serve those exact endpoints with scoped fixture trust.
	fixture.device.SetUpgradeMuxSettings(fixture.ownedDoh.settings())
	resources := defaultTunResourceProfile()
	tunSettings := fixture.applicationTunSettings(resources)
	fixture.appTun, err = clientconnect.CreateTun(ctx, tunSettings)
	if err != nil {
		return fixture, err
	}
	fixture.receiveUnsub = fixture.device.AddReceivePacketsCallback(func(_ clientconnect.TransferPath, _ protocol.ProvideMode, _ *clientconnect.IpPath, packets [][]byte) {
		fixture.receiveApplicationPackets(packets)
	})
	fixture.bridgeDone = make(chan struct{})
	go fixture.runNativeBridge(resources.BatchSize)
	specs := sdk.NewProviderSpecList()
	specs.Add(&sdk.ProviderSpec{BestAvailable: true})
	fixture.device.SetDestination(&sdk.ConnectLocation{ConnectLocationId: &sdk.ConnectLocationId{BestAvailable: true}}, specs)
	if fixture.device.MemoryUsed().TargetByteCount != target || !settings.LowMemoryProfile ||
		settings.GeneratorFunc != nil || settings.SendBufferSettings.ApplicationPacing || clientconnect.DefaultMultiClientSettings().MaxFlowsPerExit != 16 {
		return fixture, errors.New("synthetic Fast actual consumer constructor policy changed")
	}
	if err := fixture.waitInitial(); err != nil {
		return fixture, err
	}
	constructed = true
	return fixture, nil
}

// API and platform addresses both traverse the endpoint's emulated carrier.
func (self *syntheticFastNativeFixture) newSpace(strategy *clientconnect.ClientStrategy, provider bool) *sdk.NetworkSpace {
	settings := routeClientStrategySettings()
	settings.EnableResilient = false
	settings.MinNextConnectDelay, settings.MaxNextConnectDelay = 0, 0
	settings.ConnectSettings = *strategy.ConnectSettings()
	address, apiPort, platformPort := self.environment.edgeAddress, self.environment.apiPort, self.environment.h1Port
	if provider {
		address, apiPort, platformPort = self.environment.providerEdgeAddress, self.environment.providerApiPort, self.environment.providerH1Port
	}
	return sdk.NewNetworkSpaceWithUrls(self.ctx, fmt.Sprintf("http://%s:%d", address, apiPort), fmt.Sprintf("wss://%s:%d", address, platformPort), settings)
}

// The public provider factory owns its complete Desktop128 policy. Only
// owned fixture network dials and the documentation-IP exception are supplied.
func (self *syntheticFastNativeFixture) buildProvider(ordinal int, networkId, userId server.Id) error {
	provider := &self.providers[ordinal]
	deviceId, clientId := server.NewId(), server.NewId()
	model.Testing_CreateDevice(self.ctx, networkId, deviceId, clientId, "synthetic Fast provider", "synthetic public SDK Desktop128")
	provider.id = clientconnect.Id(clientId)
	byJwt := session.NewByJwt(networkId, userId, "synthetic-fast-provider", false, false).Client(deviceId, clientId).Testing_Sign()
	provider.carrier, provider.strategy = self.environment.newClientNodeWithProfileAt(false, self.providerProfile, self.environment.providerEdgeName)
	provider.space = self.newSpace(provider.strategy, true)
	settings := sdk.DefaultDeviceLocalSettings()
	settings.MemoryTargetByteCount, settings.LowMemoryProfile = 128*1024*1024, false
	settings.ProvideExtenderEnabled, settings.DefaultProvideExtender, settings.EnableRpc = false, false, false
	settings.DefaultOffline, settings.DefaultTunnelStarted = false, false
	settings.ProviderDialContextSettings = &clientconnect.DialContextSettings{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		return self.dialProvider(ctx, ordinal, network, address)
	}}
	var err error
	provider.device, err = sdk.NewDeviceLocal(provider.space, byJwt, "synthetic Fast provider", "synthetic public SDK Desktop128", "perfvar", sdk.NewId(), settings)
	if err != nil {
		return err
	}
	provider.device.SetProviderSecurityPolicyGenerator(func(ctx context.Context, stats *clientconnect.SecurityPolicyStatsCollector) clientconnect.SecurityPolicy {
		return &syntheticFastNativePolicy{SecurityPolicy: clientconnect.DefaultProviderSecurityPolicyWithStats(ctx, stats), fixture: self}
	})
	provider.device.SetProviderTransportSettings(&sdk.TransportSettings{Mode: sdk.TransportModeH1})
	provider.device.SetTransportSettings(&sdk.TransportSettings{Mode: sdk.TransportModeH1})
	provider.device.SetProvideMode(sdk.ProvideModePublic)
	return self.waitProviders()
}

// The bounded fixture waiter observes public state without forcing SDK work.
func (self *syntheticFastNativeFixture) waitProviders() error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		ready := true
		for index := range self.providers {
			provider := &self.providers[index]
			if provider.device == nil {
				continue
			}
			ready = ready && provider.device.GetProviderConnected() && provider.device.GetProvideEnabled() && provider.device.MemoryUsed().TargetByteCount == 128*1024*1024
		}
		if ready {
			return self.setupCtx.Err()
		}
		select {
		case <-self.setupCtx.Done():
			return fmt.Errorf("synthetic Fast provider did not become public and connected: %w", self.setupCtx.Err())
		case <-ticker.C:
		}
	}
}

// Counts describe the actual public snapshot, including limited clients.
// An empty limited window does not claim that branch carried loaded traffic.
type syntheticFastWindowObservation struct {
	Ready                      bool    `json:"ready"`
	NormalTarget               int     `json:"normal_target"`
	NormalLocations            int     `json:"normal_locations"`
	AllClients                 int     `json:"all_clients"`
	LimitedClients             int     `json:"limited_clients"`
	LimitedLimit               int     `json:"limited_limit"`
	LimitedFraction            float64 `json:"limited_fraction"`
	AuthenticatedPhysicalPeers int     `json:"authenticated_physical_peers"`
	LimitedResponses           uint64  `json:"limited_selector_responses"`
	LimitedOffered             uint64  `json:"limited_provider_offers"`
	LimitedCoverage            string  `json:"limited_coverage"`
}

// Physical providers may be shared by multiple real window clients. Normal
// locations exclude limited clients; GetExits and authenticated identities do not.
func (self *syntheticFastNativeFixture) windowObservation() syntheticFastWindowObservation {
	observation := syntheticFastWindowObservation{LimitedLimit: 2,
		LimitedFraction:  self.device.GetLimitedWindowSizeFraction(),
		LimitedResponses: self.limitedResponses.Load(), LimitedOffered: self.limitedOffered.Load()}
	expectedFraction := 0.5
	if self.target == 64*1024*1024 {
		expectedFraction, observation.LimitedLimit = 1, 10
	}
	policy := self.device.GetPerformanceProfile()
	if policy == nil || policy.WindowType != sdk.WindowTypeAuto || policy.WindowSize != nil || policy.AllowDirect || policy.PostQuantumEncryption {
		return observation
	}
	if self.device.GetProvideMode() != sdk.ProvideModeNone || self.device.GetProvideControlMode() != sdk.ProvideControlModeNever ||
		!self.device.GetProvidePaused() || self.device.GetProvideEnabled() || observation.LimitedFraction != expectedFraction {
		return observation
	}
	exits, status := self.device.GetExits(), self.device.GetWindowStatus()
	locations := self.device.GetConnectedProviderLocations()
	identities := self.device.GetProviderIdentities()
	if exits != nil {
		observation.AllClients = exits.Len()
	}
	if status != nil {
		observation.NormalTarget, observation.LimitedClients = status.TargetSize, status.LimitedWindowSize
	}
	if locations != nil {
		observation.NormalLocations = locations.Len()
	}
	if identities != nil {
		observation.AuthenticatedPhysicalPeers = identities.Len()
	}
	switch {
	case observation.LimitedClients > 0:
		observation.LimitedCoverage = "limited_clients_observed"
	case observation.LimitedOffered > 0:
		observation.LimitedCoverage = "limited_selector_offered_providers_no_current_client"
	case observation.LimitedResponses > 0:
		observation.LimitedCoverage = "actual_limited_selector_responses_returned_no_provider"
	default:
		observation.LimitedCoverage = "no_limited_selector_response_observed"
	}
	providerIds := make([]string, len(self.providers))
	providerKeys := make([][]byte, len(self.providers))
	for index := range self.providers {
		providerIds[index] = self.providers[index].id.String()
		providerKeys[index] = self.providers[index].device.GetPublicIdentityKey()
	}
	observation.Ready = syntheticFastWindowOwnershipReady(self.quality, self.speed, observation.LimitedLimit, providerIds, providerKeys, exits, status, locations, identities)
	return observation
}

// This observes the current defaults; it never resizes or disables a window.
func (self *syntheticFastNativeFixture) windowReady() bool {
	return self.windowObservation().Ready
}

// Discovery admits only owned physical providers. Every reported authenticated
// peer must match its actual owned provider key. Sessions not yet established
// remain unclaimed; readiness does not send or wait for an extra handshake.
func syntheticFastWindowOwnershipReady(qualityTarget, speedTarget, limitedLimit int, providerIds []string, providerKeys [][]byte,
	exits *sdk.ExitList, status *sdk.WindowStatus, locations *sdk.ConnectedProviderLocationList, identities *sdk.ProviderIdentityList,
) bool {
	normalTarget := qualityTarget + speedTarget
	if qualityTarget <= 0 || speedTarget <= 0 || limitedLimit <= 0 || len(providerIds) != normalTarget || len(providerKeys) != len(providerIds) ||
		exits == nil || status == nil || locations == nil || identities == nil || status.ConnectionGeneration <= 0 ||
		status.ProviderStateInEvaluation < 0 || status.TargetSize != normalTarget || status.LimitedWindowSize < 0 || status.LimitedWindowSize > limitedLimit ||
		exits.Len() != normalTarget+status.LimitedWindowSize || locations.Len() != normalTarget {
		return false
	}
	owned := make(map[string]int, len(providerIds))
	for ordinal, id := range providerIds {
		if id == "" {
			return false
		}
		if _, exists := owned[id]; exists {
			return false
		}
		owned[id] = ordinal
	}
	authenticated := make(map[string]bool, identities.Len())
	for index := 0; index < identities.Len(); index++ {
		identity := identities.Get(index)
		if identity == nil || identity.ClientId == nil || len(identity.PublicKey) == 0 || authenticated[identity.ClientId.String()] {
			return false
		}
		id := identity.ClientId.String()
		ordinal, exists := owned[id]
		if !exists || !bytes.Equal(identity.PublicKey, providerKeys[ordinal]) {
			return false
		}
		authenticated[id] = true
	}
	quality, speed, limited := 0, 0, 0
	localIds := make(map[string]bool, exits.Len())
	for index := 0; index < exits.Len(); index++ {
		exit := exits.Get(index)
		if exit == nil || exit.ClientId == nil || exit.Done || exit.Warning || exit.Quarantined || exit.P2pOnly || localIds[exit.ClientId.String()] ||
			(exit.CapabilityIp != "ipv4" && exit.CapabilityIp != "dualstack") ||
			(exit.CapabilityProtocol != "tcp" && exit.CapabilityProtocol != "dualproto") {
			return false
		}
		localIds[exit.ClientId.String()] = true
		switch exit.WindowType {
		case "quality":
			quality++
		case "speed":
			speed++
		case sdk.ExitWindowTypeLimited:
			if exit.CapabilityProtocol != "tcp" || exit.OverflowStep != 0 {
				return false
			}
			limited++
		default:
			return false
		}
	}
	qualityLocations, speedLocations := 0, 0
	physicalIds := make(map[string]bool, len(providerIds))
	for index := 0; index < locations.Len(); index++ {
		location := locations.Get(index)
		if location == nil || location.ClientId == nil {
			return false
		}
		id := location.ClientId.String()
		ordinal, exists := owned[id]
		if !exists {
			return false
		}
		physicalIds[id] = true
		if ordinal < qualityTarget {
			qualityLocations++
		} else {
			speedLocations++
		}
	}
	return quality == qualityTarget && speed == speedTarget && limited == status.LimitedWindowSize &&
		qualityLocations == qualityTarget && speedLocations == speedTarget && len(physicalIds) >= 2 && len(physicalIds) <= len(providerIds)
}

// No window cap, fixed destination or setup-only warm request is installed.
func (self *syntheticFastNativeFixture) waitInitial() error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if self.windowReady() {
			return self.setupCtx.Err()
		}
		select {
		case <-self.setupCtx.Done():
			return fmt.Errorf("synthetic Fast default window geometry did not become ready: %w", self.setupCtx.Err())
		case <-ticker.C:
		}
	}
}

// The application callback borrows packet slices through the real TUN write.
// Compact outbound admission remains independently counted in packets.
func (self *syntheticFastNativeFixture) receiveApplicationPackets(packets [][]byte) {
	self.receiveOfferedPackets.Add(uint64(len(packets)))
	offeredBytes := 0
	for _, packet := range packets {
		offeredBytes += len(packet)
	}
	self.receiveOfferedBytes.Add(uint64(offeredBytes))
	accepted, err := self.appTun.WriteBatch(packets)
	if accepted < 0 || accepted > offeredBytes {
		self.receiveInvalid.Store(true)
	} else {
		self.receiveAcceptedBytes.Add(uint64(accepted))
	}
	if err != nil && !self.closing.Load() {
		message := err.Error()
		self.receiveError.CompareAndSwap(nil, &message)
	}
}

// Takes every TUN original locally; the public compact ABI copies each input.
// The callback borrows received packets only through the synchronous TUN write.
func (self *syntheticFastNativeFixture) runNativeBridge(batchSize int) {
	defer close(self.bridgeDone)
	packets := make([][]byte, batchSize)
	returnPackets := func() {
		for index, packet := range packets {
			if packet != nil {
				clientconnect.MessagePoolReturn(packet)
				packets[index] = nil
			}
		}
	}
	defer returnPackets()
	for {
		count, err := self.appTun.ReadBatch(packets)
		if err != nil {
			return
		}
		if count <= 0 || count > len(packets) || count > 64 {
			self.bridgeInvalid.Store(true)
			return
		}
		byteCount := 0
		for _, packet := range packets[:count] {
			if len(packet) == 0 || len(packet) > 65535 {
				self.bridgeInvalid.Store(true)
				return
			}
			byteCount += 2 + len(packet)
		}
		if byteCount > 96*1024 {
			self.bridgeInvalid.Store(true)
			return
		}
		compact, offset := make([]byte, byteCount), 0
		for _, packet := range packets[:count] {
			binary.BigEndian.PutUint16(compact[offset:offset+2], uint16(len(packet)))
			offset += 2
			copy(compact[offset:], packet)
			offset += len(packet)
		}
		self.bridgeStarted.Add(uint64(count))
		accepted := self.device.SendPacketBatch(compact)
		if accepted < 0 || accepted > int32(count) {
			self.bridgeInvalid.Store(true)
			return
		}
		self.bridgeAccepted.Add(uint64(accepted))
		self.bridgeRefused.Add(uint64(int32(count) - accepted))
		returnPackets()
		self.bridgeCompleted.Add(uint64(count))
	}
}

// Only the four currently owned origin destinations bypass ordinary policy.
func (self *syntheticFastNativeFixture) matches(ip net.IP, port int, protocol clientconnect.IpProtocol) bool {
	if port != 443 || protocol != clientconnect.IpProtocolTcp {
		return false
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for index, originPort := range self.originPorts {
		if originPort > 0 && ip.Equal(net.IPv4(203, 0, 113, byte(10+index))) {
			return true
		}
	}
	return false
}

// Control/API/carrier dials keep the provider's emulated interface; only
// documentation origins translate to newly owned local listeners.
func (self *syntheticFastNativeFixture) dialProvider(ctx context.Context, ordinal int, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	ip := net.ParseIP(host)
	if portNumber, parseErr := strconv.Atoi(port); parseErr == nil && self.ownedDoh != nil &&
		(network == "tcp" || network == "tcp4" || network == "tcp6") && self.ownedDoh.matches(ip, portNumber) {
		return self.ownedDoh.dial(ctx, network, address)
	}
	self.stateLock.Lock()
	originPort := 0
	for index, ownedPort := range self.originPorts {
		if ownedPort > 0 && port == "443" && ip.Equal(net.IPv4(203, 0, 113, byte(10+index))) {
			originPort = ownedPort
			break
		}
	}
	self.stateLock.Unlock()
	if originPort == 0 {
		if portNumber, parseErr := strconv.Atoi(port); parseErr == nil && self.ownedDoh != nil &&
			(network == "tcp" || network == "tcp4" || network == "tcp6") && self.ownedDoh.matches(ip, portNumber) {
			self.ownedDoh.observeMissingRoute()
		}
		return self.providers[ordinal].carrier.DialContext(ctx, network, address)
	}
	if network != "tcp" && network != "tcp4" {
		return nil, errors.New("synthetic Fast origin requires TCP")
	}
	connection, err := (&net.Dialer{}).DialContext(ctx, "tcp4", net.JoinHostPort("127.0.0.1", fmt.Sprint(originPort)))
	if err != nil {
		return nil, err
	}
	tuple := sdkBrowsingOriginTuple{sourceAddr: connection.LocalAddr().String(), destinationAddr: connection.RemoteAddr().String()}
	owned := &syntheticFastNativeOriginConn{Conn: connection, fixture: self, tuple: tuple, provider: ordinal}
	self.stateLock.Lock()
	if self.originOwners[tuple] != nil {
		self.stateLock.Unlock()
		_ = connection.Close()
		return nil, errors.New("synthetic Fast upstream tuple already has a live owner")
	}
	self.originOwners[tuple] = owned
	self.stateLock.Unlock()
	self.originDials[ordinal].Add(1)
	return owned, nil
}

// Exact accepted/dial endpoint matching never guesses from a source port.
func (self *syntheticFastNativeFixture) providerForRequest(request *http.Request) int {
	local, ok := request.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok || local == nil {
		return -1
	}
	tuple := sdkBrowsingOriginTuple{sourceAddr: request.RemoteAddr, destinationAddr: local.String()}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if owner := self.originOwners[tuple]; owner != nil {
		return owner.provider
	}
	return -1
}

// Public per-device accounting is sampled after its exact successful join.
func syntheticFastDeviceJoin(ctx context.Context, device *sdk.DeviceLocal) (*sdk.DeviceLocalMemoryUsage, error) {
	if device == nil {
		return nil, nil
	}
	if err := device.CloseAndWait(ctx); err != nil {
		return nil, err
	}
	usage := device.MemoryUsed()
	if usage.TransferRootUsedByteCount != 0 || usage.TransferRootReservedByteCount != usage.TransferRootReleasedByteCount ||
		usage.PlatformTransportUsedByteCount != 0 || usage.PlatformTransportUsedCount != 0 || usage.PlatformTransportPendingH1Count != 0 ||
		usage.PlatformTransportPendingHandoffCount != 0 || usage.PlatformTransportActiveHandoffCount != 0 ||
		usage.PlatformTransportReservedBytes != usage.PlatformTransportReleasedBytes {
		return usage, errors.New("synthetic Fast public SDK ownership remained after join")
	}
	return usage, nil
}

// Close preserves exact SDK/provider/native owners through joined shutdown.
// This does not reset any fixture between the five measurements.
func (self *syntheticFastNativeFixture) close() error {
	self.closeOnce.Do(func() {
		self.closing.Store(true)
		joinCtx, cancelJoin := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancelJoin()
		if self.appTun != nil {
			self.closeErr = errors.Join(self.closeErr, self.appTun.Close())
		}
		if self.device != nil {
			self.device.Cancel()
		}
		if self.bridgeDone != nil {
			select {
			case <-self.bridgeDone:
			case <-joinCtx.Done():
				self.closeErr = errors.Join(self.closeErr, errors.New("synthetic Fast native bridge did not join"))
			}
		}
		var err error
		self.afterJoin, err = syntheticFastDeviceJoin(joinCtx, self.device)
		self.closeErr = errors.Join(self.closeErr, err)
		if self.receiveUnsub != nil {
			self.receiveUnsub()
		}
		if self.space != nil {
			self.space.Close()
		}
		if self.deviceStrategy != nil {
			self.deviceStrategy.Close()
		}
		if self.deviceCarrier != nil {
			self.closeErr = errors.Join(self.closeErr, self.deviceCarrier.Close())
		}
		self.providerAfterJoin = make([]*sdk.DeviceLocalMemoryUsage, len(self.providers))
		for index := range self.providers {
			provider := &self.providers[index]
			usage, err := syntheticFastDeviceJoin(joinCtx, provider.device)
			self.providerAfterJoin[index] = usage
			self.closeErr = errors.Join(self.closeErr, err)
			if provider.space != nil {
				provider.space.Close()
			}
			if provider.strategy != nil {
				provider.strategy.Close()
			}
			if provider.carrier != nil {
				self.closeErr = errors.Join(self.closeErr, provider.carrier.Close())
			}
			if self.originDials[index].Load() != self.originCloses[index].Load() {
				self.closeErr = errors.Join(self.closeErr, fmt.Errorf("synthetic Fast provider %d origin socket receipts do not balance", index))
			}
		}
		self.stateLock.Lock()
		if len(self.originOwners) != 0 {
			self.closeErr = errors.Join(self.closeErr, errors.New("synthetic Fast upstream socket owner remained"))
		}
		self.stateLock.Unlock()
		if self.bridgeInvalid.Load() || self.bridgeStarted.Load() != self.bridgeCompleted.Load() ||
			self.bridgeCompleted.Load() != self.bridgeAccepted.Load()+self.bridgeRefused.Load() {
			self.closeErr = errors.Join(self.closeErr, errors.New("synthetic Fast native compact input ownership did not reconcile"))
		}
		if self.receiveInvalid.Load() {
			self.closeErr = errors.Join(self.closeErr, errors.New("synthetic Fast application TUN receive byte count is invalid"))
		}
		if message := self.receiveError.Load(); message != nil {
			self.closeErr = errors.Join(self.closeErr, fmt.Errorf("synthetic Fast application TUN receive: %s", *message))
		}
		if self.ownedDoh != nil {
			self.closeErr = errors.Join(self.closeErr, self.ownedDoh.close(joinCtx))
		}
		self.cancelSetup()
		if self.environment != nil {
			self.closeErr = errors.Join(self.closeErr, self.environment.closeWithError())
		}
		self.cancel()
	})
	return self.closeErr
}

// Only fixture endpoint policy is added; every other path retains its default.
type syntheticFastNativePolicy struct {
	clientconnect.SecurityPolicy
	fixture *syntheticFastNativeFixture
}

// Either packet direction can name the owned origin; all other packets defer.
func (self *syntheticFastNativePolicy) InspectIngress(mode protocol.ProvideMode, path *clientconnect.IpPath, packet []byte) (clientconnect.SecurityPolicyResult, error) {
	if path != nil && (self.fixture.matches(path.SourceIp, path.SourcePort, path.Protocol) || self.fixture.matches(path.DestinationIp, path.DestinationPort, path.Protocol)) {
		return clientconnect.SecurityPolicyResultAllow, nil
	}
	return self.SecurityPolicy.InspectIngress(mode, path, packet)
}

// The same exact destination set governs replies without changing base policy.
func (self *syntheticFastNativePolicy) InspectEgress(mode protocol.ProvideMode, path *clientconnect.IpPath, packet []byte) (clientconnect.SecurityPolicyResult, error) {
	if path != nil && (self.fixture.matches(path.SourceIp, path.SourcePort, path.Protocol) || self.fixture.matches(path.DestinationIp, path.DestinationPort, path.Protocol)) {
		return clientconnect.SecurityPolicyResultAllow, nil
	}
	return self.SecurityPolicy.InspectEgress(mode, path, packet)
}

// Named role pools constrain the real authenticated API query, not its results.
type syntheticFastNativeDiscovery struct {
	fixture *syntheticFastNativeFixture
	base    clientconnect.NetworkProviderDiscovery
}

// Caller exclusions, scope, family and count survive the bounded fixture query.
func (self *syntheticFastNativeDiscovery) FindProviders2(ctx context.Context, token string, args *clientconnect.FindProviders2Args) (*clientconnect.FindProviders2Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if args == nil || token == "" {
		return nil, errors.New("synthetic Fast discovery lacks caller authority")
	}
	start, end := 0, self.fixture.quality
	if args.RankMode == "speed" {
		start, end = end, len(self.fixture.providers)
	} else if args.RankMode != "" && args.RankMode != "quality" {
		return nil, errors.New("synthetic Fast discovery rank mode is unsupported")
	}
	query := *args
	query.ExcludeClientIds = slices.Clone(args.ExcludeClientIds)
	query.ExcludeDestinations = make([][]clientconnect.Id, len(args.ExcludeDestinations))
	for index, path := range args.ExcludeDestinations {
		query.ExcludeDestinations[index] = slices.Clone(path)
	}
	query.Specs = nil
	for _, spec := range args.Specs {
		if spec == nil || spec.LocationId != nil || spec.LocationGroupId != nil || !spec.BestAvailable && spec.ClientId == nil {
			return nil, errors.New("synthetic Fast discovery scope is unowned")
		}
		if spec.ClientId == nil {
			continue
		}
		owned := false
		for index := start; index < end; index++ {
			owned = owned || self.fixture.providers[index].id == *spec.ClientId
		}
		if !owned {
			return &clientconnect.FindProviders2Result{Providers: []*clientconnect.FindProvidersProvider{}}, nil
		}
		id := *spec.ClientId
		query.Specs = append(query.Specs, &clientconnect.ProviderSpec{ClientId: &id})
	}
	if len(query.Specs) == 0 {
		for index := start; index < end; index++ {
			id := self.fixture.providers[index].id
			query.Specs = append(query.Specs, &clientconnect.ProviderSpec{ClientId: &id})
		}
	}
	result, err := self.base.FindProviders2(ctx, token, &query)
	if err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if result == nil {
		return nil, errors.New("synthetic Fast discovery lost actual API result")
	}
	for _, candidate := range result.Providers {
		if candidate == nil {
			return nil, errors.New("synthetic Fast discovery returned an empty provider")
		}
		if len(candidate.IntermediaryIds) != 0 {
			return nil, errors.New("synthetic Fast relay benchmark requires an actual one-hop provider result")
		}
		owned := false
		for index := start; index < end; index++ {
			owned = owned || self.fixture.providers[index].id == candidate.ClientId
		}
		if !owned {
			return nil, errors.New("synthetic Fast discovery returned an unowned provider")
		}
	}
	if args.Select != nil && args.Select.Protocol == clientconnect.ProviderProtocolCapabilityTcp &&
		result.Select != nil && result.Select.Protocol == clientconnect.ProviderProtocolCapabilityTcp {
		// Publish offers first so a zero-offer report cannot race their response.
		self.fixture.limitedOffered.Add(uint64(len(result.Providers)))
		self.fixture.limitedResponses.Add(1)
	}
	return result, nil
}
