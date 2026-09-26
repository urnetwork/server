//go:build acklineagetrace

package perfvar

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
)

// This experiment is deliberately distinct from both the earlier direct-TUN
// screen and canonical PERFVAR. It uses the production remote DoH transport,
// MultiClient, both Transfer endpoints, fixed H1+, and the real provider NAT.
// Only the named timer settings vary. Both directions always retain Transfer
// ACKs; NoAck policy experiments were withdrawn and are not selectable here.
type dohTransferArm struct {
	Name                 string        `json:"name"`
	ApplicationTCPMaxRTO time.Duration `json:"application_tcp_max_rto_nanoseconds"`
	DeviceMaxResend      time.Duration `json:"device_max_resend_nanoseconds"`
	ProviderMaxResend    time.Duration `json:"provider_max_resend_nanoseconds"`
}

func dohTransferArmNamed(name string) (dohTransferArm, error) {
	arm := dohTransferArm{Name: name, ApplicationTCPMaxRTO: 8 * time.Second, DeviceMaxResend: 8 * time.Second, ProviderMaxResend: 8 * time.Second}
	switch name {
	case "ack-8-8":
	case "ack-3-3":
		arm.ApplicationTCPMaxRTO, arm.DeviceMaxResend, arm.ProviderMaxResend = 3*time.Second, 3*time.Second, 3*time.Second
	case "ack-5-5":
		arm.ApplicationTCPMaxRTO, arm.DeviceMaxResend, arm.ProviderMaxResend = 5*time.Second, 5*time.Second, 5*time.Second
	case "ack-3-8":
		arm.ApplicationTCPMaxRTO = 3 * time.Second
	case "ack-8-3":
		arm.DeviceMaxResend, arm.ProviderMaxResend = 3*time.Second, 3*time.Second
	default:
		return dohTransferArm{}, fmt.Errorf("unsupported DoH Transfer arm %q", name)
	}
	return arm, nil
}

type dohTransferScenario struct {
	Version           int                `json:"version"`
	Name              string             `json:"name"`
	DeviceAccess      networkProfile     `json:"device_access"`
	SetupDeviceAccess networkProfile     `json:"setup_device_access"`
	EstablishClean    bool               `json:"establish_outer_route_clean"`
	ProviderAccess    networkProfile     `json:"provider_access"`
	Resources         tunResourceProfile `json:"resources"`
	FaultRole         string             `json:"fault_role"`
	PayloadDrops      int64              `json:"payload_drops"`
	Reconnect         bool               `json:"reconnect"`
	RequestBudget     time.Duration      `json:"request_budget_nanoseconds"`
	CarrierMaxRTO     time.Duration      `json:"fixed_carrier_max_rto_nanoseconds"`
	CollapseMaxHold   time.Duration      `json:"fixed_tcp_collapse_max_hold_nanoseconds"`
	FaultPolicy       string             `json:"fault_policy"`
	SettingsScope     string             `json:"settings_scope"`
	// Omitted for the historical post-close scenarios so their exact hashes
	// and failed records remain unchanged. The new scenario is request-only.
	RequestBoundaryPolicy string `json:"request_boundary_policy,omitempty"`
}

func dohTransferScenarioNamed(name string) (dohTransferScenario, error) {
	baseName, role, reconnect := name, "device", false
	boundaryPolicy := ""
	switch name {
	case "established-rtt3s-keepalive", "established-rtt3s-keepalive-burst-1":
		baseName = strings.Replace(name, "-keepalive", "", 1)
		boundaryPolicy = dohTransferKeepaliveBoundary
	case "established-rtt3s-request-prefix", "established-rtt3s-request-prefix-burst-1":
		baseName = strings.Replace(name, "-request-prefix", "", 1)
		boundaryPolicy = dohTransferPrefixBoundary
	}
	establishClean := strings.HasPrefix(name, "established-rtt3s")
	if establishClean {
		baseName = strings.Replace(baseName, "established-rtt3s", "clean-rtt3s", 1)
	}
	if name == "carrier-reconnect" || name == "return-carrier-reconnect" {
		baseName, reconnect = "clean", true
	}
	if strings.HasPrefix(name, "return-") {
		role = "provider"
		if !reconnect {
			baseName = strings.TrimPrefix(name, "return-")
		}
	}
	base, err := tcpRecoveryScenarioNamed("doh", baseName)
	if err != nil {
		return dohTransferScenario{}, err
	}
	setupProfile := base.Profile
	if establishClean {
		setupProfile = initialNetworkProfiles(20260810)["clean-lan"]
	}
	scenario := dohTransferScenario{
		Version: 3, Name: name, DeviceAccess: base.Profile, SetupDeviceAccess: setupProfile, EstablishClean: establishClean,
		ProviderAccess: initialNetworkProfiles(20260810)["clean-lan"],
		Resources:      tunResourceProfile{ChannelSize: 128, TcpBufferDefault: 64 * 1024, TcpBufferMax: 64 * 1024, UdpBuffer: 32 * 1024, BatchSize: 8},
		FaultRole:      role, PayloadDrops: int64(base.ForcedPayloadDrops), Reconnect: reconnect,
		RequestBudget: 60 * time.Second, CarrierMaxRTO: 8 * time.Second, CollapseMaxHold: 1500 * time.Millisecond,
		FaultPolicy:           "query-flow payload route-attempt pins first eligible old-H1 TCP byte; drop first-N transmissions covering that same byte; later data/ACKs/new tuples excluded; reconnect closes old carrier sockets only",
		SettingsScope:         "application DoH TUN plus device/provider Transfer caps; both TCP IP directions ACK-required with bounded collapse prevention; fixed outer H1 carrier TUN8; no platform OS TCP setting",
		RequestBoundaryPolicy: boundaryPolicy,
	}
	if boundaryPolicy != "" {
		scenario.Version = 4
	}
	if boundaryPolicy == dohTransferPrefixBoundary {
		scenario.Version = 5
	}
	return scenario, nil
}

// This bounds the opt-in diagnostic selector, not any production setting or
// repeat count. Identity preflight must reject every run the launcher rejects;
// keeping a separate launch-only cap previously stopped a sealed cohort at 31.
const dohTransferMaxRun = 64

func validateDohTransferRun(run int) error {
	if run < 1 || dohTransferMaxRun < run {
		return fmt.Errorf("DoH Transfer experiment run must be 1..%d", dohTransferMaxRun)
	}
	return nil
}

func dohTransferTrace(scenario dohTransferScenario, run int) (perfvarTrace, error) {
	if err := validateDohTransferRun(run); err != nil {
		return perfvarTrace{}, err
	}
	return perfvarTraceForRun(perfvarScenario{
		Route: fullTunRouteExchangeH1, Workload: perfvarWorkload("diagnostic-doh-transfer-" + scenario.Name),
		Direction: perfvarDirectionDownload, Profile: scenario.DeviceAccess, ProviderAccessProfile: scenario.ProviderAccess,
		Resource: perfvarResourceMobile, PayloadByteCount: 4, RunCount: 3, FlowCount: 1,
	}, run)
}

func parseDohTransferRun(value string) (int, error) {
	run, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("invalid DoH Transfer experiment run: %w", err)
	}
	if err := validateDohTransferRun(run); err != nil {
		return 0, err
	}
	return run, nil
}

type dohTransferEffectiveSettings struct {
	ClientID    clientconnect.Id `json:"client_id"`
	MaxResend   time.Duration    `json:"max_resend_nanoseconds"`
	AckLifetime time.Duration    `json:"ack_lifetime_nanoseconds"`
}

type dohTransferEndpointObserver struct {
	wire                    dohTransferWireObserver
	h1                      clientconnect.H1ConnectionStats
	mu                      sync.Mutex
	settings                map[clientconnect.Id]dohTransferEffectiveSettings
	ackExpired              map[string]uint64
	overflow                atomic.Uint64
	independentPingFailures atomic.Uint64
}

func newDohTransferEndpointObserver(provider bool) *dohTransferEndpointObserver {
	return &dohTransferEndpointObserver{wire: dohTransferWireObserver{provider: provider}, settings: map[clientconnect.Id]dohTransferEffectiveSettings{}, ackExpired: map[string]uint64{}}
}

func (self *dohTransferEndpointObserver) configure(settings *clientconnect.ClientSettings, cap time.Duration) {
	settings.SendBufferSettings.MaxResendInterval = cap
	priorWire := settings.SendBufferSettings.TransferWireMessageObserver
	settings.SendBufferSettings.TransferWireMessageObserver = self.wire.observe
	if priorWire != nil {
		settings.SendBufferSettings.TransferWireMessageObserver = func(event clientconnect.TransferWireMessageObservation) {
			self.wire.observe(event)
			priorWire(event)
		}
	}
	prior := settings.SendBufferSettings.SendPackLifecycleObserver
	settings.SendBufferSettings.SendPackLifecycleObserver = func(event clientconnect.SendPackLifecycleObservation) {
		if dohTransferIndependentPingFailure(event) {
			self.independentPingFailures.Add(1)
		}
		// This event occurs after NewClient and the MultiClient AckTimeout
		// override. Reading the effective value here avoids calling the raw
		// generator's 60s setting the active device's 30s lifetime.
		self.mu.Lock()
		if _, present := self.settings[event.ClientId]; !present {
			if len(self.settings) < 64 {
				self.settings[event.ClientId] = dohTransferEffectiveSettings{event.ClientId, settings.SendBufferSettings.MaxResendInterval, settings.SendBufferSettings.AckTimeout}
			} else {
				self.overflow.Add(1)
			}
		}
		self.mu.Unlock()
		if prior != nil {
			prior(event)
		}
	}
	logger := settings.Log
	if logger == nil {
		logger = clientconnect.DefaultLogger()
	}
	settings.Log = &dohTransferExitLogger{Logger: logger, observer: self}
}

type dohTransferExitLogger struct {
	clientconnect.Logger
	observer *dohTransferEndpointObserver
}

func (self *dohTransferExitLogger) Infof(format string, args ...any) {
	if format == h1FailureExitFormat && len(args) == 29 && args[0] == "ack_lifetime" {
		if id, ok := args[1].(string); ok {
			self.observer.mu.Lock()
			if len(self.observer.ackExpired) < 64 {
				self.observer.ackExpired[id]++
			} else {
				self.observer.overflow.Add(1)
			}
			self.observer.mu.Unlock()
		}
	}
	self.Logger.Infof(format, args...)
}

func (self *dohTransferEndpointObserver) snapshot(id clientconnect.Id) (dohTransferEffectiveSettings, uint64) {
	self.mu.Lock()
	defer self.mu.Unlock()
	return self.settings[id], self.ackExpired[id.String()]
}

type dohTransferFaultObservation struct {
	Role               string                    `json:"role"`
	OldConnectionCount int                       `json:"old_connection_count"`
	PayloadDrops       int64                     `json:"payload_drops"`
	PayloadDropBytes   int64                     `json:"payload_drop_bytes"`
	ClosedCarriers     int64                     `json:"closed_carriers"`
	FirstDropNS        int64                     `json:"first_drop_ns"`
	ClosedNS           int64                     `json:"closed_ns"`
	NativeDialsBefore  uint64                    `json:"native_dials_before"`
	NativeDialsAfter   uint64                    `json:"native_dials_after"`
	Target             *dohTransferCarrierTarget `json:"target,omitempty"`
}

// Endpoint-wide NoAck route-write accounting remains separate from the exact
// query-flow pre-write wire observer and from peer delivery/TCP correctness.
type dohTransferNoAckRouteWrites struct {
	Started, Completed, Failed uint64
	Invalid                    bool
}

func dohTransferNoAckSnapshot(tracker *noAckSendTracker) dohTransferNoAckRouteWrites {
	return dohTransferNoAckRouteWrites{tracker.startedCount.Load(), tracker.completedCount.Load(), tracker.failureCount.Load(), tracker.invalid.Load()}
}

func dohTransferNoAckDelta(after, before dohTransferNoAckRouteWrites) dohTransferNoAckRouteWrites {
	return dohTransferNoAckRouteWrites{after.Started - before.Started, after.Completed - before.Completed, after.Failed - before.Failed, after.Invalid || before.Invalid}
}

type dohTransferRecord struct {
	RecordType                                                    string                       `json:"record_type"`
	ScenarioHash                                                  string                       `json:"scenario_hash"`
	ProfileHash                                                   string                       `json:"profile_hash"`
	Scenario                                                      dohTransferScenario          `json:"scenario"`
	Arm                                                           dohTransferArm               `json:"arm"`
	Trace                                                         perfvarTrace                 `json:"trace"`
	RunIndex                                                      int                          `json:"run_index"`
	Host                                                          perfvarHostMetadata          `json:"host"`
	Correct                                                       bool                         `json:"correct"`
	DNSCorrect                                                    bool                         `json:"dns_correct"`
	OriginalStreamCorrect                                         bool                         `json:"original_stream_correct"`
	FailureReason                                                 string                       `json:"failure_reason,omitempty"`
	InvalidReason                                                 string                       `json:"invalid_reason,omitempty"`
	SetupDuration                                                 time.Duration                `json:"setup_duration_nanoseconds"`
	Workload                                                      workloadResult               `json:"workload"`
	DeviceSettings                                                dohTransferEffectiveSettings `json:"device_settings"`
	ProviderSettings                                              dohTransferEffectiveSettings `json:"provider_settings"`
	DeviceReadTimeout, ProviderReadTimeout                        time.Duration
	ReadAlignedBlackholeDefault                                   time.Duration
	DeviceWire                                                    dohTransferWireSnapshot `json:"device_wire_attempts"`
	ProviderWire                                                  dohTransferWireSnapshot `json:"provider_wire_attempts"`
	RequestDeviceWire                                             dohTransferWireSnapshot `json:"request_device_wire_attempts"`
	RequestProviderWire                                           dohTransferWireSnapshot `json:"request_provider_wire_attempts"`
	DeviceNoAckRouteWrites, ProviderNoAckRouteWrites              dohTransferNoAckRouteWrites
	DeviceAckExpiries, ProviderAckExpiries                        uint64
	TcpCollapseDrops                                              uint64                       `json:"tcp_collapse_drops"`
	ProfileTransition                                             []networkProfileUpdateResult `json:"profile_transition,omitempty"`
	TransitionCarrierDialsBefore, TransitionCarrierDialsAfter     uint64
	AppliedAccessProfiles                                         map[string]linkProfile `json:"applied_access_profiles,omitempty"`
	DevicePackFailures, ProviderPackFailures                      uint64
	DeviceWorkloadPackFailures, ProviderWorkloadPackFailures      uint64
	DeviceIndependentPingFailures                                 uint64 `json:"device_independent_ping_failures,omitempty"`
	ProviderIndependentPingFailures                               uint64 `json:"provider_independent_ping_failures,omitempty"`
	DeviceFailureSamples, ProviderFailureSamples                  []string
	DeviceH1, ProviderH1                                          clientconnect.H1ConnectionStatsSnapshot
	Carrier                                                       perfvarCarrierObservation `json:"carrier"`
	ApplicationTCP, DeviceCarrierTCP, ProviderCarrierTCP, EdgeTCP h1FailureTCPStackCounters
	TCPInfoBefore, TCPInfoAfter                                   tcpip.TCPInfoOption
	Fault                                                         dohTransferFaultObservation `json:"fault"`
	DeviceIDBefore, DeviceIDAfter, ProviderID                     clientconnect.Id
	DeviceIdentitySourceBefore, DeviceIdentitySourceAfter         string
	InnerLocal, InnerRemote                                       string
	ApplicationTUNOwned                                           bool
	RequestOwnershipBoundaryComplete                              bool
	ServerAccepted, ServerHTTP2Requests                           int64
	Memory                                                        tcpRecoveryMemory                    `json:"memory"`
	BaselineEligible                                              bool                                 `json:"baseline_eligible"`
	Cleanup                                                       *dohTransferCleanupObservation       `json:"post_request_cleanup,omitempty"`
	CarrierPrefix                                                 *dohTransferCarrierPrefixObservation `json:"request_carrier_prefix,omitempty"`
}

func beginDohTransferMemory() func(*tcpRecoveryMemory) {
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	initialRSS, goroutines := tcpRecoveryMaxRSS(), runtime.NumGoroutine()
	var peak atomic.Uint64
	peak.Store(before.HeapAlloc)
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				var sample runtime.MemStats
				runtime.ReadMemStats(&sample)
				peak.Store(max(peak.Load(), sample.HeapAlloc))
			}
		}
	}()
	return func(result *tcpRecoveryMemory) {
		close(stop)
		<-done
		var after runtime.MemStats
		runtime.ReadMemStats(&after)
		*result = tcpRecoveryMemory{HeapBeforeBytes: before.HeapAlloc, HeapAfterBytes: after.HeapAlloc,
			HeapPeakBytes: max(peak.Load(), after.HeapAlloc), AllocatedBytes: after.TotalAlloc - before.TotalAlloc,
			Allocations: after.Mallocs - before.Mallocs, GarbageCollections: after.NumGC - before.NumGC,
			ProcessMaxRSSBytes: tcpRecoveryMaxRSS(), InitialMaxRSSBytes: initialRSS,
			GoroutinesBefore: goroutines, GoroutinesAfter: runtime.NumGoroutine()}
	}
}

func installDohTransferFault(ctx context.Context, path *fullTunPath, scenario dohTransferScenario, recorder *h1FailureTCPRecorder, observer *dohTransferWireObserver) (func() dohTransferFaultObservation, error) {
	result := dohTransferFaultObservation{Role: scenario.FaultRole, NativeDialsBefore: recorder.retained.Load()}
	if scenario.PayloadDrops == 0 && !scenario.Reconnect {
		return func() dohTransferFaultObservation { result.NativeDialsAfter = recorder.retained.Load(); return result }, nil
	}
	node := path.deviceCarrierNode
	if scenario.FaultRole == "provider" {
		var ok bool
		node, ok = path.environment.network.nodeNameForTun(path.providerCarrierTun)
		if !ok {
			return nil, fmt.Errorf("provider carrier TUN has no node")
		}
	}
	var connections []net.Conn
	fault := &dohTransferCarrierFault{limit: scenario.PayloadDrops, first: make(chan struct{})}
	for entry := recorder.head.Load(); entry != nil; entry = entry.next {
		if entry.node != node {
			continue
		}
		info, err := entry.reader.TcpInfo()
		if err != nil || tcp.EndpointState(info.State) != tcp.StateEstablished {
			continue
		}
		tuple, err := dohTransferConnTuple(entry.connection)
		if err != nil {
			return nil, err
		}
		fault.tuples = append(fault.tuples, tuple)
		connections = append(connections, entry.connection)
	}
	result.OldConnectionCount = len(connections)
	if len(connections) == 0 || recorder.overflow.Load() != 0 || recorder.unsupported.Load() != 0 {
		return nil, fmt.Errorf("exact old native H1 ownership unavailable: count=%d overflow=%d unsupported=%d", len(connections), recorder.overflow.Load(), recorder.unsupported.Load())
	}
	path.environment.network.stateLock.Lock()
	link := path.environment.network.links[tunLinkKey{source: node, destination: "edge"}]
	path.environment.network.stateLock.Unlock()
	if link == nil || link.forcedLossForTest.Load() != nil {
		return nil, fmt.Errorf("missing or already owned native H1 loss link for %s", node)
	}
	if scenario.Reconnect {
		fault.limit = 4096 // fail closed if the bounded close handoff never runs
	}
	link.forcedLossForTest.Store(&linkLossTestHook{drop: fault.drop})
	observer.fault.Store(fault)
	stop, done := make(chan struct{}), make(chan struct{})
	var closed, closedNS atomic.Int64
	go func() {
		defer close(done)
		if !scenario.Reconnect {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case <-fault.first:
			for _, connection := range connections {
				_ = connection.Close()
				closed.Add(1)
			}
			closedNS.Store(time.Now().UnixNano())
		}
	}()
	return func() dohTransferFaultObservation {
		observer.fault.Store(nil)
		link.forcedLossForTest.Store(nil)
		close(stop)
		<-done
		result.PayloadDrops, result.PayloadDropBytes = fault.drops.Load(), fault.bytes.Load()
		result.FirstDropNS, result.ClosedNS, result.ClosedCarriers = fault.firstNS.Load(), closedNS.Load(), closed.Load()
		result.NativeDialsAfter = recorder.retained.Load()
		if target := fault.target.Load(); target != nil {
			value := *target
			result.Target = &value
		}
		return result
	}, nil
}

func measureDohTransferExperiment(ctx context.Context, t testing.TB, scenario dohTransferScenario, arm dohTransferArm, run int) (record dohTransferRecord) {
	record = dohTransferRecord{RecordType: "diagnostic-doh-transfer-recovery", Scenario: scenario, Arm: arm, RunIndex: run,
		ScenarioHash: tcpRecoveryHash(scenario), ProfileHash: tcpRecoveryHash([]networkProfile{scenario.DeviceAccess, scenario.ProviderAccess})}
	finishMemory := beginDohTransferMemory()
	defer func() { finishMemory(&record.Memory) }()
	trace, err := dohTransferTrace(scenario, run)
	if err != nil {
		record.InvalidReason = err.Error()
		return
	}
	record.Trace = trace
	if clientconnect.DefaultTunSettingsWithBufferSize(4096).TcpMaxRto != scenario.CarrierMaxRTO {
		record.InvalidReason = "outer carrier TUN default changed from pinned 8s"
		return
	}
	setup := time.Now()
	profile, setupProfile, providerProfile := scenario.DeviceAccess, scenario.SetupDeviceAccess, scenario.ProviderAccess
	profile.Seed, setupProfile.Seed, providerProfile.Seed = trace.ApplicationOrDirectSeed, trace.ApplicationOrDirectSeed, trace.ProviderSeed
	environment := newRouteEnvironmentWithNetworkPeers(ctx, t, setupProfile, false)
	defer environment.close()
	if scenario.RequestBoundaryPolicy == dohTransferPrefixBoundary {
		if err := environment.network.enablePacketFences(); err != nil {
			record.InvalidReason = err.Error()
			return
		}
	}
	environment.deviceAccessProfile, environment.providerAccessProfile = setupProfile, providerProfile
	device, provider := newDohTransferEndpointObserver(false), newDohTransferEndpointObserver(true)
	var native h1FailureTCPRecorder
	var providerReadTimeout atomic.Int64
	hooks := &fullTunConstructionTestHooks{
		configureDeviceClientSettings:   func(settings *clientconnect.ClientSettings) { device.configure(settings, arm.DeviceMaxResend) },
		configureProviderClientSettings: func(settings *clientconnect.ClientSettings) { provider.configure(settings, arm.ProviderMaxResend) },
		configureDevicePlatformSettings: func(settings *clientconnect.PlatformTransportSettings) { settings.H1ConnectionStats = &device.h1 },
		configureProviderPlatformSettings: func(settings *clientconnect.PlatformTransportSettings) {
			settings.H1ConnectionStats = &provider.h1
			providerReadTimeout.Store(int64(settings.ReadTimeout))
		},
		configureApplicationTunSettings: func(settings *clientconnect.TunSettings) {
			settings.TcpMaxRto, settings.DialRace = arm.ApplicationTCPMaxRTO, 1
		},
		afterStage: func(stage fullTunConstructionStage, path *fullTunPath) error {
			if stage == fullTunConstructionStageProviderCarrierTun {
				environment.clientDialObserverForTest.Store(&routeClientDialObserver{observe: func(node string, _ *clientconnect.Tun, connection net.Conn) {
					native.observeDial(node, connection, environment.h1Port, environment.providerH1Port)
				}})
			}
			return nil
		},
	}
	path, err := tryNewFullTunPathWithTopologyHooks(ctx, t, environment, fullTunRouteExchangeH1, false, scenario.Resources, 1, hooks)
	if err != nil {
		record.FailureReason = "route setup: " + err.Error()
		return
	}
	if scenario.RequestBoundaryPolicy == dohTransferKeepaliveBoundary || scenario.RequestBoundaryPolicy == dohTransferPrefixBoundary {
		record.Cleanup = &dohTransferCleanupObservation{}
		defer func() {
			joinCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			captureDohTransferCleanup(record.Cleanup, func() error { return path.closeAndWait(joinCtx) }, func() dohTransferCleanupCounters {
				return dohTransferCleanupCounters{
					DevicePackFailures: path.devicePackSends.failures.Load(), ProviderPackFailures: path.providerPackSends.failures.Load(),
					DeviceWorkloadFailures: path.devicePackSends.workloadFailures.Load(), ProviderWorkloadFailures: path.providerPackSends.workloadFailures.Load(),
					DeviceTrackerInvalid: path.devicePackSends.invalid.Load(), ProviderTrackerInvalid: path.providerPackSends.invalid.Load(),
				}
			})
			if record.Cleanup.Error != "" {
				t.Errorf("full-TUN teardown: %s", record.Cleanup.Error)
			}
		}()
	} else {
		defer path.close()
	}
	if !fullTunMultiClientSettings(path).TcpCollapsePrevention || path.multiClient.ReliabilitySettings().TcpCollapseMaxHold != scenario.CollapseMaxHold {
		record.InvalidReason = "bounded TCP collapse prevention changed from declared fixed policy"
		return
	}
	if scenario.EstablishClean {
		// Cold 3s RTT cannot fit the production 5s outer TCP+TLS handshake.
		// This separately named scenario retains that established H1 owner,
		// then warms the real inner DoH connection at the target RTT. It does
		// not extend handshakes or replace the cold-setup failure evidence.
		node, ok := environment.network.nodeNameForTun(path.deviceCarrierTun)
		if !ok {
			record.InvalidReason = "established-route transition has no exact device carrier node"
			return
		}
		before := environment.network.snapshotProfiles()
		record.TransitionCarrierDialsBefore = native.retained.Load()
		record.ProfileTransition, err = environment.network.updateNodeProfiles(ctx, node, "established-doh-access", time.Now(), &profile.Forward, &profile.Reverse)
		if err != nil || len(record.ProfileTransition) != 2 {
			record.InvalidReason = fmt.Sprintf("established-route transition: links=%d error=%v", len(record.ProfileTransition), err)
			return
		}
		record.AppliedAccessProfiles = environment.network.snapshotProfiles()
		if err := validateDohTransferAccessTransition(node, before, record.AppliedAccessProfiles, profile); err != nil {
			record.InvalidReason = err.Error()
			return
		}
		environment.deviceAccessProfile = profile
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0") // provider-side origin, never the client dialer
	if err != nil {
		record.FailureReason = err.Error()
		return
	}
	port := uint32(listener.Addr().(*net.TCPAddr).Port)
	device.wire.port.Store(port)
	provider.wire.port.Store(port)
	op, err := newTCPRecoveryDohWithListener(path.appTun, listener)
	if err != nil {
		record.FailureReason = err.Error()
		return
	}
	defer func() {
		if record.Cleanup != nil {
			record.Cleanup.Before = dohTransferCleanupCounters{
				DevicePackFailures: path.devicePackSends.failures.Load(), ProviderPackFailures: path.providerPackSends.failures.Load(),
				DeviceWorkloadFailures: path.devicePackSends.workloadFailures.Load(), ProviderWorkloadFailures: path.providerPackSends.workloadFailures.Load(),
			}
		}
		op.close()
		if record.Cleanup != nil {
			record.Cleanup.CacheCloseReturned = true
		}
	}()
	if err := op.warm(ctx); err != nil {
		record.FailureReason = "warm DoH: " + err.Error()
		// Explicit diagnostic only, before deferred owner teardown. The normal
		// success path does not sample stacks or add another packet observer.
		dumpDohWarmFailure(t, os.Getenv("CONNECT_PERFVAR_DOH_WARM_FAILURE_SNAPSHOT"), record, path, &native, device, provider, op)
		return
	}
	if scenario.EstablishClean {
		record.TransitionCarrierDialsAfter = native.retained.Load()
		if record.TransitionCarrierDialsBefore == 0 || record.TransitionCarrierDialsAfter != record.TransitionCarrierDialsBefore {
			record.InvalidReason = "established-route RTT warmup replaced a carrier instead of retaining it"
			return
		}
	}
	raw := op.raw.Load()
	if raw == nil {
		record.InvalidReason = "remote Tun.DohCache request did not own a native TunTcpConn"
		return
	}
	record.InnerLocal, record.InnerRemote = raw.LocalAddr().String(), raw.RemoteAddr().String()
	local := raw.LocalAddr().(*net.TCPAddr).AddrPort().Addr().Unmap()
	if local != path.appTun.LocalAddresses()[0].Unmap() || raw.RemoteAddr().(*net.TCPAddr).Port != int(port) {
		record.InvalidReason = "DoH connection did not originate in the exact application TUN"
		return
	}
	record.ApplicationTUNOwned = true
	activeClient, activeID, _, identitySource := activeH3FullTunDeviceClient(path)
	record.DeviceIDBefore, record.DeviceIdentitySourceBefore = activeID, identitySource
	var readTimeoutKnown bool
	if path.apiGenerator != nil {
		record.DeviceReadTimeout, readTimeoutKnown = path.apiGenerator.ClientReadTimeout(activeClient)
	}
	record.ProviderReadTimeout = time.Duration(providerReadTimeout.Load())
	record.ReadAlignedBlackholeDefault = clientconnect.DefaultMultiClientSettings().BlackholeTimeout
	if !readTimeoutKnown || record.DeviceReadTimeout != 30*time.Second || record.ProviderReadTimeout != 30*time.Second || record.ReadAlignedBlackholeDefault != record.DeviceReadTimeout {
		record.InvalidReason = "effective H1 read timeout/no-send-ACK default differs from pinned 30s"
		return
	}
	record.ProviderID = path.providerClientId
	record.DeviceSettings, _ = device.snapshot(record.DeviceIDBefore)
	record.ProviderSettings, _ = provider.snapshot(path.providerClientId)
	if record.DeviceIdentitySourceBefore != "flow" || record.DeviceSettings.AckLifetime != 30*time.Second || record.ProviderSettings.AckLifetime != 60*time.Second || record.DeviceSettings.MaxResend != arm.DeviceMaxResend || record.ProviderSettings.MaxResend != arm.ProviderMaxResend {
		record.InvalidReason = "effective endpoint identity/cap/lifetime differs from declared arm"
		return
	}
	boundary, err := beginPerfvarCarrierMeasurement(path)
	if err != nil {
		record.InvalidReason = "start boundary: " + err.Error()
		return
	}
	record.SetupDuration = time.Since(setup)
	record.TCPInfoBefore, _ = raw.TcpInfo()
	deviceWire, providerWire := device.wire.snapshot(), provider.wire.snapshot()
	deviceNoAck, providerNoAck := dohTransferNoAckSnapshot(path.deviceNoAckSends), dohTransferNoAckSnapshot(path.providerNoAckSends)
	appTCP, deviceTCP, providerTCP, edgeTCP := h1FailureTCPStats(path.appTun), h1FailureTCPStats(path.deviceCarrierTun), h1FailureTCPStats(path.providerCarrierTun), h1FailureTCPStats(environment.edgeTun)
	df, pf := path.devicePackSends.failures.Load(), path.providerPackSends.failures.Load()
	dip, pip := device.independentPingFailures.Load(), provider.independentPingFailures.Load()
	dwf, pwf := path.devicePackSends.workloadFailures.Load(), path.providerPackSends.workloadFailures.Load()
	_, deviceExpired := device.snapshot(record.DeviceIDBefore)
	_, providerExpired := provider.snapshot(record.ProviderID)
	collapseBefore := path.multiClient.TcpCollapseDropCount()
	faultObserver := &device.wire
	if scenario.FaultRole == "provider" {
		faultObserver = &provider.wire
	}
	finishFault, err := installDohTransferFault(ctx, path, scenario, &native, faultObserver)
	if err != nil {
		record.InvalidReason = err.Error()
		return
	}
	requestCtx, cancel := context.WithTimeout(ctx, scenario.RequestBudget)
	started := time.Now()
	var first atomic.Int64
	useful, hash, requestErr := op.request(requestCtx, &first, 1)
	duration := time.Since(started)
	cancel()
	record.Fault = finishFault()
	record.DNSCorrect = requestErr == nil
	if requestErr != nil {
		record.FailureReason = requestErr.Error()
	}
	record.OriginalStreamCorrect = record.DNSCorrect && op.raw.Load() == raw && op.accepted.Load() == 1 && op.http2.Load() == 2
	_, record.DeviceIDAfter, _, record.DeviceIdentitySourceAfter = activeH3FullTunDeviceClient(path)
	record.Correct = record.OriginalStreamCorrect && record.DeviceIDAfter == record.DeviceIDBefore && record.ProviderID == path.providerClientId
	if record.DNSCorrect && !record.Correct {
		record.FailureReason = "answer arrived only after inner connection/client identity changed; not original-stream correctness"
	}
	record.TCPInfoAfter, _ = raw.TcpInfo()
	record.DeviceH1, record.ProviderH1 = device.h1.Snapshot(), provider.h1.Snapshot()
	record.ServerAccepted, record.ServerHTTP2Requests = op.accepted.Load(), op.http2.Load()
	// Seal application evidence before closing the connection: a later FIN or
	// pure TCP ACK must not stand in for actual ACK-required DNS payload.
	record.RequestDeviceWire = dohTransferWireDelta(device.wire.snapshot(), deviceWire)
	record.RequestProviderWire = dohTransferWireDelta(provider.wire.snapshot(), providerWire)
	var firstByte time.Duration
	if first.Load() > 0 {
		firstByte = time.Unix(0, first.Load()).Sub(started)
	}
	record.Workload = finishWorkloadResult(workloadResult{UsefulByteCount: useful, ContentHash: hash, Duration: duration, TimeToFirstByte: firstByte, Latency: summarizeLatencies([]time.Duration{duration})})
	joinRequest := path.waitForPostWorkloadBoundary
	if scenario.RequestBoundaryPolicy == dohTransferPrefixBoundary {
		record.CarrierPrefix = &dohTransferCarrierPrefixObservation{}
		joinRequest = func(joinCtx context.Context) error {
			return joinDohTransferCarrierPrefix(joinCtx, path, record.CarrierPrefix)
		}
	}
	if err := joinDohTransferRequestBoundary(ctx, scenario.RequestBoundaryPolicy, op.retire, joinRequest); err != nil {
		record.InvalidReason = "terminal ownership boundary: " + err.Error()
		dumpDohTerminalFailure(t, os.Getenv("CONNECT_PERFVAR_DOH_TERMINAL_FAILURE_SNAPSHOT"), record, path, &native, device, provider, op)
	} else {
		record.RequestOwnershipBoundaryComplete = true
	}
	record.Carrier = observePerfvarWorkloadCarrier(path, boundary)
	record.DeviceWire, record.ProviderWire = dohTransferWireDelta(device.wire.snapshot(), deviceWire), dohTransferWireDelta(provider.wire.snapshot(), providerWire)
	record.DeviceNoAckRouteWrites = dohTransferNoAckDelta(dohTransferNoAckSnapshot(path.deviceNoAckSends), deviceNoAck)
	record.ProviderNoAckRouteWrites = dohTransferNoAckDelta(dohTransferNoAckSnapshot(path.providerNoAckSends), providerNoAck)
	record.ApplicationTCP, record.DeviceCarrierTCP = tcpRecoveryStatsDelta(h1FailureTCPStats(path.appTun), appTCP), tcpRecoveryStatsDelta(h1FailureTCPStats(path.deviceCarrierTun), deviceTCP)
	record.ProviderCarrierTCP, record.EdgeTCP = tcpRecoveryStatsDelta(h1FailureTCPStats(path.providerCarrierTun), providerTCP), tcpRecoveryStatsDelta(h1FailureTCPStats(environment.edgeTun), edgeTCP)
	record.DevicePackFailures, record.ProviderPackFailures = path.devicePackSends.failures.Load()-df, path.providerPackSends.failures.Load()-pf
	record.DeviceIndependentPingFailures, record.ProviderIndependentPingFailures = device.independentPingFailures.Load()-dip, provider.independentPingFailures.Load()-pip
	record.DeviceWorkloadPackFailures, record.ProviderWorkloadPackFailures = path.devicePackSends.workloadFailures.Load()-dwf, path.providerPackSends.workloadFailures.Load()-pwf
	_, expired := device.snapshot(record.DeviceIDBefore)
	record.DeviceAckExpiries = expired - deviceExpired
	_, expired = provider.snapshot(record.ProviderID)
	record.ProviderAckExpiries = expired - providerExpired
	record.TcpCollapseDrops = path.multiClient.TcpCollapseDropCount() - collapseBefore
	for _, event := range path.devicePackSends.failureSnapshot() {
		record.DeviceFailureSamples = append(record.DeviceFailureSamples, fmt.Sprintf("client=%s type=%s ack=%t probe=%t error=%v", event.ClientId, event.MessageType, event.AckRequired, event.HealthProbe, event.Err))
	}
	for _, event := range path.providerPackSends.failureSnapshot() {
		record.ProviderFailureSamples = append(record.ProviderFailureSamples, fmt.Sprintf("client=%s type=%s ack=%t probe=%t error=%v", event.ClientId, event.MessageType, event.AckRequired, event.HealthProbe, event.Err))
	}
	if native.overflow.Load()+native.unsupported.Load()+device.overflow.Load()+provider.overflow.Load() > 0 || record.DeviceWire.Malformed+record.ProviderWire.Malformed+record.DeviceWire.WrongDirection+record.ProviderWire.WrongDirection > 0 || record.DeviceNoAckRouteWrites.Invalid || record.ProviderNoAckRouteWrites.Invalid {
		record.InvalidReason = "bounded native/wire observer incomplete or direction mismatch"
	}
	if invalid := validateDohTransferEvidence(record); invalid != "" {
		record.InvalidReason = strings.TrimPrefix(record.InvalidReason+"; "+invalid, "; ")
	}
	if record.DeviceWorkloadPackFailures > 0 || record.ProviderWorkloadPackFailures > 0 || record.DeviceAckExpiries > 0 || record.ProviderAckExpiries > 0 {
		record.Correct = false
		record.FailureReason += "; workload Pack/ACK failure retained separately from DNS correctness"
	}
	return
}

// A live RTT transition must touch exactly the two device-access links. In
// particular, a prefix-colliding node and the provider route remain untouched.
func validateDohTransferAccessTransition(node string, before, after map[string]linkProfile, target networkProfile) error {
	if node == "" || len(before) != len(after) {
		return fmt.Errorf("established-route transition changed topology")
	}
	selected := 0
	for name, old := range before {
		want := old
		switch {
		case strings.HasPrefix(name, node+"->"):
			want = target.Forward
			selected++
		case strings.HasSuffix(name, "->"+node):
			want = target.Reverse
			selected++
		}
		if got, present := after[name]; !present || got != want {
			return fmt.Errorf("established-route transition applied wrong profile to %s", name)
		}
	}
	if selected != 2 {
		return fmt.Errorf("established-route transition selected %d device links, want 2", selected)
	}
	return nil
}

func validateDohTransferEvidence(record dohTransferRecord) string {
	if !record.DNSCorrect {
		return "" // Retain failed requests; missing successful delivery is not missing instrumentation.
	}
	var invalid []string
	device, provider := record.RequestDeviceWire, record.RequestProviderWire
	if !record.ApplicationTUNOwned || device.PayloadFrames == 0 || provider.PayloadFrames == 0 {
		invalid = append(invalid, "DoH answer lacks exact application-TUN and bidirectional query payload evidence")
	}
	if provider.NoAckPacks != 0 || provider.AckPayloadPacks == 0 || device.NoAckPacks != 0 || device.AckPayloadPacks == 0 {
		invalid = append(invalid, "TCP DoH payload bypassed required bidirectional Transfer ACK policy")
	}
	if record.Scenario.EstablishClean && len(record.ProfileTransition) != 2 {
		invalid = append(invalid, "established-route measurement lacks both directional profile transitions")
	}
	if record.DeviceH1.H1PlusConnectionCount <= 0 || record.ProviderH1.H1PlusConnectionCount != 1 || record.DeviceH1.WebSocketConnectionCount+record.ProviderH1.WebSocketConnectionCount != 0 {
		invalid = append(invalid, "DoH did not retain the fixed H1+ route")
	}
	if !record.Scenario.Reconnect && record.Fault.PayloadDrops != record.Scenario.PayloadDrops || record.Scenario.Reconnect &&
		(record.Fault.PayloadDrops == 0 || record.Fault.ClosedCarriers == 0 || record.Fault.NativeDialsAfter <= record.Fault.NativeDialsBefore) {
		invalid = append(invalid, "successful request did not exercise the exact declared carrier fault")
	}
	return strings.Join(invalid, "; ")
}

func TestDohTransferRecoveryExperimentReplay(t *testing.T) {
	name := os.Getenv("CONNECT_PERFVAR_DOH_TRANSFER_ARM")
	if name == "" {
		t.Skip("explicit production-shaped DoH Transfer experiment selector required")
	}
	if os.Getenv("URNETWORK_ACK_LINEAGE_REPLAY_SLOT") != "granted" || runtime.GOMAXPROCS(0) != 8 || !ackLineageReplayRuntimeSupported(runtime.Version(), runtime.GOOS, runtime.GOARCH, 8) || perfvarRaceEnabled || perfvarProgressTraceEnabled() || clientconnect.DefaultLogger().V(1).Enabled() {
		t.Fatal("DoH Transfer experiment requires the owned CPU8 non-race V0 host and no per-packet progress logger")
	}
	arm, err := dohTransferArmNamed(name)
	if err != nil {
		t.Fatal(err)
	}
	scenario, err := dohTransferScenarioNamed(os.Getenv("CONNECT_PERFVAR_DOH_TRANSFER_PROFILE"))
	if err != nil {
		t.Fatal(err)
	}
	run, err := parseDohTransferRun(os.Getenv("CONNECT_PERFVAR_DOH_TRANSFER_RUN"))
	if err != nil {
		t.Fatal(err)
	}
	host := currentPerfvarHostMetadata()
	if err := validatePerfvarHostMetadata(host); err != nil {
		t.Fatal(err)
	}
	host.MeasurementKind = "diagnostic-production-shaped-doh-transfer"
	var emitted atomic.Uint64
	environment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	environment.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		record := measureDohTransferExperiment(ctx, t, scenario, arm, run)
		record.Host = host
		encoded, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("[perfvar-doh-transfer] %s", encoded)
		emitted.Add(1)
		if record.InvalidReason != "" {
			t.Errorf("invalid DoH Transfer experiment: %s", record.InvalidReason)
		}
		if !record.Correct {
			t.Errorf("incorrect DoH Transfer experiment: %s", record.FailureReason)
		}
	})
	if emitted.Load() != 1 {
		t.Fatalf("DoH Transfer launcher emitted %d/1 records; no-op/rerun is not evidence", emitted.Load())
	}
}

func TestDohTransferRecoveryIdentityAndScope(t *testing.T) {
	middle, err := dohTransferArmNamed("ack-5-5")
	if err != nil || middle.ApplicationTCPMaxRTO != 5*time.Second ||
		middle.DeviceMaxResend != 5*time.Second || middle.ProviderMaxResend != 5*time.Second {
		t.Fatalf("5/5 arm did not cap all three independent retry intervals: %+v, %v", middle, err)
	}
	for _, profile := range []string{"clean", "clean-rtt3s", "clean-rtt3s-burst-1", "established-rtt3s", "established-rtt3s-burst-1", "low-edge", "burst-3", "burst-6", "burst-10", "carrier-reconnect", "return-burst-3", "return-carrier-reconnect"} {
		scenario, err := dohTransferScenarioNamed(profile)
		if err != nil {
			t.Fatal(err)
		}
		trace, err := dohTransferTrace(scenario, 1)
		if err != nil || trace.ApplicationOrDirectSeed == 0 || trace.ProviderSeed == 0 || scenario.CarrierMaxRTO != 8*time.Second || scenario.Resources.TcpBufferMax != 64*1024 {
			t.Fatal("invalid scenario identity or ownership")
		}
		for _, name := range []string{"ack-8-8", "ack-5-5", "ack-3-3", "ack-3-8", "ack-8-3"} {
			_, err := dohTransferArmNamed(name)
			paired, traceErr := dohTransferTrace(scenario, 1)
			if err != nil || traceErr != nil || paired != trace {
				t.Fatal("arm changed scenario/trace")
			}
		}
		if scenario.EstablishClean != strings.HasPrefix(profile, "established-rtt3s") {
			t.Fatal("cold and established-route high RTT were conflated")
		}
		if scenario.EstablishClean {
			if scenario.SetupDeviceAccess != initialNetworkProfiles(20260810)["clean-lan"] || scenario.DeviceAccess.Forward.BaseDelay+scenario.DeviceAccess.Reverse.BaseDelay != 3*time.Second {
				t.Fatal("established-route scenario did not pin clean setup and exact 3s target RTT")
			}
		} else if scenario.SetupDeviceAccess != scenario.DeviceAccess {
			t.Fatal("ordinary scenario silently changed its setup impairment")
		}
		if profile == "low-edge" && (scenario.DeviceAccess.Forward.RateBitsPerSecond != 64000 || scenario.DeviceAccess.Reverse.RateBitsPerSecond != 256000) {
			t.Fatalf("low-edge silently selected wrong rates: %+v", scenario.DeviceAccess)
		}
		if os.Getenv("CONNECT_PERFVAR_DOH_TRANSFER_EMIT_IDENTITIES") == "1" {
			identity := dohTransferRecord{Scenario: scenario, ScenarioHash: tcpRecoveryHash(scenario),
				ProfileHash: tcpRecoveryHash([]networkProfile{scenario.DeviceAccess, scenario.ProviderAccess}), Trace: trace, RunIndex: 1}
			encoded, err := json.Marshal(identity)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("[doh-transfer-identity] %s", encoded)
		}
	}
	for _, name := range []string{"noack-8-8", "noack-3-3", "noack-3-8", "noack-8-3", "noack-3-device3-provider8", "symmetric-noack-3-3"} {
		if _, err := dohTransferArmNamed(name); err == nil {
			t.Fatalf("withdrawn NoAck policy arm %q remains enabled", name)
		}
	}
	if _, err := dohTransferScenarioNamed("missing-low-edge"); err == nil {
		t.Fatal("unknown impairment silently became a clean profile")
	}
}

func TestDohTransferEffectiveSettingsAfterMultiClientOverride(t *testing.T) {
	observer := newDohTransferEndpointObserver(false)
	settings := clientconnect.DefaultClientSettings()
	var forwarded atomic.Uint64
	settings.SendBufferSettings.SendPackLifecycleObserver = func(clientconnect.SendPackLifecycleObservation) { forwarded.Add(1) }
	observer.configure(settings, 3*time.Second)
	settings.SendBufferSettings.AckTimeout = 30 * time.Second
	id := clientconnect.NewId()
	settings.SendBufferSettings.SendPackLifecycleObserver(clientconnect.SendPackLifecycleObservation{ClientId: id})
	got, _ := observer.snapshot(id)
	if got.AckLifetime != 30*time.Second || got.MaxResend != 3*time.Second || forwarded.Load() != 1 {
		t.Fatal("effective settings or existing lifecycle accounting were lost")
	}
	if observer.wire.port.Load() != 0 || observer.wire.fault.Load() != nil {
		t.Fatal("unselected flow/fault became active by construction")
	}
}

func TestDohTransferAckTimerComponentControls(t *testing.T) {
	for _, test := range []struct {
		name          string
		tcp, transfer time.Duration
	}{
		{"ack-8-8", 8 * time.Second, 8 * time.Second},
		{"ack-5-5", 5 * time.Second, 5 * time.Second},
		{"ack-3-3", 3 * time.Second, 3 * time.Second},
		{"ack-3-8", 3 * time.Second, 8 * time.Second},
		{"ack-8-3", 8 * time.Second, 3 * time.Second},
	} {
		arm, err := dohTransferArmNamed(test.name)
		if err != nil || arm.ApplicationTCPMaxRTO != test.tcp || arm.DeviceMaxResend != test.transfer || arm.ProviderMaxResend != test.transfer {
			t.Fatalf("component control %s changed the wrong owner: %+v %v", test.name, arm, err)
		}
	}
	settings := clientconnect.DefaultMultiClientSettings()
	if !settings.TcpCollapsePrevention || settings.TcpCollapseMaxHold != 1500*time.Millisecond {
		t.Fatal("timer fixture changed fixed bounded collapse prevention")
	}
}

func TestDohTransferEstablishedAccessTransitionIsExact(t *testing.T) {
	target, err := dohTransferScenarioNamed("established-rtt3s-burst-1")
	if err != nil || target.PayloadDrops != 1 {
		t.Fatal("one-loss established scenario changed")
	}
	clean := target.SetupDeviceAccess.Forward
	before := map[string]linkProfile{
		"client-1->edge": clean, "edge->client-1": clean,
		"client-10->edge": clean, "edge->client-10": clean,
		"provider->edge": clean, "edge->provider": clean,
	}
	clone := func(source map[string]linkProfile) map[string]linkProfile {
		result := make(map[string]linkProfile, len(source))
		for name, profile := range source {
			result[name] = profile
		}
		return result
	}
	after := clone(before)
	after["client-1->edge"], after["edge->client-1"] = target.DeviceAccess.Forward, target.DeviceAccess.Reverse
	if err := validateDohTransferAccessTransition("client-1", before, after, target.DeviceAccess); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"missing-node", "missing-reverse", "provider-changed", "prefix-collision", "missing-link", "topology-expanded"} {
		t.Run(kind, func(t *testing.T) {
			broken, node := clone(after), "client-1"
			switch kind {
			case "missing-node":
				node = "client-2"
			case "missing-reverse":
				broken["edge->client-1"] = clean
			case "provider-changed":
				broken["provider->edge"] = target.DeviceAccess.Forward
			case "prefix-collision":
				broken["client-10->edge"] = target.DeviceAccess.Forward
			case "missing-link":
				delete(broken, "edge->provider")
			case "topology-expanded":
				broken["other->edge"] = clean
			}
			if validateDohTransferAccessTransition(node, before, broken, target.DeviceAccess) == nil {
				t.Fatal("invalid transition accepted")
			}
		})
	}
}

func TestDohTransferEvidenceRejectsShortcutAndPolicySubstitution(t *testing.T) {
	valid := dohTransferRecord{
		DNSCorrect: true, ApplicationTUNOwned: true,
		RequestDeviceWire:   dohTransferWireSnapshot{PayloadFrames: 1, AckPacks: 1, AckPayloadPacks: 1},
		RequestProviderWire: dohTransferWireSnapshot{PayloadFrames: 1, AckPacks: 1, AckPayloadPacks: 1},
		DeviceH1:            clientconnect.H1ConnectionStatsSnapshot{H1PlusConnectionCount: 1},
		ProviderH1:          clientconnect.H1ConnectionStatsSnapshot{H1PlusConnectionCount: 1},
	}
	if invalid := validateDohTransferEvidence(valid); invalid != "" {
		t.Fatal(invalid)
	}
	for _, kind := range []string{"host-shortcut", "return-noack", "device-noack", "pure-ack-only", "missing-payload", "websocket", "fault-not-exercised", "reconnect-no-new-tuple", "missing-transition"} {
		t.Run(kind, func(t *testing.T) {
			record := valid
			switch kind {
			case "host-shortcut":
				record.ApplicationTUNOwned = false
			case "return-noack":
				record.RequestProviderWire.NoAckPacks = 1
			case "device-noack":
				record.RequestDeviceWire.NoAckPacks = 1
			case "pure-ack-only":
				record.RequestDeviceWire.AckPayloadPacks = 0
			case "missing-payload":
				record.RequestDeviceWire.PayloadFrames = 0
			case "websocket":
				record.DeviceH1.WebSocketConnectionCount = 1
			case "fault-not-exercised":
				record.Scenario.PayloadDrops = 1
			case "reconnect-no-new-tuple":
				record.Scenario.Reconnect = true
				record.Fault.PayloadDrops, record.Fault.ClosedCarriers = 1, 1
			case "missing-transition":
				record.Scenario.EstablishClean = true
			}
			if validateDohTransferEvidence(record) == "" {
				t.Fatal("invalid evidence accepted")
			}
		})
	}
}

func TestDohTransferEndpointCaptureIsBoundedAndPreservesLogger(t *testing.T) {
	observer := newDohTransferEndpointObserver(false)
	settings := clientconnect.DefaultClientSettings()
	settings.Log = clientconnect.NewNoopLogger()
	observer.configure(settings, 3*time.Second)
	for range 65 {
		settings.SendBufferSettings.SendPackLifecycleObserver(clientconnect.SendPackLifecycleObservation{ClientId: clientconnect.NewId()})
	}
	if len(observer.settings) != 64 || observer.overflow.Load() != 1 {
		t.Fatal("endpoint settings recorder is unbounded or silently truncated")
	}
	client, provider := clientconnect.NewId(), clientconnect.NewId()
	args := h1FailureTestArgs(client, provider)
	settings.Log.Infof("unrelated", args...)
	settings.Log.Infof(h1FailureExitFormat, args[:1]...)
	if _, expired := observer.snapshot(client); expired != 0 {
		t.Fatal("unrelated/malformed log became an ACK lifetime")
	}
	settings.Log.Infof(h1FailureExitFormat, args...)
	if _, expired := observer.snapshot(client); expired != 1 {
		t.Fatal("exact active client ACK lifetime was lost")
	}
}
