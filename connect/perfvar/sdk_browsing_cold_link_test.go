// The same cold-link request/pressure plan runs with either verified inner
// protocol. Existing profile catalog entries remain separate measurements.
package perfvar

import (
	"context"
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

// Carrier selection and provider recovery are independent explicit choices.
// Omitted options retain ordinary construction, including recovery count1.
type sdkBrowsingColdLinkOptions struct {
	autoH1Only                           bool
	providerReturnRecoveryMaxPacketCount int
}

// Apply explicit choices before constructing any initial or late provider.
func runSdkBrowsingColdLink100(t *testing.T, profileName string, http2 bool, options ...sdkBrowsingColdLinkOptions) {
	if testing.Short() {
		t.Skip("actual SDK32/H1/public-provider topology needs shared PERFVAR services")
	}
	environment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	environment.Run(t, func(t testing.TB) {
		plan, err := newSdkBrowsingColdLinkPlan(profileName)
		if err != nil {
			t.Fatal(err)
		}
		if len(options) > 1 {
			t.Fatal("one optional cold-link configuration is supported")
		}
		if len(options) == 1 {
			plan.autoH1OnlyForTest = options[0].autoH1Only
			plan.providerReturnRecoveryMaxPacketCountForTest = options[0].providerReturnRecoveryMaxPacketCount
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
		performance := newC48ReturnFlightPerformance(plan.autoH1OnlyForTest)
		witness := newNativeBrowsingColdLinkWitness(scenario, pressure, http2)
		defer witness.close()
		settings := plan.runSettings(pressure)
		settings.coldLinkForTest, settings.innerHttp2ForTest = true, http2
		settings.originServerTlsForTest, settings.originClientTlsForTest = serverTls, clientTls
		settings.originListenerForTest, settings.originDestinationForTest = native.addOrigin, native.destination
		settings.afterOriginsClosedForTest = native.clearOrigins
		settings.applicationDeadlineForTest = plan.deadline
		settings.beforePageNavigationForTest = func(ctx context.Context, page int, closeIdle func()) error {
			performance.beforeNavigation(page)
			return witness.beforeNavigation(ctx, page, closeIdle)
		}
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
			performance.afterHtml(page, result.err)
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
		if plan.autoH1OnlyForTest {
			if err := nativeBrowsingAutoH1OnlyPolicyError(fixture.device); err != nil {
				t.Fatal(err)
			}
		}
		performance.BeforeWork = c48ReturnFlightCaptureBoundary(fixture, false)
		start := time.Now()
		performance.start = start
		performance.WorkloadStartUnixNs = start.UnixNano()
		workloadErr := measureNativeBrowsing100WithSettings(fixture.ctx, native.path, observation, settings)
		performance.WorkloadEndOffsetNs = int64(time.Since(start))
		pressure.finish(start)
		performance.AfterWork = c48ReturnFlightCaptureBoundary(fixture, false)
		var verdictErr, lateErr, witnessVerdictErr, latencyErr, bodyErr, autoPolicyErr error
		var witnessLatencyFailed bool
		if plan.autoH1OnlyForTest {
			autoPolicyErr = nativeBrowsingAutoH1OnlyPolicyError(fixture.device)
		}
		if workloadErr == nil {
			// Join an actually started constructor after measurement, within its
			// original absolute app deadline. Visibility never gates navigation.
			lateErr = witness.joinLateConstructor()
			witnessVerdictErr, witnessLatencyFailed = witness.verdictWithLatencyForTest()
			latencyErr = nativeBrowsingPressureLatencyError(observation)
			bodyErr = nativeBrowsingAssetBodyOwnershipError(pressure)
			verdictErr = errors.Join(lateErr, witnessVerdictErr, latencyErr, bodyErr)
		}
		verdictErr = errors.Join(verdictErr, autoPolicyErr)
		closeErr := fixture.close()
		observation.Drain = nativeBrowsingMemoryNow()
		cold, witnessErr := witness.snapshot()
		if closeErr == nil {
			performance.AfterJoin = c48ReturnFlightCaptureBoundary(fixture, true)
		}
		performance.Outcome = c48ReturnFlightOutcome{
			ClassificationVersion:      2,
			GoalGatePassed:             workloadErr == nil && verdictErr == nil && witnessErr == nil && closeErr == nil,
			WorkloadErrorClass:         c48ReturnFlightWorkloadErrorClass(workloadErr),
			WorkloadDeadlineOnly:       c48ReturnFlightDeadlineOnly(workloadErr),
			WorkloadOwnershipFailed:    c48ReturnFlightWorkloadOwnershipFailed(workloadErr),
			WorkloadAdditionalFailure:  errors.Is(workloadErr, context.DeadlineExceeded) && !c48ReturnFlightDeadlineOnly(workloadErr),
			VerdictEvaluated:           workloadErr == nil,
			LatencyGateFailed:          latencyErr != nil || witnessLatencyFailed,
			ColdChainLatencyGateFailed: witnessLatencyFailed,
			PayloadMismatch:            errors.Is(workloadErr, errNativeBrowsingResponseMismatch) || bodyErr != nil,
			LateConstructorFailed:      lateErr != nil,
			WitnessVerdictFailed:       witnessVerdictErr != nil && !witnessLatencyFailed,
			WitnessSnapshotFailed:      witnessErr != nil,
			CleanupFailed:              closeErr != nil,
			AutoPolicyFailed:           autoPolicyErr != nil,
		}
		performance.Outcome.LatencyDataValid = c48ReturnFlightLatencyDataValid(observation, cold,
			performance.Outcome, performance.AfterJoin)
		if plan.autoH1OnlyForTest {
			performance.AutoPolicyBeforeCloseValid = performance.AfterWork.AutoPolicyValid
		}
		report := struct {
			Topology    string                            `json:"topology"`
			Performance *c48ReturnFlightPerformance       `json:"return_flight_performance"`
			Browsing    *nativeBrowsingObservation        `json:"browsing"`
			ColdLink    nativeBrowsingColdLinkObservation `json:"cold_link"`
			LastHealthy *sdkBrowsingHttp2Boundary         `json:"last_healthy,omitempty"`
			FirstSlow   *sdkBrowsingHttp2Boundary         `json:"first_slow_or_error,omitempty"`
			AfterJoin   *sdk.DeviceLocalMemoryUsage       `json:"after_join"`
		}{Topology: sdkBrowsingTopology, Performance: performance, Browsing: observation, ColdLink: cold, LastHealthy: lastHealthy, FirstSlow: firstSlow, AfterJoin: fixture.afterJoin}
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
