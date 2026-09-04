// This file blocks performance campaigns on exact production-route behavior
// across high latency, application workloads, constrained resources, and the
// focused extreme profiles selected by the campaign plan.
package perfvar

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	connectserver "github.com/urnetwork/server/v2026/connect"
)

const (
	perfvarMobileTcpWindowSweepEnvironment   = "CONNECT_PERFVAR_MOBILE_TCP_WINDOW_SWEEP"
	perfvarMobilePacketGroupSweepEnvironment = "CONNECT_PERFVAR_MOBILE_PACKET_GROUP_SWEEP"
)

// One opt-in axis record isolates the TCP window from every other mobile
// surrogate resource and route input.
type perfvarMobileTcpWindowObservation struct {
	TcpBufferDefault     int                           `json:"tcp_buffer_default_bytes"`
	TcpBufferMax         int                           `json:"tcp_buffer_max_bytes"`
	UsefulByteCount      int64                         `json:"useful_byte_count"`
	Duration             time.Duration                 `json:"duration_nanoseconds"`
	GoodputGigabits      float64                       `json:"goodput_gigabits_per_second"`
	BridgeBatches        fullTunBridgeBatchObservation `json:"bridge_batches"`
	CarrierWireByteCount uint64                        `json:"carrier_wire_byte_count"`
	CarrierDuration      time.Duration                 `json:"carrier_duration_nanoseconds"`
}

// One opt-in comparison changes only group preservation after the native read
// burst; its singular mode is diagnostic and is never used by production.
type perfvarMobilePacketGroupObservation struct {
	RunIndex             int                           `json:"run_index"`
	Route                fullTunRoute                  `json:"route"`
	Mode                 string                        `json:"mode"`
	UsefulByteCount      int64                         `json:"useful_byte_count"`
	Duration             time.Duration                 `json:"duration_nanoseconds"`
	GoodputGigabits      float64                       `json:"goodput_gigabits_per_second"`
	BridgeBatches        fullTunBridgeBatchObservation `json:"bridge_batches"`
	CarrierWireByteCount uint64                        `json:"carrier_wire_byte_count"`
	CarrierDuration      time.Duration                 `json:"carrier_duration_nanoseconds"`
}

// One owned fixture gives every route/profile case a hard lifetime and an
// exact carrier observation boundary.
type perfvarCorrectnessFixture struct {
	ctx         context.Context
	cancel      context.CancelFunc
	environment *routeEnvironment
	path        *fullTunPath
	closeOnce   sync.Once
}

// One workload result remains paired with the physical counters observed at
// the same boundary.
type perfvarCorrectnessObservation struct {
	Result  workloadResult
	Carrier perfvarCarrierObservation
}

// Both directions use one established route while retaining independent
// workload boundaries.
type perfvarCorrectnessTCPPair struct {
	Upload   perfvarCorrectnessObservation
	Download perfvarCorrectnessObservation
}

// Aggregated link counters support focused assertions without depending on
// generated client node names.
type perfvarCorrectnessLinkTotals struct {
	LossDropPacketCount    uint64
	MtuDropPacketCount     uint64
	ReorderedPacketCount   uint64
	MaximumQueuedByteCount int
}

// Construction applies the direct-path and access-path profiles explicitly;
// every caller receives a fresh route and a hard context deadline.
func newPerfvarCorrectnessFixture(
	t testing.TB,
	route fullTunRoute,
	directProfile networkProfile,
	deviceAccessProfile networkProfile,
	providerAccessProfile networkProfile,
	resources tunResourceProfile,
	timeout time.Duration,
) (*perfvarCorrectnessFixture, error) {
	return newPerfvarCorrectnessFixtureWithHooks(
		t,
		route,
		directProfile,
		deviceAccessProfile,
		providerAccessProfile,
		resources,
		timeout,
		nil,
	)
}

// Builds the same correctness fixture while allowing a test to observe exact
// construction seams without changing production-equivalent defaults.
func newPerfvarCorrectnessFixtureWithHooks(
	t testing.TB,
	route fullTunRoute,
	directProfile networkProfile,
	deviceAccessProfile networkProfile,
	providerAccessProfile networkProfile,
	resources tunResourceProfile,
	timeout time.Duration,
	hooks *fullTunConstructionTestHooks,
) (*perfvarCorrectnessFixture, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	enableNetworkPeers := route == fullTunRouteP2pFast || route == fullTunRouteP2pLegacy
	var configureHandlerSettings func(*connectserver.ConnectHandlerSettings)
	if hooks != nil {
		configureHandlerSettings = hooks.configureConnectHandlerSettings
	}
	environment := newRouteEnvironmentWithNetworkPeersAndHandlerSettings(
		ctx,
		t,
		directProfile,
		enableNetworkPeers,
		nil,
		configureHandlerSettings,
	)
	environment.accessProfile = deviceAccessProfile
	environment.deviceAccessProfile = deviceAccessProfile
	environment.providerAccessProfile = providerAccessProfile
	fixture := &perfvarCorrectnessFixture{
		ctx:         ctx,
		cancel:      cancel,
		environment: environment,
	}
	t.Cleanup(fixture.close)
	path, err := tryNewFullTunPathWithTopologyHooks(
		ctx,
		t,
		environment,
		route,
		false,
		resources,
		1,
		hooks,
	)
	if err != nil {
		fixture.close()
		return nil, err
	}
	fixture.path = path
	return fixture, nil
}

// Teardown preserves ownership order: route users stop before their network
// and deadline are released.
func (self *perfvarCorrectnessFixture) close() {
	self.closeOnce.Do(func() {
		if self.path != nil {
			self.path.close()
		}
		self.environment.close()
		self.cancel()
	})
}

// Exact result and carrier checks run before any fixture teardown can erase
// route-specific counters.
func (self *perfvarCorrectnessFixture) measure(
	workload perfvarWorkload,
	direction perfvarDirection,
	measure func(context.Context, *fullTunPath) (workloadResult, error),
) (perfvarCorrectnessObservation, error) {
	if err := self.path.waitForMeasurementBoundary(self.ctx); err != nil {
		return perfvarCorrectnessObservation{}, fmt.Errorf(
			"%s/%s premeasurement boundary: %w",
			workload,
			direction,
			err,
		)
	}
	boundary, err := beginPerfvarCarrierMeasurement(self.path)
	if err != nil {
		return perfvarCorrectnessObservation{}, fmt.Errorf(
			"%s/%s carrier measurement start: %w",
			workload,
			direction,
			err,
		)
	}
	result, err := measure(self.ctx, self.path)
	if err == nil {
		err = self.path.waitForPostWorkloadBoundary(self.ctx)
	}
	carrier := observePerfvarWorkloadCarrier(self.path, boundary)
	observation := perfvarCorrectnessObservation{
		Result:  result,
		Carrier: carrier,
	}
	if err != nil {
		return observation, err
	}
	if result.CorruptPacketCount != 0 {
		return observation, fmt.Errorf("%s/%s corruption count=%d", workload, direction, result.CorruptPacketCount)
	}
	if carrier.WireByteCount == 0 {
		return observation, fmt.Errorf("%s/%s carrier recorded no workload bytes", workload, direction)
	}
	if err := self.path.verifyRoute(); err != nil {
		return observation, err
	}
	if err := verifyPerfvarTopologyCarrier(
		perfvarScenario{
			Route:     self.path.route,
			Workload:  workload,
			Direction: direction,
			Topology:  perfvarTopologyOneHop,
		},
		carrier,
		result.UsefulByteCount,
	); err != nil {
		return observation, err
	}
	return observation, nil
}

// Campaign correctness consumes the same workload-local start and frozen end
// as the performance runner. A synthetic setup-wide boundary cannot replace
// the narrower interval published by a warmed or post-handshake workload.
func TestPerfvarCorrectnessFixtureUsesWorkloadCarrierBoundaries(t *testing.T) {
	boundaryFixture := newFullTunMeasurementBoundaryTestFixture()
	defer boundaryFixture.close()
	boundaryFixture.path.route = fullTunRouteExchangeH1
	fixture := &perfvarCorrectnessFixture{
		ctx:  boundaryFixture.ctx,
		path: boundaryFixture.path,
	}
	startTime := time.Unix(3_000, 0)
	start := perfvarCarrierBoundary{
		capturedAt: startTime,
		links: map[string]directionalLinkSnapshot{
			"workload": {WireByteCount: 10},
		},
	}
	end := perfvarCarrierBoundary{
		capturedAt: startTime.Add(2 * time.Second),
		links: map[string]directionalLinkSnapshot{
			"workload": {WireByteCount: 110},
		},
	}
	observation, err := fixture.measure(
		perfvarWorkloadTCP,
		perfvarDirectionUpload,
		func(context.Context, *fullTunPath) (workloadResult, error) {
			boundaryFixture.path.setCarrierMeasurementStart(start)
			boundaryFixture.path.setCarrierMeasurementEnd(end, 0)
			return workloadResult{UsefulByteCount: 1}, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if observation.Carrier.WireByteCount != 100 ||
		observation.Carrier.Duration != 2*time.Second {
		t.Fatalf("campaign ignored workload-local carrier boundaries: %+v", observation.Carrier)
	}
}

// A correctness observation cannot begin while a source Pack callback has
// entered but not published. The explicit publisher barrier reproduces the
// setup-tail race that an immediate lifetime snapshot used to admit.
func TestPerfvarCorrectnessFixtureJoinsPremeasurementPackPublication(t *testing.T) {
	if testing.Short() {
		return
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		profile := initialNetworkProfiles(20260811)["clean-lan"]
		fixture, err := newPerfvarCorrectnessFixture(
			t,
			fullTunRouteExchangeH1,
			profile,
			profile,
			profile,
			defaultTunResourceProfile(),
			5*time.Minute,
		)
		if err != nil {
			t.Fatal(err)
		}
		defer fixture.close()

		publicationEntered := make(chan struct{})
		releasePublication := make(chan struct{})
		boundaryWaiting := make(chan struct{})
		var publicationOnce sync.Once
		var boundaryOnce sync.Once
		const token = uint64(20260811)
		fixture.path.devicePackSends.setBeforeObserverPublishForTest(func(
			observation clientconnect.SendPackLifecycleObservation,
		) {
			if observation.Token == token &&
				observation.Phase == clientconnect.SendPackLifecyclePhaseStarted {
				publicationOnce.Do(func() {
					close(publicationEntered)
					select {
					case <-releasePublication:
					case <-fixture.ctx.Done():
					}
				})
			}
		})
		fixture.path.devicePackSends.setBeforePublisherWaitForTest(func() {
			boundaryOnce.Do(func() { close(boundaryWaiting) })
		})
		defer fixture.path.devicePackSends.setBeforeObserverPublishForTest(nil)
		defer fixture.path.devicePackSends.setBeforePublisherWaitForTest(nil)

		observer := fixture.path.devicePackSends.newObserver()
		identity := clientconnect.SendPackLifecycleObservation{
			Token:         token,
			ClientId:      clientconnect.NewId(),
			DestinationId: clientconnect.NewId(),
			AckRequired:   true,
			MessageType:   protocol.MessageType_IpIpPacketToProvider,
		}
		publisherDone := make(chan struct{})
		go func() {
			started := identity
			started.Phase = clientconnect.SendPackLifecyclePhaseStarted
			observer(started)
			firstWrite := identity
			firstWrite.Phase = clientconnect.SendPackLifecyclePhaseFirstRouteWrite
			observer(firstWrite)
			terminal := identity
			terminal.Phase = clientconnect.SendPackLifecyclePhaseTerminal
			observer(terminal)
			close(publisherDone)
		}()
		select {
		case <-publicationEntered:
		case <-fixture.ctx.Done():
			t.Fatalf("synthetic setup publication did not enter: %v", fixture.ctx.Err())
		}

		workloadStarted := make(chan struct{})
		measurementDone := make(chan error, 1)
		go func() {
			_, measureErr := fixture.measure(
				perfvarWorkloadTCP,
				perfvarDirectionUpload,
				func(ctx context.Context, path *fullTunPath) (workloadResult, error) {
					close(workloadStarted)
					return measureFullTunUpload(ctx, path, 64*1024)
				},
			)
			measurementDone <- measureErr
		}()
		select {
		case <-boundaryWaiting:
		case <-fixture.ctx.Done():
			t.Fatalf("premeasurement boundary did not join the publisher: %v", fixture.ctx.Err())
		}
		select {
		case <-workloadStarted:
			t.Fatal("workload started while the setup Pack publication was held")
		default:
		}
		close(releasePublication)
		select {
		case <-publisherDone:
		case <-fixture.ctx.Done():
			t.Fatalf("synthetic setup publication did not finish: %v", fixture.ctx.Err())
		}
		select {
		case measureErr := <-measurementDone:
			if measureErr != nil {
				t.Fatal(measureErr)
			}
		case <-fixture.ctx.Done():
			t.Fatalf("measurement did not finish after setup publication: %v", fixture.ctx.Err())
		}
	})
}

// Hashing and byte-count checks make successful connection setup insufficient
// for either directional TCP gate.
func (self *perfvarCorrectnessFixture) measureExactTCP(
	byteCount int64,
) (perfvarCorrectnessTCPPair, error) {
	upload, err := self.measure(
		perfvarWorkloadTCP,
		perfvarDirectionUpload,
		func(ctx context.Context, path *fullTunPath) (workloadResult, error) {
			return measureFullTunUpload(ctx, path, byteCount)
		},
	)
	if err != nil {
		return perfvarCorrectnessTCPPair{Upload: upload}, fmt.Errorf("upload: %w", err)
	}
	download, err := self.measure(
		perfvarWorkloadTCP,
		perfvarDirectionDownload,
		func(ctx context.Context, path *fullTunPath) (workloadResult, error) {
			return measureFullTunDownload(ctx, path, byteCount)
		},
	)
	pair := perfvarCorrectnessTCPPair{
		Upload:   upload,
		Download: download,
	}
	if err != nil {
		return pair, fmt.Errorf("download: %w", err)
	}
	expectedHash := deterministicPayloadHash(byteCount)
	for _, result := range []workloadResult{upload.Result, download.Result} {
		if result.UsefulByteCount != byteCount || result.ContentHash != expectedHash {
			return pair, fmt.Errorf(
				"exact TCP result bytes=%d hash=%q, expected bytes=%d hash=%q",
				result.UsefulByteCount,
				result.ContentHash,
				byteCount,
				expectedHash,
			)
		}
	}
	return pair, nil
}

// The four production carrier choices are kept in one stable order for every
// matrix gate.
func perfvarCorrectnessRoutes() []fullTunRoute {
	return []fullTunRoute{
		fullTunRouteExchangeH1,
		fullTunRouteExchangeH3,
		fullTunRouteP2pLegacy,
		fullTunRouteP2pFast,
	}
}

// A colocated provider keeps these cases specific to the user-to-connect
// latency represented by the single-region profile.
func testPerfvarSingleRegionEveryRouteCorrectness(
	t *testing.T,
	profileName string,
	seed int64,
	fixtureTimeout time.Duration,
) {
	if testing.Short() {
		return
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		for _, route := range perfvarCorrectnessRoutes() {
			profiles := initialNetworkProfiles(seed)
			profile := profiles[profileName]
			providerProfile := profiles["clean-lan"]
			providerProfile.SourceNote = "synthetic provider colocated with server/connect"
			fixture, err := newPerfvarCorrectnessFixture(
				t,
				route,
				profile,
				profile,
				providerProfile,
				defaultTunResourceProfile(),
				fixtureTimeout,
			)
			if err != nil {
				t.Fatalf("construct %s/%s: %v", route, profileName, err)
			}
			_, measureErr := fixture.measureExactTCP(64 * 1024)
			fixture.close()
			if measureErr != nil {
				t.Fatalf("%s/%s exact bidirectional TCP: %v", route, profileName, measureErr)
			}
		}
	})
}

// A 500 ms user-to-connect RTT must preserve exact TCP in both directions on
// every carrier before a performance campaign may interpret its numbers.
func TestPerfvarSingleRegion500msEveryRouteCorrectness(t *testing.T) {
	testPerfvarSingleRegionEveryRouteCorrectness(
		t,
		"single-region-500ms-rtt",
		2026081150,
		8*time.Minute,
	)
}

// A 1 s user-to-connect RTT is the maximum regional correctness gate applied
// to every carrier before its performance records are accepted.
func TestPerfvarSingleRegion1000msEveryRouteCorrectness(t *testing.T) {
	testPerfvarSingleRegionEveryRouteCorrectness(
		t,
		"single-region-1000ms-rtt",
		2026081100,
		10*time.Minute,
	)
}

// A focused developer gate reproduces the maximum-RTT legacy P2P lifecycle
// without paying for the preceding exchange routes.
func TestPerfvarSingleRegion1000msP2pLegacyCorrectness(t *testing.T) {
	if testing.Short() {
		return
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		profiles := initialNetworkProfiles(2026081100)
		profile := profiles["single-region-1000ms-rtt"]
		providerProfile := profiles["clean-lan"]
		providerProfile.SourceNote = "synthetic provider colocated with server/connect"
		fixture, err := newPerfvarCorrectnessFixture(
			t,
			fullTunRouteP2pLegacy,
			profile,
			profile,
			providerProfile,
			defaultTunResourceProfile(),
			10*time.Minute,
		)
		if err != nil {
			t.Fatal(err)
		}
		_, measureErr := fixture.measureExactTCP(64 * 1024)
		fixture.close()
		if measureErr != nil {
			t.Fatal(measureErr)
		}
	})
}

// A focused developer gate reproduces the maximum-RTT native P2P lifecycle
// without paying for the preceding exchange routes.
func TestPerfvarSingleRegion1000msP2pFastCorrectness(t *testing.T) {
	if testing.Short() {
		return
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		profiles := initialNetworkProfiles(2026081100)
		profile := profiles["single-region-1000ms-rtt"]
		providerProfile := profiles["clean-lan"]
		providerProfile.SourceNote = "synthetic provider colocated with server/connect"
		fixture, err := newPerfvarCorrectnessFixture(
			t,
			fullTunRouteP2pFast,
			profile,
			profile,
			providerProfile,
			defaultTunResourceProfile(),
			10*time.Minute,
		)
		if err != nil {
			t.Fatal(err)
		}
		_, measureErr := fixture.measureExactTCP(64 * 1024)
		fixture.close()
		if measureErr != nil {
			t.Fatal(measureErr)
		}
	})
}

// Fresh construction matches the performance runner's one-route-per-scenario
// isolation and prevents an earlier protocol from satisfying carrier checks.
func measurePerfvarFreshApplicationWorkload(
	t testing.TB,
	route fullTunRoute,
	profile networkProfile,
	workload perfvarWorkload,
	direction perfvarDirection,
	measure func(context.Context, *fullTunPath) (workloadResult, error),
) (perfvarCorrectnessObservation, error) {
	t.Helper()
	fixture, err := newPerfvarCorrectnessFixture(
		t,
		route,
		profile,
		profile,
		profile,
		defaultTunResourceProfile(),
		5*time.Minute,
	)
	if err != nil {
		return perfvarCorrectnessObservation{}, err
	}
	observation, measureErr := fixture.measure(workload, direction, measure)
	if measureErr != nil && (route == fullTunRouteExchangeH3 || route == fullTunRouteExchangeAuto) {
		measureErr = fmt.Errorf(
			"%w; device_h3=%+v provider_h3=%+v device_receive=%+v provider_receive=%+v device_packets=%+v provider_packets=%+v device_recovery=%+v provider_recovery=%+v",
			measureErr,
			fixture.path.deviceH3DatagramStats.Snapshot(),
			fixture.path.providerH3DatagramStats.Snapshot(),
			fixture.path.devicePlatformReceiveStats.Snapshot(),
			fixture.path.providerPlatformReceiveStats.Snapshot(),
			observation.Carrier.DevicePacketStats,
			observation.Carrier.ProviderPacketStats,
			observation.Carrier.DeviceSendRecovery,
			observation.Carrier.ProviderSendRecovery,
		)
	}
	fixture.close()
	return observation, measureErr
}

// All currently implemented workload directions use exact protocol-specific
// checks on fresh production-route fixtures.
func testPerfvarApplicationWorkloads(
	t testing.TB,
	route fullTunRoute,
	profile networkProfile,
) error {
	const parallelFlowCount = 4
	const parallelFlowByteCount = int64(64 * 1024)
	expectedParallelByteCount := int64(parallelFlowCount) * parallelFlowByteCount
	expectedParallelHash := deterministicPayloadHash(parallelFlowByteCount)
	parallelUpload, err := measurePerfvarFreshApplicationWorkload(
		t,
		route,
		profile,
		perfvarWorkloadTCPParallel,
		perfvarDirectionUpload,
		func(ctx context.Context, path *fullTunPath) (workloadResult, error) {
			return measureFullTunParallelUploads(ctx, path, parallelFlowCount, parallelFlowByteCount)
		},
	)
	if err != nil {
		return fmt.Errorf("parallel TCP upload: %w", err)
	}
	if parallelUpload.Result.UsefulByteCount != expectedParallelByteCount ||
		parallelUpload.Result.ContentHash != expectedParallelHash {
		return fmt.Errorf("parallel TCP upload result=%+v", parallelUpload.Result)
	}
	parallelDownload, err := measurePerfvarFreshApplicationWorkload(
		t,
		route,
		profile,
		perfvarWorkloadTCPParallel,
		perfvarDirectionDownload,
		func(ctx context.Context, path *fullTunPath) (workloadResult, error) {
			return measureFullTunParallelDownloads(ctx, path, parallelFlowCount, parallelFlowByteCount)
		},
	)
	if err != nil {
		return fmt.Errorf("parallel TCP download: %w", err)
	}
	if parallelDownload.Result.UsefulByteCount != expectedParallelByteCount ||
		parallelDownload.Result.ContentHash != expectedParallelHash {
		return fmt.Errorf("parallel TCP download result=%+v", parallelDownload.Result)
	}

	const quicByteCount = int64(64 * 1024)
	quic, err := measurePerfvarFreshApplicationWorkload(
		t,
		route,
		profile,
		perfvarWorkloadQUIC,
		perfvarDirectionUpload,
		func(ctx context.Context, path *fullTunPath) (workloadResult, error) {
			return measureFullTunQUIC(ctx, path, quicByteCount)
		},
	)
	if err != nil {
		return fmt.Errorf("QUIC upload: %w", err)
	}
	if quic.Result.UsefulByteCount != quicByteCount ||
		quic.Result.ContentHash != deterministicPayloadHash(quicByteCount) {
		return fmt.Errorf("QUIC upload result=%+v", quic.Result)
	}

	udpUpload, err := measurePerfvarFreshApplicationWorkload(
		t,
		route,
		profile,
		perfvarWorkloadUDP,
		perfvarDirectionUpload,
		func(ctx context.Context, path *fullTunPath) (workloadResult, error) {
			return measureFullTunUDPDirection(ctx, path, true, 100*time.Millisecond, 2_000_000, 800)
		},
	)
	if err != nil {
		return fmt.Errorf("UDP upload: %w", err)
	}
	if udpUpload.Result.OfferedPacketCount <= 0 ||
		udpUpload.Result.DeliveredPacketCount != udpUpload.Result.OfferedPacketCount ||
		udpUpload.Result.CorruptPacketCount != 0 {
		return fmt.Errorf("UDP upload result=%+v", udpUpload.Result)
	}
	udpDownload, err := measurePerfvarFreshApplicationWorkload(
		t,
		route,
		profile,
		perfvarWorkloadUDP,
		perfvarDirectionDownload,
		func(ctx context.Context, path *fullTunPath) (workloadResult, error) {
			return measureFullTunUDPDirection(ctx, path, false, 100*time.Millisecond, 2_000_000, 800)
		},
	)
	if err != nil {
		return fmt.Errorf("UDP download: %w", err)
	}
	if udpDownload.Result.OfferedPacketCount <= 0 ||
		udpDownload.Result.DeliveredPacketCount != udpDownload.Result.OfferedPacketCount ||
		udpDownload.Result.CorruptPacketCount != 0 {
		return fmt.Errorf("UDP download result=%+v", udpDownload.Result)
	}

	web, err := measurePerfvarFreshApplicationWorkload(
		t,
		route,
		profile,
		perfvarWorkloadWeb,
		perfvarDirectionDownload,
		measureFullTunWeb,
	)
	if err != nil {
		return fmt.Errorf("web download: %w", err)
	}
	if web.Result.UsefulByteCount != 544*1024 || web.Result.TimeToFirstByte <= 0 {
		return fmt.Errorf("web download result=%+v", web.Result)
	}

	const loadedByteCount = int64(2 * 1024 * 1024)
	loaded, err := measurePerfvarFreshApplicationWorkload(
		t,
		route,
		profile,
		perfvarWorkloadLatencyUnderLoad,
		perfvarDirectionUpload,
		func(ctx context.Context, path *fullTunPath) (workloadResult, error) {
			return measureFullTunLatencyUnderLoad(ctx, path, loadedByteCount)
		},
	)
	if err != nil {
		return fmt.Errorf("latency under load result=%+v: %w", loaded.Result, err)
	}
	if loaded.Result.UsefulByteCount != loadedByteCount ||
		loaded.Result.ContentHash != deterministicPayloadHash(loadedByteCount) ||
		loaded.Result.IdleLatency.P50 <= 0 || loaded.Result.PostLoadLatency.P50 <= 0 ||
		loaded.Result.LoadedProbeSuccessCount < minimumLatencyProbeSuccessCount {
		return fmt.Errorf("latency under load result=%+v", loaded.Result)
	}
	loadedDownload, err := measurePerfvarFreshApplicationWorkload(
		t,
		route,
		profile,
		perfvarWorkloadLatencyUnderLoad,
		perfvarDirectionDownload,
		func(ctx context.Context, path *fullTunPath) (workloadResult, error) {
			return measureFullTunLatencyUnderLoadDirection(ctx, path, loadedByteCount, false)
		},
	)
	if err != nil {
		return fmt.Errorf(
			"download latency under load result=%+v: %w",
			loadedDownload.Result,
			err,
		)
	}
	if loadedDownload.Result.UsefulByteCount != loadedByteCount ||
		loadedDownload.Result.ContentHash != deterministicPayloadHash(loadedByteCount) ||
		loadedDownload.Result.IdleLatency.P50 <= 0 ||
		loadedDownload.Result.PostLoadLatency.P50 <= 0 ||
		loadedDownload.Result.LoadedProbeSuccessCount < minimumLatencyProbeSuccessCount {
		return fmt.Errorf("download latency under load result=%+v", loadedDownload.Result)
	}
	return nil
}

// Every supported non-baseline workload/direction pair must work over all
// four production carriers before its measurements are eligible for analysis.
func TestPerfvarEveryRouteApplicationWorkloadDirectionsCorrectness(t *testing.T) {
	if testing.Short() {
		return
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		for routeIndex, route := range perfvarCorrectnessRoutes() {
			profile := initialNetworkProfiles(2026081200 + int64(routeIndex))["clean-lan"]
			measureErr := testPerfvarApplicationWorkloads(t, route, profile)
			if measureErr != nil {
				t.Fatalf("%s application workload correctness: %v", route, measureErr)
			}
		}
	})
}

// Constrained TUN queues, buffers, batches, and app-boundary delay must retain
// exact bidirectional TCP on every carrier.
func TestPerfvarEveryRouteMobileSurrogateCorrectness(t *testing.T) {
	if testing.Short() {
		return
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		for routeIndex, route := range perfvarCorrectnessRoutes() {
			profile := initialNetworkProfiles(2026081300 + int64(routeIndex))["clean-lan"]
			fixture, err := newPerfvarCorrectnessFixture(
				t,
				route,
				profile,
				profile,
				profile,
				mobileTunResourceProfile(),
				8*time.Minute,
			)
			if err != nil {
				t.Fatalf("construct %s mobile-surrogate fixture: %v", route, err)
			}
			_, measureErr := fixture.measureExactTCP(64 * 1024)
			fixture.close()
			if measureErr != nil {
				t.Fatalf("%s mobile-surrogate exact bidirectional TCP: %v", route, measureErr)
			}
		}
	})
}

// An opt-in same-host sweep identifies the smallest auto-tuning ceiling that
// removes the old fixed-window limit without changing any other axis.
func TestPerfvarMobileTcpWindowSweep(t *testing.T) {
	if os.Getenv(perfvarMobileTcpWindowSweepEnvironment) != "1" {
		return
	}
	if perfvarRaceEnabled {
		t.Fatal("mobile TCP window measurements must not run with the race detector")
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		profile := initialNetworkProfiles(20260812)["clean-lan"]
		for _, tcpBufferMax := range []int{
			256 * 1024,
			512 * 1024,
			1024 * 1024,
			2 * 1024 * 1024,
			4 * 1024 * 1024,
		} {
			resources := mobileTunResourceProfile()
			resources.TcpBufferMax = tcpBufferMax
			fixture, err := newPerfvarCorrectnessFixture(
				t,
				fullTunRouteExchangeH1,
				profile,
				profile,
				profile,
				resources,
				5*time.Minute,
			)
			if err != nil {
				t.Fatalf("construct mobile TCP maximum=%d: %v", tcpBufferMax, err)
			}
			observation, measureErr := fixture.measure(
				perfvarWorkloadTCP,
				perfvarDirectionUpload,
				func(ctx context.Context, path *fullTunPath) (workloadResult, error) {
					return measureFullTunUpload(ctx, path, 32*1024*1024)
				},
			)
			fixture.close()
			if measureErr != nil {
				t.Fatalf("measure mobile TCP maximum=%d: %v", tcpBufferMax, measureErr)
			}
			record := perfvarMobileTcpWindowObservation{
				TcpBufferDefault:     resources.TcpBufferDefault,
				TcpBufferMax:         resources.TcpBufferMax,
				UsefulByteCount:      observation.Result.UsefulByteCount,
				Duration:             observation.Result.Duration,
				GoodputGigabits:      observation.Result.GoodputGigabits,
				BridgeBatches:        observation.Carrier.BridgeBatches,
				CarrierWireByteCount: observation.Carrier.WireByteCount,
				CarrierDuration:      observation.Carrier.Duration,
			}
			encoded, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("[perfvar-mobile-window] %s", encoded)
		}
	})
}

// An opt-in same-host comparison measures the old packet-at-a-time route
// handoff against one whole-group send under the corrected TCP window.
func TestPerfvarMobilePacketGroupSweep(t *testing.T) {
	if os.Getenv(perfvarMobilePacketGroupSweepEnvironment) != "1" {
		return
	}
	if perfvarRaceEnabled {
		t.Fatal("mobile packet-group measurements must not run with the race detector")
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		for routeIndex, route := range perfvarCorrectnessRoutes() {
			profile := initialNetworkProfiles(20260812 + int64(routeIndex))["clean-lan"]
			for runIndex := 1; runIndex <= 5; runIndex += 1 {
				modes := []bool{true, false}
				if runIndex%2 == 0 {
					modes = []bool{false, true}
				}
				for _, singularBridgeSend := range modes {
					resources := mobileTunResourceProfile()
					resources.SingularBridgeSend = singularBridgeSend
					fixture, err := newPerfvarCorrectnessFixture(
						t,
						route,
						profile,
						profile,
						profile,
						resources,
						5*time.Minute,
					)
					if err != nil {
						t.Fatalf("construct mobile packet group singular=%t: %v", singularBridgeSend, err)
					}
					observation, measureErr := fixture.measure(
						perfvarWorkloadTCP,
						perfvarDirectionUpload,
						func(ctx context.Context, path *fullTunPath) (workloadResult, error) {
							return measureFullTunUpload(ctx, path, 32*1024*1024)
						},
					)
					fixture.close()
					if measureErr != nil {
						t.Fatalf("measure mobile packet group singular=%t: %v", singularBridgeSend, measureErr)
					}
					mode := "packet-group"
					if singularBridgeSend {
						mode = "packet-at-a-time"
					}
					record := perfvarMobilePacketGroupObservation{
						RunIndex:             runIndex,
						Route:                route,
						Mode:                 mode,
						UsefulByteCount:      observation.Result.UsefulByteCount,
						Duration:             observation.Result.Duration,
						GoodputGigabits:      observation.Result.GoodputGigabits,
						BridgeBatches:        observation.Carrier.BridgeBatches,
						CarrierWireByteCount: observation.Carrier.WireByteCount,
						CarrierDuration:      observation.Carrier.Duration,
					}
					encoded, err := json.Marshal(record)
					if err != nil {
						t.Fatal(err)
					}
					t.Logf("[perfvar-mobile-packet-group] %s", encoded)
				}
			}
		}
	})
}

// Summation keeps impairment evidence independent of generated simulator link
// names while preserving every cause-specific counter.
func perfvarCorrectnessTotals(
	carrier perfvarCarrierObservation,
) perfvarCorrectnessLinkTotals {
	totals := perfvarCorrectnessLinkTotals{}
	for _, snapshot := range carrier.Links {
		totals.LossDropPacketCount += snapshot.LossDropPacketCount
		totals.MtuDropPacketCount += snapshot.MtuDropPacketCount
		totals.ReorderedPacketCount += snapshot.ReorderedPacketCount
		totals.MaximumQueuedByteCount = max(totals.MaximumQueuedByteCount, snapshot.MaximumQueuedBytes)
	}
	for _, snapshot := range []directionalLinkSnapshot{
		carrier.P2PNetwork.Forward,
		carrier.P2PNetwork.Reverse,
	} {
		totals.LossDropPacketCount += snapshot.LossDropPacketCount
		totals.MtuDropPacketCount += snapshot.MtuDropPacketCount
		totals.ReorderedPacketCount += snapshot.ReorderedPacketCount
		totals.MaximumQueuedByteCount = max(totals.MaximumQueuedByteCount, snapshot.MaximumQueuedBytes)
	}
	return totals
}

// A configured simulator link only counts when the same workload boundary
// reports physical bytes on that link.
func (self *perfvarCorrectnessFixture) hasActiveLinkProfile(
	observation perfvarCorrectnessObservation,
	match func(linkProfile) bool,
) bool {
	profiles := self.environment.network.snapshotProfiles()
	for name, snapshot := range observation.Carrier.Links {
		profile, ok := profiles[name]
		if ok && snapshot.WireByteCount != 0 && match(profile) {
			return true
		}
	}
	return false
}

// Focused cases require both exact application delivery and the strongest
// deterministic impairment evidence exposed by their physical carrier.
func verifyPerfvarExtremeObservation(
	fixture *perfvarCorrectnessFixture,
	profileName string,
	profile networkProfile,
	observation perfvarCorrectnessObservation,
) error {
	totals := perfvarCorrectnessTotals(observation.Carrier)
	switch profileName {
	case "jitter-25ms":
		activeJitter := fixture.hasActiveLinkProfile(observation, func(link linkProfile) bool {
			return link.Jitter == 25*time.Millisecond
		})
		if fixture.path.p2pNetwork != nil {
			activeJitter = (fixture.path.p2pNetwork.profile.Forward.Jitter == 25*time.Millisecond ||
				fixture.path.p2pNetwork.profile.Reverse.Jitter == 25*time.Millisecond) &&
				0 < observation.Carrier.P2PNetwork.ForwardPacketCount+
					observation.Carrier.P2PNetwork.ReversePacketCount
		}
		if !activeJitter {
			return fmt.Errorf("no active link exposed 25 ms jitter: %+v", observation.Carrier.Links)
		}
	case "reorder-500bp":
		if totals.ReorderedPacketCount == 0 {
			return fmt.Errorf("no seeded reorder event was observed: %+v", observation.Carrier.Links)
		}
	case "loss-200bp":
		dropCount := totals.LossDropPacketCount +
			observation.Carrier.P2PNetwork.ForwardDropCount +
			observation.Carrier.P2PNetwork.ReverseDropCount
		if dropCount == 0 {
			return fmt.Errorf("no seeded loss event was observed: links=%+v p2p=%+v", observation.Carrier.Links, observation.Carrier.P2PNetwork)
		}
	case "rate-10mbps":
		activeRate := fixture.hasActiveLinkProfile(observation, func(link linkProfile) bool {
			return link.RateBitsPerSecond == 10_000_000
		})
		if fixture.path.p2pNetwork != nil {
			activeRate = (fixture.path.p2pNetwork.profile.Forward.RateBitsPerSecond == 10_000_000 ||
				fixture.path.p2pNetwork.profile.Reverse.RateBitsPerSecond == 10_000_000) &&
				0 < observation.Carrier.P2PNetwork.ForwardPacketCount+
					observation.Carrier.P2PNetwork.ReversePacketCount
		}
		if !activeRate {
			return fmt.Errorf("10 Mbps profile was not active on the workload carrier")
		}
	case "queue-shallow":
		expectedQueueByteCount := bandwidthDelayQueue(1_000_000_000, 5*time.Millisecond)
		activeQueue := fixture.hasActiveLinkProfile(observation, func(link linkProfile) bool {
			return link.QueueByteCount == expectedQueueByteCount && link.AllowQueueDrops
		})
		if fixture.path.p2pNetwork != nil {
			activeQueue = (fixture.path.p2pNetwork.profile.Forward.QueueByteCount == expectedQueueByteCount &&
				fixture.path.p2pNetwork.profile.Forward.AllowQueueDrops) ||
				(fixture.path.p2pNetwork.profile.Reverse.QueueByteCount == expectedQueueByteCount &&
					fixture.path.p2pNetwork.profile.Reverse.AllowQueueDrops)
		}
		if !activeQueue || totals.MaximumQueuedByteCount == 0 {
			return fmt.Errorf("shallow queue was not active and observable: totals=%+v", totals)
		}
	case "mtu-1280", "mtu-1400":
		outerMtu := profile.Forward.OuterMtu
		if totals.MtuDropPacketCount != 0 || observation.Carrier.P2PNetwork.MtuDropCount != 0 {
			return fmt.Errorf("safe MTU profile dropped packets: links=%+v p2p=%+v", observation.Carrier.Links, observation.Carrier.P2PNetwork)
		}
		if fixture.path.p2pNetwork != nil {
			if uint64(outerMtu) < observation.Carrier.P2PNetwork.MaximumPacketByteCount {
				return fmt.Errorf("P2P packet exceeded safe outer MTU %d: %+v", outerMtu, observation.Carrier.P2PNetwork)
			}
		} else if !fixture.hasActiveLinkProfile(observation, func(link linkProfile) bool {
			return link.OuterMtu == outerMtu
		}) {
			return fmt.Errorf("outer MTU %d was not active on an exchange link", outerMtu)
		}
	case "rtt-150ms":
		if fixture.path.p2pNetwork == nil {
			return fmt.Errorf("150 ms RTT profile did not use the P2P carrier")
		}
		directProfile := fixture.path.p2pNetwork.profile
		if directProfile.Forward.BaseDelay != 75*time.Millisecond ||
			directProfile.Reverse.BaseDelay != 75*time.Millisecond {
			return fmt.Errorf("150 ms RTT profile was not active on the P2P carrier: %+v", directProfile)
		}
		if observation.Carrier.P2PNetwork.ForwardPacketCount == 0 ||
			observation.Carrier.P2PNetwork.ReversePacketCount == 0 {
			return fmt.Errorf("workload did not cross both P2P carrier directions: %+v", observation.Carrier.P2PNetwork)
		}
	default:
		return fmt.Errorf("unknown extreme correctness profile %q", profileName)
	}
	return nil
}

// The selected edge cases cover each simulator axis that could otherwise make
// a completed measurement incorrect or physically unattributable.
func TestPerfvarExtremeProfileRoutesCorrectness(t *testing.T) {
	if testing.Short() {
		return
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		cases := []struct {
			route        fullTunRoute
			profileName  string
			payloadBytes int64
		}{
			{route: fullTunRouteExchangeH3, profileName: "jitter-25ms", payloadBytes: 512 * 1024},
			{route: fullTunRouteExchangeH3, profileName: "reorder-500bp", payloadBytes: 1024 * 1024},
			{route: fullTunRouteExchangeH3, profileName: "loss-200bp", payloadBytes: 1024 * 1024},
			{route: fullTunRouteExchangeH3, profileName: "rate-10mbps", payloadBytes: 512 * 1024},
			{route: fullTunRouteExchangeH3, profileName: "queue-shallow", payloadBytes: 512 * 1024},
			{route: fullTunRouteExchangeH3, profileName: "mtu-1280", payloadBytes: 512 * 1024},
			{route: fullTunRouteExchangeH1, profileName: "reorder-500bp", payloadBytes: 1024 * 1024},
			{route: fullTunRouteP2pLegacy, profileName: "jitter-25ms", payloadBytes: 512 * 1024},
			{route: fullTunRouteP2pFast, profileName: "jitter-25ms", payloadBytes: 512 * 1024},
			{route: fullTunRouteP2pFast, profileName: "reorder-500bp", payloadBytes: 1024 * 1024},
			{route: fullTunRouteP2pFast, profileName: "queue-shallow", payloadBytes: 512 * 1024},
			{route: fullTunRouteP2pFast, profileName: "loss-200bp", payloadBytes: 1024 * 1024},
			{route: fullTunRouteP2pFast, profileName: "rate-10mbps", payloadBytes: 512 * 1024},
			{route: fullTunRouteP2pFast, profileName: "rtt-150ms", payloadBytes: 512 * 1024},
			{route: fullTunRouteP2pFast, profileName: "mtu-1400", payloadBytes: 512 * 1024},
			{route: fullTunRouteP2pFast, profileName: "mtu-1280", payloadBytes: 512 * 1024},
		}
		for caseIndex, testCase := range cases {
			profiles := allNetworkProfiles(2026081400 + int64(caseIndex))
			profile := profiles[testCase.profileName]
			deviceAccessProfile := profile
			providerAccessProfile := profile
			if testCase.route == fullTunRouteP2pFast ||
				testCase.route == fullTunRouteP2pLegacy {
				deviceAccessProfile = profiles["clean-lan"]
				providerAccessProfile = profiles["clean-lan"]
			}
			fixture, err := newPerfvarCorrectnessFixture(
				t,
				testCase.route,
				profile,
				deviceAccessProfile,
				providerAccessProfile,
				defaultTunResourceProfile(),
				10*time.Minute,
			)
			if err != nil {
				t.Fatalf("construct %s/%s: %v", testCase.route, testCase.profileName, err)
			}
			pair, measureErr := fixture.measureExactTCP(testCase.payloadBytes)
			if measureErr == nil {
				measureErr = verifyPerfvarExtremeObservation(
					fixture,
					testCase.profileName,
					profile,
					pair.Upload,
				)
			}
			if measureErr == nil {
				measureErr = verifyPerfvarExtremeObservation(
					fixture,
					testCase.profileName,
					profile,
					pair.Download,
				)
			}
			fixture.close()
			if measureErr != nil {
				t.Fatalf("%s/%s exact bidirectional TCP and impairment evidence: %v", testCase.route, testCase.profileName, measureErr)
			}
		}
	})
}

// A constrained direct carrier must continue carrying both inner TCP
// directions after its initial send burst fills the modeled bottleneck queue.
func TestPerfvarP2pFastRateLimitedTCPDirectionsCorrectness(t *testing.T) {
	if testing.Short() {
		return
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		profiles := allNetworkProfiles(2026081412)
		profile := profiles["rate-10mbps"]
		cleanProfile := profiles["clean-lan"]
		fixture, err := newPerfvarCorrectnessFixture(
			t,
			fullTunRouteP2pFast,
			profile,
			cleanProfile,
			cleanProfile,
			defaultTunResourceProfile(),
			5*time.Minute,
		)
		if err != nil {
			t.Fatal(err)
		}
		defer fixture.close()
		pair, measureErr := fixture.measureExactTCP(512 * 1024)
		if measureErr != nil {
			t.Fatalf(
				"rate-limited exact TCP: %v; upload=%+v download=%+v live-p2p=%+v provider=%+v device=%+v",
				measureErr,
				pair.Upload,
				pair.Download,
				fixture.path.p2pNetwork.snapshot(),
				fixture.path.providerStats.Snapshot(),
				fixture.path.deviceStats.Snapshot(),
			)
		}
	})
}
