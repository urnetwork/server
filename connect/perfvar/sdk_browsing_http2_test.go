// Persistent inner HTTP/2 runs the original 100-page plan through the public
// SDK32/native/provider/outer-H1 fixture. The H1 new-provider row stays separate.
package perfvar

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/sdk"
	"github.com/urnetwork/server"
)

// Fixed scalar samples are navigation-return diagnostics. They do not claim
// to identify a queue at the exact five-second crossing or an HTTP/2 window.
type sdkBrowsingHttp2ProviderBoundary struct {
	InitialWrites          uint64 `json:"initial_writes"`
	TimeoutResends         uint64 `json:"timeout_resends"`
	AdmissionWaitNs        int64  `json:"admission_wait_ns"`
	Unacked                uint64 `json:"unacked"`
	PackDrops              uint64 `json:"pack_drops"`
	PackWaits              uint64 `json:"pack_waits"`
	PackSaturations        uint64 `json:"pack_saturations"`
	AckBlocked             uint64 `json:"ack_blocked"`
	ReturnStartedEntries   uint64 `json:"return_started_entries"`
	ReturnCompletedPackets int64  `json:"return_completed_packets"`
	ReturnFailures         int64  `json:"return_failures"`
}

type sdkBrowsingHttp2Boundary struct {
	Page            int                                 `json:"page"`
	Device          *sdk.DeviceLocalMemoryUsage         `json:"device"`
	BridgeStarted   uint64                              `json:"bridge_started"`
	BridgeCompleted uint64                              `json:"bridge_completed"`
	BridgeAccepted  uint64                              `json:"bridge_accepted"`
	BridgeRefused   uint64                              `json:"bridge_refused"`
	Providers       [6]sdkBrowsingHttp2ProviderBoundary `json:"providers"`
}

func sdkBrowsingHttp2Capture(fixture *sdkBrowsingSetupFixture, page int) sdkBrowsingHttp2Boundary {
	native := fixture.native.Load()
	result := sdkBrowsingHttp2Boundary{Page: page, Device: fixture.device.MemoryUsed(), BridgeStarted: native.bridgeStarted.Load(),
		BridgeCompleted: native.bridgeCompleted.Load(), BridgeAccepted: native.bridgeAccepted.Load(), BridgeRefused: native.bridgeRefused.Load()}
	for ordinal := range fixture.providers {
		provider := &fixture.providers[ordinal]
		if provider.client == nil {
			continue
		}
		recovery, receive := provider.client.SendRecoveryStats(), provider.client.ReceiveStats()
		row := &result.Providers[ordinal]
		row.InitialWrites, row.TimeoutResends, row.AdmissionWaitNs = recovery.InitialWriteCount, recovery.TimeoutResendWriteCount, int64(recovery.ReliableAdmissionWaitDuration)
		row.Unacked, row.PackDrops, row.PackWaits = receive.ResendQueueUnackedItemCount, receive.PackHandoffDropCount, receive.PackHandoffWaitCount
		row.PackSaturations, row.AckBlocked = receive.PackHandoffSaturationCount, receive.AckRouteWriteBlockedCount
		if provider.returns != nil {
			row.ReturnStartedEntries, row.ReturnCompletedPackets, row.ReturnFailures = provider.returns.startedEntryCount.Load(), provider.returns.completedPacketCount.Load(), provider.returns.failures.Load()
		}
	}
	return result
}

// Changes only the inner protocol and response-consumption policy. Original
// SDK construction, public providers, memory targets and carrier mode remain.
func TestFullTunH1Sdk32PersistentInnerHttp2Browsing100Pages(t *testing.T) {
	if testing.Short() {
		t.Skip("actual SDK/H1/provider topology needs shared PERFVAR services")
	}
	environment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	environment.Run(t, func(t testing.TB) {
		plan := newNativeBrowsingPressurePlan()
		scenario := newSdkBrowsingScenario(plan)
		fixture, err := newSdkBrowsingSetupFixture(t, plan, scenario)
		if err != nil {
			t.Fatal("public SDK32 constructor: ", err)
		}
		defer func() {
			if err := fixture.close(); err != nil {
				t.Error(err)
			}
		}()
		if _, err := fixture.waitInitial(); err != nil {
			t.Fatal(err)
		}
		native := fixture.native.Load()
		native.suppressOriginReadHoldForTest.Store(true)
		serverTls, clientTls, err := nativeBrowsingTlsConfigs()
		if err != nil {
			t.Fatal(err)
		}
		pressure := &nativeBrowsingPressureObservation{Profile: plan.profile, ProviderProfile: plan.provider.Name,
			MaximumLivePages: plan.maxLivePages, HttpConnectionsPerOrigin: nativeBrowsingConnectionsPerOrigin,
			slowNavigation: sdk.DefaultDeviceLocalSettings().SendTimeout,
			PhaseNames:     [8]string{"get_conn", "got_conn", "connect_start", "connect_done", "tls_start", "tls_done", "wrote_request", "first_byte"},
			Scope:          "actual public SDK32/native/provider; outer H1; verified TLS1.3 inner h2; original max4 connections/origin, four live pages, 100HTML/400assets/99image-tail overlaps; prior original JS1KiB body remains unread through same-session next HTML; no application DNS; unchanged 300s application/5s navigation budgets; no H2 flow-window saturation claim"}
		// Canonical private multi-client aggregates are unavailable through SDK.
		pressure.captureBoundaryForTest = func(*fullTunPath, int, *nativeBrowsingPressureObservation) *nativeBrowsingPressureBoundary {
			return nil
		}
		for page := range pressure.Navigations {
			pressure.Navigations[page].ConnectionAcquireNs, pressure.Navigations[page].WrittenToFirstByteNs = -1, -1
			for phase := range pressure.Navigations[page].Phases {
				pressure.Navigations[page].Phases[phase] = -1
			}
		}
		witness := newNativeBrowsingHttp2Witness(pressure)
		defer witness.close()
		settings := plan.runSettings(pressure)
		settings.innerHttp2ForTest = true
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
			page, slot, _, parseErr := nativeBrowsingHttp2RequestSlot(target)
			if parseErr != nil || slot != nativeBrowsingAssetCount {
				return
			}
			row := sdkBrowsingHttp2Capture(fixture, page)
			h2, _ := witness.snapshot()
			if result.err != nil || int64(pressure.slowNavigation) <= h2.CombinedFirstByteNs[page] {
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
		verdictErr := error(nil)
		if workloadErr == nil {
			verdictErr = errors.Join(witness.verdict(sdk.DefaultDeviceLocalSettings().SendTimeout), nativeBrowsingPressureLatencyError(observation), nativeBrowsingAssetBodyOwnershipError(pressure))
		}
		h2, witnessErr := witness.snapshot()
		closeErr := fixture.close()
		observation.Drain = nativeBrowsingMemoryNow()
		report := struct {
			Browsing    *nativeBrowsingObservation     `json:"browsing"`
			Http2       nativeBrowsingHttp2Observation `json:"inner_http2"`
			LastHealthy *sdkBrowsingHttp2Boundary      `json:"last_healthy,omitempty"`
			FirstSlow   *sdkBrowsingHttp2Boundary      `json:"first_slow_or_error,omitempty"`
			AfterJoin   *sdk.DeviceLocalMemoryUsage    `json:"after_join"`
		}{Browsing: observation, Http2: h2, LastHealthy: lastHealthy, FirstSlow: firstSlow, AfterJoin: fixture.afterJoin}
		encoded, encodeErr := json.Marshal(report)
		if encodeErr != nil || nativeBrowsingPressureReportByteLimit < len(encoded) {
			t.Error("bounded inner-H2 report: ", encodeErr, " bytes=", len(encoded))
		} else {
			t.Logf("[perfvar] sdk-native-inner-http2 %s", encoded)
		}
		if err := errors.Join(workloadErr, verdictErr, witnessErr, closeErr); err != nil {
			t.Fatal("SDK32 inner-H2 browsing: ", err)
		}
	})
}
