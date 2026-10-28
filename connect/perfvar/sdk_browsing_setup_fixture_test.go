// Public SDK setup uses real authenticated H1 providers and ordinary discovery.
// No application workload or private window callback is part of this qualifier.
package perfvar

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/sdk"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// Topology v2 owns five distinct initial quality tails plus one speed tail.
// Ordinal4 stays physically absent until the unchanged late-constructor edge.
const sdkBrowsingProviderCount = 7
const sdkBrowsingInitialQualityMask uint8 = 0x4f
const sdkBrowsingTopology = "public_sdk32_quality5_speed1_distinct_initial_v2"

// Initial quality roles0..3 and6 share the same navigation-refusal policy.
func sdkBrowsingInitialQualityProvider(ordinal int) bool {
	return 0 <= ordinal && ordinal < 4 || ordinal == 6
}

// Publication follows complete transport, NAT and public-mode registration.
// All non-atomic fields remain immutable after ready becomes true.
type sdkBrowsingSetupProvider struct {
	ready        atomic.Bool
	id           clientconnect.Id
	carrier      *clientconnect.Tun
	strategy     *clientconnect.ClientStrategy
	client       *clientconnect.Client
	transport    *clientconnect.PlatformTransport
	localNat     *clientconnect.LocalUserNat
	remoteNat    *clientconnect.RemoteUserNatProvider
	resendBudget *clientconnect.TransferMemoryBudget
	noAck        *noAckSendTracker
	packs        *sendPackLifecycleTracker
	returns      *providerReturnSendTracker
}

// Every acquired resource is retained until joined, including partial setup.
// The absent ordinal4 is the future physically late quality provider.
type sdkBrowsingSetupFixture struct {
	t                 testing.TB
	ctx               context.Context
	setupCtx          context.Context
	cancel            context.CancelFunc
	cancelSetup       context.CancelFunc
	environment       *routeEnvironment
	providerNetworkId server.Id
	providerUserId    server.Id
	providers         [sdkBrowsingProviderCount]sdkBrowsingSetupProvider
	deviceCarrier     *clientconnect.Tun
	deviceStrategy    *clientconnect.ClientStrategy
	space             *sdk.NetworkSpace
	device            *sdk.DeviceLocal
	discovery         *sdkBrowsingSetupDiscovery
	closeOnce         sync.Once
	closeErr          error
	afterJoin         *sdk.DeviceLocalMemoryUsage
	consumerClientId  server.Id
	callerToken       string
	scenario          *sdkBrowsingScenario
	native            atomic.Pointer[sdkBrowsingNative]
}

// Retain the public factory and its default multi-client/profile construction.
// Provider fixtures have their own network so the relationship is public.
func newSdkBrowsingSetupFixture(t testing.TB, plan *nativeBrowsingPressurePlan, scenarios ...*sdkBrowsingScenario) (fixture *sdkBrowsingSetupFixture, resultErr error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Second)
	setupCtx, cancelSetup := context.WithTimeout(ctx, 180*time.Second)
	fixture = &sdkBrowsingSetupFixture{t: t, ctx: ctx, setupCtx: setupCtx, cancel: cancel, cancelSetup: cancelSetup}
	if len(scenarios) != 0 {
		fixture.scenario = scenarios[0]
		fixture.scenario.fixture = fixture
	}
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
	fixture.environment = newRouteEnvironmentWithNetworkPeers(ctx, t, plan.profile, false)
	fixture.environment.deviceAccessProfile = plan.profile
	fixture.environment.providerAccessProfile = plan.provider
	fixture.providerNetworkId, fixture.providerUserId = server.NewId(), server.NewId()
	model.Testing_CreateNetwork(ctx, fixture.providerNetworkId, fmt.Sprintf("sdk-browsing-provider-%s", fixture.providerNetworkId), fixture.providerUserId)
	for ordinal := range fixture.providers {
		if ordinal != 4 {
			if err := fixture.buildProvider(ordinal, plan); err != nil {
				return fixture, fmt.Errorf("public SDK provider %d setup: %w", ordinal, err)
			}
		}
	}
	fixture.deviceCarrier, fixture.deviceStrategy = fixture.environment.newClientNodeWithProfileAt(false, plan.profile, fixture.environment.deviceEdgeName)
	strategySettings := routeClientStrategySettings()
	strategySettings.EnableResilient = false
	strategySettings.MinNextConnectDelay, strategySettings.MaxNextConnectDelay = 0, 0
	strategySettings.ConnectSettings = *fixture.deviceStrategy.ConnectSettings()
	apiUrl := fmt.Sprintf("http://%s:%d", fixture.environment.edgeAddress, fixture.environment.apiPort)
	platformUrl := fmt.Sprintf("wss://%s:%d", fixture.environment.edgeAddress, fixture.environment.h1Port)
	fixture.space = sdk.NewNetworkSpaceWithUrls(ctx, apiUrl, platformUrl, strategySettings)
	deviceId, clientId := server.NewId(), server.NewId()
	fixture.consumerClientId = clientId
	model.Testing_CreateDevice(ctx, fixture.environment.networkId, deviceId, clientId, "sdk browsing device", "synthetic sdk browsing device")
	for ordinal := range fixture.providers {
		if ordinal == 4 {
			continue
		}
		details := model.GetProvideRelationshipDetails(setupCtx, clientId, server.Id(fixture.providers[ordinal].id))
		if details.Mode != model.ProvideModePublic || details.SourceLifecycle != model.NetworkClientLifecycleActiveTop || details.DestinationLifecycle != model.NetworkClientLifecycleActiveTop {
			return fixture, fmt.Errorf("public SDK relationship missing for provider %d", ordinal)
		}
	}
	byJwt := session.NewByJwt(fixture.environment.networkId, fixture.environment.userId, fmt.Sprintf("sdk-browsing-%s", fixture.environment.networkId), false, false).Client(deviceId, clientId).Testing_Sign()
	fixture.callerToken = byJwt
	fixture.discovery = newSdkBrowsingSetupDiscovery(fixture.readyProviderIds, &sdkBrowsingSetupApiDiscovery{strategy: fixture.deviceStrategy, apiUrl: apiUrl})
	settings := sdk.DefaultDeviceLocalSettings()
	settings.MemoryTargetByteCount, settings.LowMemoryProfile = 32*1024*1024, true
	settings.AllowProvider, settings.ProvideExtenderEnabled, settings.EnableRpc = false, false, false
	settings.DefaultRouteLocal, settings.DefaultOffline, settings.DefaultTunnelStarted = false, false, true
	settings.GeneratorFunc = nil
	settings.ProviderDiscovery = fixture.discovery
	device, err := sdk.NewDeviceLocal(fixture.space, byJwt, "sdk browsing device", "synthetic sdk browsing device", "perfvar", sdk.NewId(), settings)
	if err != nil {
		return fixture, err
	}
	fixture.device = device
	if fixture.scenario != nil {
		native, err := newSdkBrowsingNative(fixture, fixture.scenario, plan.resources, plan.profile)
		if err != nil {
			return fixture, err
		}
		fixture.native.Store(native)
		fixture.scenario.native = native
		device.SetClientSecurityPolicyGenerator(func(ctx context.Context, stats *clientconnect.SecurityPolicyStatsCollector) clientconnect.SecurityPolicy {
			return &sdkBrowsingOriginPolicy{SecurityPolicy: clientconnect.DefaultSecurityPolicyWithStats(ctx, stats), native: native}
		})
	}
	device.SetTransportSettings(&sdk.TransportSettings{Mode: sdk.TransportModeH1})
	device.SetRouteLocal(false)
	specs := sdk.NewProviderSpecList()
	specs.Add(&sdk.ProviderSpec{BestAvailable: true})
	device.SetDestination(&sdk.ConnectLocation{ConnectLocationId: &sdk.ConnectLocationId{BestAvailable: true}}, specs)
	memory := device.MemoryUsed()
	if settings.GeneratorFunc != nil || settings.MultiClientIdentityStore != nil || fixture.space.GetAsyncLocalState() != nil || settings.SendBufferSettings.ApplicationPacing || memory.TargetByteCount != 32*1024*1024 || memory.TransferRootBudgetByteCount != 21810380 || memory.ClientTransferBudgetByteCount != 21810380 || memory.PackQueueCapacityByteCount != 2181038 {
		return fixture, errors.New("public SDK32 factory or shared mobile admission policy changed")
	}
	constructed = true
	return fixture, nil
}

// A release/acquire publication permits a short immutable ready-ID snapshot.
// No fixture lock is retained across transport reads or backend API calls.
func (self *sdkBrowsingSetupFixture) readyProviderIds() [sdkBrowsingProviderCount]clientconnect.Id {
	var ids [sdkBrowsingProviderCount]clientconnect.Id
	for ordinal := range self.providers {
		provider := &self.providers[ordinal]
		if provider.ready.Load() && provider.transport.IsConnected() {
			ids[ordinal] = provider.id
		}
	}
	return ids
}

// Construction repeats the existing real provider hierarchy and publishes last.
func (self *sdkBrowsingSetupFixture) buildProvider(ordinal int, plan *nativeBrowsingPressurePlan) error {
	provider := &self.providers[ordinal]
	deviceId, clientId := server.NewId(), server.NewId()
	model.Testing_CreateDevice(self.ctx, self.providerNetworkId, deviceId, clientId, "sdk browsing provider", "synthetic sdk browsing provider")
	byJwt := session.NewByJwt(self.providerNetworkId, self.providerUserId, fmt.Sprintf("sdk-browsing-provider-%s", self.providerNetworkId), false, false).Client(deviceId, clientId).Testing_Sign()
	provider.id = clientconnect.Id(clientId)
	provider.carrier, provider.strategy = self.environment.newClientNodeWithProfileAt(false, plan.provider, self.environment.providerEdgeName)
	provider.noAck, provider.packs, provider.returns = newNoAckSendTracker(), newSendPackLifecycleTracker(), newProviderReturnSendTracker()
	settings := fullTunClientSettingsWithFeatures(fullTunRouteExchangeH1, &clientconnect.P2pDataPlaneStats{}, provider.noAck, provider.packs, plan.resources.LogicalDataLaneCount, plan.resources.Features)
	provider.resendBudget = clientconnect.NewTransferMemoryBudget(2 * 1024 * 1024)
	settings.SendBufferSettings.ResendQueueBudget = provider.resendBudget
	settings.SendBufferSettings.ResendQueueRetainedByteAccounting = true
	settings.WebRtcSettings.IceServerUrls, settings.WebRtcSettings.IceServerPoolUrls = nil, nil
	settings.WebRtcSettings.UseLoopbackOnlyIceInterfaces = true
	apiUrl := fmt.Sprintf("http://%s:%d", self.environment.providerEdgeAddress, self.environment.providerApiPort)
	provider.client = clientconnect.NewClient(self.ctx, provider.id, clientconnect.NewApiOutOfBandControl(self.ctx, provider.strategy, byJwt, apiUrl), settings)
	platformSettings := fullTunPlatformSettings(self.environment.providerH3Port, clientconnect.TransportModeH1, provider.carrier, &clientconnect.PlatformTransportReceiveStats{}, newFullTunEndpointPlatformBudget())
	provider.transport = clientconnect.NewPlatformTransportWithTargetMode(self.ctx, provider.strategy, provider.client.RouteManager(), fmt.Sprintf("wss://%s:%d", self.environment.providerEdgeAddress, self.environment.providerH1Port), &clientconnect.ClientAuth{ByJwt: byJwt, InstanceId: clientconnect.NewId(), AppVersion: "perfvar"}, clientconnect.TransportModeH1, platformSettings)
	if !waitForPlatform(self.setupCtx, provider.transport) {
		return errors.New("provider H1 transport did not connect")
	}
	localSettings := clientconnect.DefaultLocalUserNatSettings()
	if self.scenario != nil {
		localSettings.TcpBufferSettings.DialContextSettings = &clientconnect.DialContextSettings{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			if native := self.native.Load(); native != nil {
				return native.dialProvider(ordinal, ctx, network, address)
			}
			return (&net.Dialer{}).DialContext(ctx, network, address)
		}}
	}
	provider.localNat = clientconnect.NewLocalUserNat(provider.client.Ctx(), clientId.String(), localSettings)
	natSettings := clientconnect.DefaultRemoteUserNatProviderSettings()
	natSettings.ReturnSendObserver = provider.returns.observe
	if self.scenario != nil {
		natSettings.SecurityPolicyGenerator = func(ctx context.Context, stats *clientconnect.SecurityPolicyStatsCollector) clientconnect.SecurityPolicy {
			return &sdkBrowsingProviderOriginPolicy{SecurityPolicy: clientconnect.DefaultProviderSecurityPolicyWithStats(ctx, stats), fixture: self}
		}
	}
	provider.remoteNat = clientconnect.NewRemoteUserNatProvider(provider.client, provider.localNat, natSettings)
	registered := make(chan error, 1)
	provider.client.ContractManager().SetProvideModesWithReturnTrafficWithAckCallback(map[protocol.ProvideMode]bool{protocol.ProvideMode_Public: true}, func(err error) {
		select {
		case registered <- err:
		default:
		}
	})
	select {
	case err := <-registered:
		if err == nil {
			provider.ready.Store(true)
		}
		return err
	case <-self.setupCtx.Done():
		return self.setupCtx.Err()
	}
}

// Public change subscriptions precede every read; their coalesced edge only
// wakes this fixture waiter, never the SDK's discovery or packet workers.
func (self *sdkBrowsingSetupFixture) waitInitial() (sdkBrowsingSetupSnapshot, error) {
	listener := &sdkBrowsingSetupListener{changed: make(chan struct{}, 1)}
	windowSub := self.device.AddWindowStatusChangeListener(listener)
	locationSub := self.device.AddConnectedProviderLocationChangeListener(listener)
	defer windowSub.Close()
	defer locationSub.Close()
	var snapshot sdkBrowsingSetupSnapshot
	for {
		snapshot = sdkBrowsingSetupSnapshotFrom(self.device.GetExits(), self.device.GetWindowStatus(), self.device.GetConnectedProviderLocations(), self.readyProviderIds())
		ready, err := sdkBrowsingSetupReady(snapshot, self.discovery.snapshot().InitialExcludedEmpty)
		if err != nil {
			return snapshot, err
		}
		if ready {
			if err := self.setupCtx.Err(); err != nil {
				return snapshot, err
			}
			return snapshot, nil
		}
		select {
		case <-self.setupCtx.Done():
			return snapshot, fmt.Errorf("SDK_BROWSING_SETUP_PREMISE: %s: %w", snapshot, self.setupCtx.Err())
		case <-listener.changed:
		case <-self.discovery.changed:
		}
	}
}

// Partial owners close before TestEnv can reset the database; the later cleanup
// is idempotent. SDK retirement retains its authority until its join completes.
func (self *sdkBrowsingSetupFixture) close() error {
	self.closeOnce.Do(func() {
		if self.scenario != nil {
			self.closeErr = errors.Join(self.closeErr, self.scenario.stop())
		}
		joinCtx, cancelJoin := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancelJoin()
		if native := self.native.Load(); native != nil {
			self.closeErr = errors.Join(self.closeErr, native.path.appTun.Close())
		}
		if self.device != nil {
			self.device.Cancel()
			if native := self.native.Load(); native != nil {
				select {
				case <-native.bridgeDone:
				case <-joinCtx.Done():
					self.closeErr = errors.Join(self.closeErr, errors.New("SDK native producer did not join"))
				}
			}
			err := self.device.CloseAndWait(joinCtx)
			if err != nil {
				sdkBrowsingCaptureFailedDeviceJoin(self.t, err, runtime.Stack)
			}
			self.closeErr = errors.Join(self.closeErr, err)
			if err == nil {
				self.afterJoin = self.device.MemoryUsed()
				memory := self.afterJoin
				if memory.TransferRootUsedByteCount != 0 || memory.TransferRootReservedByteCount != memory.TransferRootReleasedByteCount || memory.PlatformTransportUsedByteCount != 0 || memory.PlatformTransportUsedCount != 0 || memory.PlatformTransportPendingH1Count != 0 || memory.PlatformTransportPendingHandoffCount != 0 || memory.PlatformTransportActiveHandoffCount != 0 || memory.PlatformTransportReservedBytes != memory.PlatformTransportReleasedBytes {
					self.closeErr = errors.Join(self.closeErr, errors.New("public SDK owner custody remained after join"))
				}
			}
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
		for ordinal := range self.providers {
			provider := &self.providers[ordinal]
			if provider.remoteNat != nil {
				provider.remoteNat.Close()
			}
			if provider.localNat != nil {
				self.closeErr = errors.Join(self.closeErr, provider.localNat.CloseAndWait(joinCtx))
			}
			if provider.client != nil {
				self.closeErr = errors.Join(self.closeErr, closeClientAndOutOfBandWait(joinCtx, provider.client))
			}
			if provider.transport != nil {
				self.closeErr = errors.Join(self.closeErr, provider.transport.CloseAndWait(joinCtx))
			}
			if provider.strategy != nil {
				provider.strategy.Close()
			}
			if provider.carrier != nil {
				self.closeErr = errors.Join(self.closeErr, provider.carrier.Close())
			}
			if provider.noAck != nil {
				provider.noAck.close()
			}
			if provider.packs != nil {
				boundary, ok := provider.packs.boundary(joinCtx)
				if !ok || !provider.packs.waitThrough(joinCtx, boundary) {
					self.closeErr = errors.Join(self.closeErr, fmt.Errorf("provider %d Pack ownership did not join", ordinal))
				}
				provider.packs.close()
			}
			if provider.returns != nil {
				boundary, ok := provider.returns.lifecycleBoundary(joinCtx)
				if !ok || !provider.returns.waitLifecycleThrough(joinCtx, boundary) {
					self.closeErr = errors.Join(self.closeErr, fmt.Errorf("provider %d return ownership did not join", ordinal))
				}
				provider.returns.close()
			}
			if provider.resendBudget != nil {
				reserved, released := provider.resendBudget.Counts()
				if provider.resendBudget.UsedByteCount() != 0 || reserved != released {
					self.closeErr = errors.Join(self.closeErr, fmt.Errorf("provider %d retained retry bytes after join", ordinal))
				}
			}
		}
		if native := self.native.Load(); native != nil {
			if native.receiveUnsub != nil {
				native.receiveUnsub()
			}
			native.stateLock.Lock()
			remaining := len(native.upstreamConns)
			native.stateLock.Unlock()
			if remaining != 0 || native.bridgeInvalid.Load() || native.bridgeStarted.Load() != native.bridgeCompleted.Load() || native.bridgeCompleted.Load() != native.bridgeAccepted.Load()+native.bridgeRefused.Load() {
				self.closeErr = errors.Join(self.closeErr, errors.New("SDK native input or upstream socket ownership remained after join"))
			}
		}
		if self.environment != nil {
			self.environment.close()
		}
		self.cancelSetup()
		self.cancel()
	})
	return self.closeErr
}

// Five quality local owners and one speed owner fill the six slots from six
// initial physical authorities; installed multiplicity remains observational.
// Unpublished work remains unknown; full100 must recheck actual owners.
func TestFullTunH1Sdk32InitialPublicFullWindowSetup(t *testing.T) {
	if testing.Short() {
		t.Skip("actual SDK/H1/provider setup needs the shared PERFVAR environment")
	}
	environment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	environment.Run(t, func(t testing.TB) {
		fixture, err := newSdkBrowsingSetupFixture(t, newNativeBrowsingPressurePlan())
		if err != nil {
			t.Fatal("SDK_BROWSING_SETUP_CONSTRUCTION: ", err)
		}
		defer func() {
			if err := fixture.close(); err != nil {
				t.Error(err)
			}
		}()
		snapshot, err := fixture.waitInitial()
		if err != nil {
			t.Fatal(err)
		}
		if fixture.providers[4].client != nil || fixture.providers[4].ready.Load() {
			t.Fatal("future late provider was physically constructed during initial setup")
		}
		discovery := fixture.discovery.snapshot()
		if err := fixture.close(); err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(struct {
			GeometryContract       string                          `json:"geometry_contract"`
			Setup                  sdkBrowsingSetupSnapshot        `json:"setup"`
			Discovery              sdkBrowsingSetupDiscoveryReport `json:"discovery"`
			AfterJoin              *sdk.DeviceLocalMemoryUsage     `json:"owner_after_join"`
			UnpublishedOffersKnown bool                            `json:"unpublished_offers_known"`
			QuiescenceKnown        bool                            `json:"quiescence_known"`
			ObservedExcludedEmpty  bool                            `json:"observed_excluded_empty"`
			Full100Run             bool                            `json:"full100_run"`
		}{GeometryContract: sdkBrowsingTopology, Setup: snapshot, Discovery: discovery, AfterJoin: fixture.afterJoin, ObservedExcludedEmpty: discovery.InitialExcludedEmpty != nil})
		if err != nil || len(encoded) > 8*1024 {
			t.Fatalf("bounded public setup receipt: bytes=%d err=%v", len(encoded), err)
		}
		t.Logf("[perfvar] sdk-browsing-full-window-setup %s", encoded)
	})
}
