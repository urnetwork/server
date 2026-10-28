// The same cold-link request/pressure plan runs with either verified inner
// protocol. Existing profile catalog entries remain separate measurements.
package perfvar

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/sdk"
	"github.com/urnetwork/server"
)

func newSdkBrowsingColdLinkPlan(profileName string) (*nativeBrowsingPressurePlan, error) {
	plan := newNativeBrowsingPressurePlan()
	switch profileName {
	case "wifi-good":
		plan.profile = initialNetworkProfiles(4013)[profileName]
	case cellEdge5mDown1mUpName:
		// The unchanged original cell stress profile remains available.
	default:
		return nil, errors.New("unowned cold-link comparison profile")
	}
	if err := plan.profile.validate(); err != nil {
		return nil, err
	}
	return plan, nil
}

// Late provider fields are immutable only after its final ready publication.
// The ordinary warm helper never overlaps construction; this row can do so.
func sdkBrowsingColdLinkCapture(fixture *sdkBrowsingSetupFixture, page int) sdkBrowsingHttp2Boundary {
	native := fixture.native.Load()
	result := sdkBrowsingHttp2Boundary{Page: page, Device: fixture.device.MemoryUsed(), BridgeStarted: native.bridgeStarted.Load(),
		BridgeCompleted: native.bridgeCompleted.Load(), BridgeAccepted: native.bridgeAccepted.Load(), BridgeRefused: native.bridgeRefused.Load()}
	for ordinal := range fixture.providers {
		provider := &fixture.providers[ordinal]
		if !provider.ready.Load() {
			continue
		}
		recovery, receive := provider.client.SendRecoveryStats(), provider.client.ReceiveStats()
		row := &result.Providers[ordinal]
		row.InitialWrites, row.TimeoutResends, row.AdmissionWaitNs = recovery.InitialWriteCount, recovery.TimeoutResendWriteCount, int64(recovery.ReliableAdmissionWaitDuration)
		row.Unacked, row.PackDrops, row.PackWaits = receive.ResendQueueUnackedItemCount, receive.PackHandoffDropCount, receive.PackHandoffWaitCount
		row.PackSaturations, row.AckBlocked = receive.PackHandoffSaturationCount, receive.AckRouteWriteBlockedCount
		row.ReturnStartedEntries, row.ReturnCompletedPackets, row.ReturnFailures = provider.returns.startedEntryCount.Load(), provider.returns.completedPacketCount.Load(), provider.returns.failures.Load()
	}
	return result
}

func runSdkBrowsingColdLink100(t *testing.T, profileName string, http2 bool) {
	if testing.Short() {
		t.Skip("actual SDK32/H1/public-provider topology needs shared PERFVAR services")
	}
	environment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	environment.Run(t, func(t testing.TB) {
		plan, err := newSdkBrowsingColdLinkPlan(profileName)
		if err != nil {
			t.Fatal(err)
		}
		scenario := newSdkBrowsingScenario(plan)
		scenario.coldLinkForTest = true
		fixture, err := newSdkBrowsingSetupFixture(t, plan, scenario)
		if err != nil {
			t.Fatal("public SDK32 constructor: ", err)
		}
		defer func() {
			if err := fixture.close(); err != nil {
				t.Error(err)
			}
		}()
		initial, err := fixture.waitInitial()
		if err != nil {
			t.Fatal(err)
		}
		scenario.initial = initial
		native := fixture.native.Load()
		native.stateLock.Lock()
		native.coldLinkOriginForTest = true
		native.stateLock.Unlock()
		// Both arms use body-consumption gates, never a shared encrypted read.
		native.suppressOriginReadHoldForTest.Store(true)
		serverTls, clientTls, err := nativeBrowsingTlsConfigsForOrigins(nativeBrowsingOriginCount + 1)
		if err != nil {
			t.Fatal(err)
		}
		pressure := &nativeBrowsingPressureObservation{Profile: plan.profile, ProviderProfile: plan.provider.Name,
			MaximumLivePages: plan.maxLivePages, HttpConnectionsPerOrigin: nativeBrowsingConnectionsPerOrigin,
			slowNavigation: sdk.DefaultDeviceLocalSettings().SendTimeout,
			PhaseNames:     [8]string{"get_conn", "got_conn", "connect_start", "connect_done", "tls_start", "tls_done", "wrote_request", "first_byte"},
			Scope:          "matched verified TLS inner H1/H2; existing profile catalog; original100HTML/400assets/99image-tail overlaps/4livepages; page1HTMLalone targets fifthunvisitedorigin; original4originassets unchanged; CSSestablishedoriginalowner retained; originalJSGetConndispatch maystillawaitTLS/headers without a controllerprefixwait; bodyreadcounts are not nativepacket custody; actualcoldtargetoldproviderdialrefusal only; no midwork forced connection retirement/DNS/delay/timer change; syntheticlocalization, no physicaliOS/WiFi attribution"}
		pressure.captureBoundaryForTest = func(*fullTunPath, int, *nativeBrowsingPressureObservation) *nativeBrowsingPressureBoundary {
			return nil
		}
		for page := range pressure.Navigations {
			pressure.Navigations[page].ConnectionAcquireNs, pressure.Navigations[page].WrittenToFirstByteNs = -1, -1
			for phase := range pressure.Navigations[page].Phases {
				pressure.Navigations[page].Phases[phase] = -1
			}
		}
		witness := newNativeBrowsingColdLinkWitness(scenario, pressure, http2)
		defer witness.close()
		settings := plan.runSettings(pressure)
		settings.coldLinkForTest, settings.innerHttp2ForTest = true, http2
		settings.originServerTlsForTest, settings.originClientTlsForTest = serverTls, clientTls
		settings.originListenerForTest, settings.originDestinationForTest = native.addOrigin, native.destination
		settings.afterOriginsClosedForTest = native.clearOrigins
		settings.applicationDeadlineForTest = plan.deadline
		settings.beforePageNavigationForTest = witness.beforeNavigation
		settings.requestTraceForTest = witness.requestTraceWithContext
		settings.wrapResponseReaderContextForTest = witness.wrapReader
		settings.wrapOriginHandlerForTest = witness.wrapHandler
		var lastHealthy, firstSlow *sdkBrowsingHttp2Boundary
		settings.afterFetchForTest = func(target string, result nativeBrowsingAssetResult) {
			witness.afterFetch(target, result)
			page, slot, _, parseErr := nativeBrowsingColdLinkRequestSlot(target)
			if parseErr != nil || slot != nativeBrowsingAssetCount {
				return
			}
			row := sdkBrowsingColdLinkCapture(fixture, page)
			cold, _ := witness.snapshot()
			if result.err != nil || cold.CombinedFirstByteNs[page] >= int64(sdk.DefaultDeviceLocalSettings().SendTimeout) {
				if firstSlow == nil {
					firstSlow = &row
				}
			} else if firstSlow == nil {
				lastHealthy = &row
			}
		}
		observation := &nativeBrowsingObservation{NativeCapability: true, Baseline: nativeBrowsingMemoryNow(), Pressure: pressure}
		observation.Peak = observation.Baseline
		start := time.Now()
		workloadErr := measureNativeBrowsing100WithSettings(fixture.ctx, native.path, observation, settings)
		pressure.finish(start)
		var verdictErr error
		if workloadErr == nil {
			// Join an actually started constructor after measurement, within its
			// original absolute app deadline. Visibility never gates navigation.
			verdictErr = errors.Join(witness.joinLateConstructor(), witness.verdict(), nativeBrowsingPressureLatencyError(observation), nativeBrowsingAssetBodyOwnershipError(pressure))
		}
		closeErr := fixture.close()
		observation.Drain = nativeBrowsingMemoryNow()
		cold, witnessErr := witness.snapshot()
		report := struct {
			Topology    string                            `json:"topology"`
			Browsing    *nativeBrowsingObservation        `json:"browsing"`
			ColdLink    nativeBrowsingColdLinkObservation `json:"cold_link"`
			LastHealthy *sdkBrowsingHttp2Boundary         `json:"last_healthy,omitempty"`
			FirstSlow   *sdkBrowsingHttp2Boundary         `json:"first_slow_or_error,omitempty"`
			AfterJoin   *sdk.DeviceLocalMemoryUsage       `json:"after_join"`
		}{Topology: sdkBrowsingTopology, Browsing: observation, ColdLink: cold, LastHealthy: lastHealthy, FirstSlow: firstSlow, AfterJoin: fixture.afterJoin}
		encoded, encodeErr := json.Marshal(report)
		if encodeErr != nil || nativeBrowsingPressureReportByteLimit < len(encoded) {
			t.Error("bounded matched cold-link report: ", encodeErr, " bytes=", len(encoded))
		} else {
			t.Logf("[perfvar] sdk-native-cold-link %s", encoded)
		}
		if err := errors.Join(workloadErr, verdictErr, witnessErr, closeErr); err != nil {
			t.Fatal("SDK32 matched cold-link browsing: ", err)
		}
	})
}

func TestFullTunH1Sdk32InnerH1ColdLinkWifi100Pages(t *testing.T) {
	runSdkBrowsingColdLink100(t, "wifi-good", false)
}

func TestFullTunH1Sdk32InnerH2ColdLinkWifi100Pages(t *testing.T) {
	runSdkBrowsingColdLink100(t, "wifi-good", true)
}

func TestFullTunH1Sdk32InnerH1ColdLinkCell100Pages(t *testing.T) {
	runSdkBrowsingColdLink100(t, cellEdge5mDown1mUpName, false)
}

func TestFullTunH1Sdk32InnerH2ColdLinkCell100Pages(t *testing.T) {
	runSdkBrowsingColdLink100(t, cellEdge5mDown1mUpName, true)
}
