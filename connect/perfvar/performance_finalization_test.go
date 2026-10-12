// These tests drive measured-run finalization through real full-TUN ownership.
// Injected cleanup results use existing construction hooks; no network failure,
// sleep, retry, or sticky testing.TB failure bit supplies the causal oracle.
package perfvar

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect"
	"github.com/urnetwork/server"
)

// Capture only the explicitly expected cleanup reports. All other failures
// retain the real TestEnv reporting behavior, including unexpected timeouts.
type perfvarFinalizationReports struct {
	testing.TB
	stateLock  sync.Mutex
	pathReport string
	poolPrefix string
	reports    []string
}

// Expected reports remain visible without failing a test that deliberately
// injects them. The actual failing campaign separately proves TestEnv forwarding.
func (self *perfvarFinalizationReports) Errorf(format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	if message != self.pathReport &&
		(self.poolPrefix == "" || !strings.HasPrefix(message, self.poolPrefix)) {
		self.TB.Errorf("%s", message)
		return
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.reports = append(self.reports, message)
	}()
	self.TB.Logf("expected finalization report: %s", message)
}

// Test reads occur only after the measured call has joined every owner.
func (self *perfvarFinalizationReports) snapshot() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]string(nil), self.reports...)
}

// One case chooses an existing lifecycle boundary, never a mocked measured
// result. A retained test-owned pool buffer forces environment reconciliation.
type perfvarFinalizationCase struct {
	name             string
	topology         string
	primary          string
	apiError         bool
	providerError    bool
	environmentError bool
}

// Real measurement, construction rollback, path close, and normal or split
// environment close all run before ownership and record assertions.
func testPerfvarRunFinalization(t testing.TB, c perfvarFinalizationCase) perfvarRunRecord {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	poolBefore := routeMessagePoolOutstanding()
	if newFullTunConstructionHooksForTest != nil {
		t.Fatal("finalization fixture requires an uninstalled construction hook")
	}
	profile := initialNetworkProfiles(9137)["clean-lan"]
	profile.Name = "fixture-finalization"
	profile.SourceNote = "Synthetic loss-free finalization correctness fixture, not a performance cell."
	// Pacing keeps a real 64 KiB bulk interval long enough for the unchanged
	// offered-probe sampler, without synthetic drops or altered probe deadlines.
	profile.Forward.RateBitsPerSecond = 4 * 1024 * 1024
	profile.Reverse.RateBitsPerSecond = 4 * 1024 * 1024
	profile.Forward.BurstByteCount = 1500
	profile.Reverse.BurstByteCount = 1500
	providerProfile := initialNetworkProfiles(9138)["clean-lan"]
	scenario := perfvarScenario{
		Route:                 fullTunRouteExchangeH1,
		Profile:               profile,
		ProviderAccessProfile: providerProfile,
		Workload:              perfvarWorkloadLatencyUnderLoad,
		Direction:             perfvarDirectionDownload,
		Topology:              c.topology,
		Resource:              perfvarResourceDefault,
		ApplicationMtu:        1100,
		Seed:                  9137,
		RunCount:              1,
		PayloadByteCount:      64 * 1024,
		FlowCount:             1,
	}
	if c.topology == perfvarTopologySplitExchange {
		internalProfile := initialNetworkProfiles(9139)["clean-lan"]
		scenario.InternalExchangeProfile = &internalProfile
	}
	if c.primary != "" {
		scenario.Workload = perfvarWorkloadTCP
	}
	apiErr := errors.New("fixture identity retirement failure")
	providerErr := errors.New("fixture provider cleanup failure")
	primaryErr := errors.New("fixture primary run failure")
	pathReasons := []string{}
	if c.apiError {
		pathReasons = append(pathReasons, fmt.Sprintf(
			"join %s during full-TUN teardown: %v",
			fullTunConstructionResourceApiGenerator, apiErr,
		))
	}
	if c.providerError {
		pathReasons = append(pathReasons, fmt.Sprintf(
			"join %s during full-TUN teardown: %v",
			fullTunConstructionResourceProviderClient, providerErr,
		))
	}
	reports := &perfvarFinalizationReports{TB: t}
	if len(pathReasons) != 0 && c.primary != "route-readiness" {
		reports.pathReport = "full-TUN teardown: " + strings.Join(pathReasons, "\n")
	}
	if c.environmentError {
		reports.poolPrefix = "route message-pool ownership did not reconcile: "
		if c.topology == perfvarTopologySplitExchange {
			reports.poolPrefix = "split exchange message-pool ownership did not reconcile: "
		}
	}
	var capturedPath *fullTunPath
	var ownedBuffer []byte
	defer func() {
		if ownedBuffer != nil {
			clientconnect.MessagePoolReturn(ownedBuffer)
		}
	}()
	cleanupCounts := map[fullTunConstructionResource]int{}
	var cleanupLock sync.Mutex
	var verificationHookCount int
	newFullTunConstructionHooksForTest = func() *fullTunConstructionTestHooks {
		return &fullTunConstructionTestHooks{
			afterStage: func(stage fullTunConstructionStage, path *fullTunPath) error {
				if stage != fullTunConstructionStageRouteReady {
					return nil
				}
				if capturedPath != nil {
					return errors.New("finalization fixture constructed more than one ready path")
				}
				capturedPath = path
				if c.environmentError {
					ownedBuffer = clientconnect.MessagePoolGet(1)
				}
				path.constructionCleanupErrorForTest = func(resource fullTunConstructionResource) error {
					switch resource {
					case fullTunConstructionResourceApiGenerator:
						if c.apiError {
							return apiErr
						}
					case fullTunConstructionResourceProviderClient:
						if c.providerError {
							return providerErr
						}
					}
					return nil
				}
				path.afterConstructionCleanupForTest = func(resource fullTunConstructionResource) {
					cleanupLock.Lock()
					defer cleanupLock.Unlock()
					cleanupCounts[resource] += 1
				}
				switch c.primary {
				case "route-readiness":
					return primaryErr
				case "measurement-boundary":
					path.readinessAppFence.Store(false)
				case "workload":
					path.beforeWorkloadClientDialForTest = func(context.Context, string) error {
						return primaryErr
					}
				case "verification":
					// The TCP workload joins its application owners before this
					// existing post-workload hook. Only later route verification
					// reads the deliberately wrong fixture route.
					path.afterCarrierEndCandidateForTest = func(int) {
						if path.workloadProgressBytes.Load() < scenario.PayloadByteCount {
							return
						}
						verificationHookCount += 1
						path.route = fullTunRoute("fixture-verification")
					}
				}
				return nil
			},
		}
	}
	defer func() { newFullTunConstructionHooksForTest = nil }()
	record, err := measurePerfvarRun(ctx, reports, scenario, 1)
	// This owner, not the environment, returns the deliberate reconciliation
	// witness. Do so before any assertion can terminate the test goroutine.
	poolAtReturn := routeMessagePoolOutstanding()
	witnessReturned := true
	if ownedBuffer != nil {
		witnessReturned = clientconnect.MessagePoolReturn(ownedBuffer)
		ownedBuffer = nil
	}
	if err != nil {
		t.Fatalf("%s measurement returned outer error: %v", c.name, err)
	}
	if capturedPath == nil {
		t.Fatalf("%s did not reach the actual ready full-TUN path: %+v", c.name, record)
	}
	cleanupLock.Lock()
	finalCleanupCounts := make(map[fullTunConstructionResource]int, len(cleanupCounts))
	for resource, count := range cleanupCounts {
		finalCleanupCounts[resource] = count
	}
	cleanupLock.Unlock()
	assertFullTunConstructionRolledBack(t, c.name, capturedPath, finalCleanupCounts, []fullTunConstructionResource{
		fullTunConstructionResourceAppTun,
		fullTunConstructionResourceBridge,
		fullTunConstructionResourceMultiClient,
		fullTunConstructionResourceApiGenerator,
		fullTunConstructionResourceDeviceTransports,
		fullTunConstructionResourceProviderRemoteNat,
		fullTunConstructionResourceProviderLocalNat,
		fullTunConstructionResourceProviderClient,
		fullTunConstructionResourceProviderTransport,
		fullTunConstructionResourceProviderCarrierTun,
		fullTunConstructionResourceDeviceCarrierTun,
		fullTunConstructionResourceNoAckTracker,
		fullTunConstructionResourceNoAckTracker,
		fullTunConstructionResourcePackTracker,
		fullTunConstructionResourcePackTracker,
		fullTunConstructionResourceReturnTracker,
	})
	if capturedPath.environment.ctx.Err() == nil || capturedPath.environment.network.ctx.Err() == nil {
		t.Fatalf("%s environment ownership remained open", c.name)
	}
	for _, link := range capturedPath.environment.network.directionalLinks() {
		select {
		case <-link.done:
		default:
			t.Fatalf("%s network link remained live after finalization", c.name)
		}
	}
	poolAfter, balanced := routeMessagePoolBalance(poolBefore)
	wantOwned := int64(0)
	if c.environmentError {
		wantOwned = 1
	}
	if !witnessReturned || poolAtReturn != poolBefore+wantOwned || !balanced {
		t.Fatalf("%s pool ownership before=%d returned=%d final=%d want-witness=%d classes=%v",
			c.name, poolBefore, poolAtReturn, poolAfter.outstanding, wantOwned, poolAfter.classes)
	}
	actualReports := reports.snapshot()
	wantReportCount := 0
	if reports.pathReport != "" {
		wantReportCount += 1
	}
	if reports.poolPrefix != "" {
		wantReportCount += 1
	}
	if len(actualReports) != wantReportCount {
		t.Fatalf("%s cleanup reports=%q, want %d", c.name, actualReports, wantReportCount)
	}
	if reports.pathReport != "" && actualReports[0] != reports.pathReport {
		t.Fatalf("%s path reporting changed: %q", c.name, actualReports)
	}
	if reports.poolPrefix != "" &&
		!strings.HasPrefix(actualReports[len(actualReports)-1], reports.poolPrefix) {
		t.Fatalf("%s environment report absent or reordered: %q", c.name, actualReports)
	}
	if record.RunIndex != 1 || record.ScenarioHash == "" || record.ProfileHash == "" ||
		record.Scenario.Topology != scenario.Topology || record.RouteSetupDuration <= 0 ||
		record.GoroutinesAfter <= 0 || record.Underlay.UsefulByteCount != scenario.PayloadByteCount ||
		record.Underlay.Duration <= 0 ||
		(scenario.Workload == perfvarWorkloadTCP && record.Underlay.ContentHash == "") {
		t.Fatalf("%s lost calibration, identity, or finalized lifecycle observations: %+v", c.name, record)
	}
	completedWorkload := c.primary == "" || c.primary == "verification"
	if completedWorkload {
		if record.Tunneled.UsefulByteCount != scenario.PayloadByteCount ||
			record.Tunneled.ContentHash == "" || record.Tunneled.Duration <= 0 ||
			record.Carrier.WireByteCount == 0 || record.Memory.SampleCount < 2 ||
			len(record.Progress.Samples) < 2 {
			t.Fatalf("%s lost completed workload/carrier/progress/memory observations: %+v", c.name, record)
		}
	}
	if scenario.Workload == perfvarWorkloadLatencyUnderLoad {
		// Direct calibration constructs a count-only bulk result with 12 probes
		// per idle/post phase; full-TUN retains its TCP hash and uses 8 per phase.
		for _, measurement := range []struct {
			name              string
			result            workloadResult
			phaseAttemptCount int
		}{
			{name: "underlay", result: record.Underlay, phaseAttemptCount: 12},
			{name: "tunneled", result: record.Tunneled, phaseAttemptCount: 8},
		} {
			result := measurement.result
			if result.LoadedProbeMeasurementVersion != loadedLatencyProbeMeasurementVersion ||
				result.IdleProbeAttemptCount != measurement.phaseAttemptCount ||
				result.PostLoadProbeAttemptCount != measurement.phaseAttemptCount ||
				result.LoadedProbeSuccessCount < minimumLatencyProbeSuccessCount ||
				result.LoadedProbeAttemptCount != result.LoadedProbeSuccessCount+result.LoadedProbeFailureCount ||
				result.IdleProbeAttemptCount != result.IdleProbeSuccessCount+result.IdleProbeFailureCount ||
				result.PostLoadProbeAttemptCount != result.PostLoadProbeSuccessCount+result.PostLoadProbeFailureCount ||
				result.LoadedLatency.P95 <= 0 {
				t.Fatalf("%s %s lost latency observations or full offer dispositions: %+v", c.name, measurement.name, result)
			}
		}
	}
	if c.primary == "verification" && verificationHookCount == 0 {
		t.Fatal("verification failure did not cross the actual post-workload boundary")
	}
	expectedStage := c.primary
	if expectedStage == "" && (c.apiError || c.providerError || c.environmentError) {
		expectedStage = "teardown"
	}
	if expectedStage == "" {
		if !record.Correct || record.FailureStage != "" || record.FailureReason != "" {
			t.Errorf("%s clean finalization incorrectly failed the record: %+v", c.name, record)
		}
		return record
	}
	// These are the causal assertions, after exact cleanup and pool recovery.
	// Original source reports the injected errors but omits them from the record.
	if record.Correct || record.FailureStage != expectedStage {
		t.Errorf("%s cleanup result was not reflected in measured record: correct=%t stage=%q want=%q reason=%q",
			c.name, record.Correct, record.FailureStage, expectedStage, record.FailureReason)
	}
	for _, reason := range pathReasons {
		if !strings.Contains(record.FailureReason, reason) {
			t.Errorf("%s measured record omitted cleanup reason %q: %q", c.name, reason, record.FailureReason)
		}
	}
	if reports.poolPrefix != "" && !strings.Contains(record.FailureReason, actualReports[len(actualReports)-1]) {
		t.Errorf("%s measured record omitted environment cleanup result: %q", c.name, record.FailureReason)
	}
	switch c.primary {
	case "route-readiness":
		expectedPrefix := fmt.Sprintf("full-TUN construction stage %s: %v",
			fullTunConstructionStageRouteReady, primaryErr)
		if !strings.HasPrefix(record.FailureReason, expectedPrefix) {
			t.Errorf("%s replaced primary failure: %q", c.name, record.FailureReason)
		}
	case "workload":
		if !strings.HasPrefix(record.FailureReason, primaryErr.Error()) {
			t.Errorf("%s replaced primary failure: %q", c.name, record.FailureReason)
		}
	case "measurement-boundary":
		if !strings.HasPrefix(record.FailureReason, "application readiness: readiness fence is absent") {
			t.Errorf("%s replaced boundary failure: %q", c.name, record.FailureReason)
		}
	case "verification":
		if !strings.HasPrefix(record.FailureReason, "full-TUN P2P route is missing forced platform controllers") {
			t.Errorf("%s replaced verification failure: %q", c.name, record.FailureReason)
		}
	}
	aggregate := aggregatePerfvarRuns([]perfvarRunRecord{record})
	if aggregate.CorrectRunCount != 0 || aggregate.FailureRunCount != 1 ||
		aggregate.ValidRunCount != 0 || aggregate.InvalidRunCount != 0 ||
		len(aggregate.IndividualCorrect) != 1 || aggregate.IndividualCorrect[0] ||
		len(aggregate.IndividualRunValid) != 1 || aggregate.IndividualRunValid[0] {
		t.Errorf("%s aggregate admitted an incorrect run: %+v", c.name, aggregate)
	}
	if aggregate.MemoryMaxBytes != record.Memory.HeapAndStackInuseMax ||
		aggregate.MemorySamplesAboveCeiling != record.Memory.SamplesAboveCeiling ||
		aggregate.WindowCount != record.Progress.WindowCount ||
		aggregate.DeadWindowCount != record.Progress.DeadWindowCount {
		t.Errorf("%s aggregate dropped failed-run memory/progress evidence", c.name)
	}
	shapedRecord := record
	perfvarAnnotateShapedRun(&shapedRecord, perfvarShapedProcess{
		BinarySHA256: strings.Repeat("1", 64),
		PlanSHA256:   strings.Repeat("2", 64),
	})
	shapedAggregate := aggregatePerfvarShapedRuns([]perfvarRunRecord{shapedRecord})
	if shapedRecord.InvalidKind != "incorrect" || shapedAggregate.CorrectRunCount != 0 ||
		shapedAggregate.EligibleRunCount != 0 || len(shapedAggregate.Metrics) != 0 {
		t.Errorf("%s shaped classification admitted teardown failure: kind=%q aggregate=%+v",
			c.name, shapedRecord.InvalidKind, shapedAggregate)
	}
	return record
}

// The actual observed device-generator error must invalidate a completed run
// without deleting successful payload, latency, carrier, or memory evidence.
func TestPerfvarRunRecordsApiGeneratorTeardownFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("requires the real full-TUN TestEnv")
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		testPerfvarRunFinalization(t, perfvarFinalizationCase{
			name: "device generator", topology: perfvarTopologyOneHop, apiError: true,
		})
	})
}

// Normal route teardown returns its existing reconciliation error to the
// measured record rather than only marking testing.TB failed.
func TestPerfvarRunRecordsRouteEnvironmentTeardownFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("requires the real full-TUN TestEnv")
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		testPerfvarRunFinalization(t, perfvarFinalizationCase{
			name: "normal environment", topology: perfvarTopologyOneHop, environmentError: true,
		})
	})
}

// The split owner, not its non-owning route view, supplies the final error.
func TestPerfvarRunRecordsSplitEnvironmentTeardownFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("requires the real full-TUN TestEnv")
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		testPerfvarRunFinalization(t, perfvarFinalizationCase{
			name: "split environment", topology: perfvarTopologySplitExchange, environmentError: true,
		})
	})
}

// One injected path error cannot skip a later independent path error or the
// environment close; each owner still has exactly one terminal disposition.
func TestPerfvarRunCombinesIndependentTeardownFailures(t *testing.T) {
	if testing.Short() {
		t.Skip("requires the real full-TUN TestEnv")
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		testPerfvarRunFinalization(t, perfvarFinalizationCase{
			name: "independent errors", topology: perfvarTopologyOneHop,
			apiError: true, providerError: true, environmentError: true,
		})
	})
}

// Every return after environment construction preserves its primary failure
// while appending operation-local cleanup results and retaining observations.
func TestPerfvarRunKeepsPrimaryFailureDuringTeardown(t *testing.T) {
	if testing.Short() {
		t.Skip("requires the real full-TUN TestEnv")
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		for _, c := range []perfvarFinalizationCase{
			{name: "setup rollback", topology: perfvarTopologySplitExchange, primary: "route-readiness", apiError: true, providerError: true, environmentError: true},
			{name: "initial boundary", topology: perfvarTopologyOneHop, primary: "measurement-boundary", apiError: true, providerError: true, environmentError: true},
			{name: "workload", topology: perfvarTopologyOneHop, primary: "workload", apiError: true, providerError: true, environmentError: true},
			{name: "verification", topology: perfvarTopologyOneHop, primary: "verification", apiError: true, providerError: true, environmentError: true},
		} {
			testPerfvarRunFinalization(t, c)
		}
	})
}

// Successful normal and split finalization retain correct records. A slow
// direct calibration may still set the unchanged, independent headroom gate.
func TestPerfvarRunCleanFinalizationPreservesMeasurements(t *testing.T) {
	if testing.Short() {
		t.Skip("requires the real full-TUN TestEnv")
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		for _, topology := range []string{perfvarTopologyOneHop, perfvarTopologySplitExchange} {
			testPerfvarRunFinalization(t, perfvarFinalizationCase{
				name: "clean " + topology, topology: topology,
			})
		}
	})
}
