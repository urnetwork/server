//go:build linux

// Composed finite-role tests retain real journals, build acknowledgments and
// HTTP admission while replacing only native build/kernel/daemon boundaries.
package competitionagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Old isolated proxy fixtures omit the prospective required policy; this one
// opts into every real retirement gate and records real completed build bytes.
func newBuildRetirementProxyTest(t *testing.T) (*dockerProxyTest, *buildRetirementTestKernel, *atomic.Int64) {
	t.Helper()
	fixture := newDockerProxyTest(t)
	resource := testRunResourceIdentity()
	resource.EvidenceMemoryLimitBytes = 5 * 1024 * 1024 * 1024
	policy, err := newBuildRetirementPolicy(runSliceResources{ResourcePolicySha256: fixture.plan.ResourcePolicySha256, Identity: resource,
		BuildTasksMaximum: 4096, CpuPeriodMicros: 100000}, fixture.plan.BuildCgroupName, int64(os.Getpagesize()))
	if err != nil {
		t.Fatal(err)
	}
	fixture.plan.BuildRetirement = policy
	fixture.proxy.plan.BuildRetirement = policy
	if err := validateDockerProxyPlan(fixture.plan, fixture.settings, false); err != nil {
		t.Fatal(err)
	}
	kernel := &buildRetirementTestKernel{events: &cleanupTestEvents{}, memory: testBuildRetirementMemory()}
	fixture.proxy.buildRetirer = &hostBuildRetirement{settings: DefaultBuildRetirementSettings(), kernel: kernel}
	fixture.proxy.hostFiles = &hostFileBroker{plan: fixture.plan, settings: DefaultHostFilesSettings()}
	calls := &atomic.Int64{}
	fixture.proxy.buildRunner = func(context.Context, *dockerProxyBuild) DockerBuildResult {
		calls.Add(1)
		return DockerBuildResult{Acknowledged: true, ExitCode: 0}
	}
	return fixture, kernel, calls
}

// This is the same no-argument finite host operation used by the trusted helper.
func requestBuildRetirementTest(t *testing.T, fixture *dockerProxyTest) (*httptest.ResponseRecorder, HostFilesResult) {
	t.Helper()
	body, err := json.Marshal(HostFilesRequest{Schema: Schema, RunId: fixture.plan.RunId, Operation: "retire-build"})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	fixture.proxy.workerHandler(response, httptest.NewRequest(http.MethodPost, "http://worker/_urnetwork/host-files", bytes.NewReader(body)))
	result := HostFilesResult{}
	if response.Code == http.StatusOK && decodeStrict(response.Body.Bytes(), &result) != nil {
		t.Fatal("retirement returned an unbounded or malformed result")
	}
	return response, result
}

// A real synced result record must precede retirement, not merely RPC headers.
func acknowledgeBuildRetirementTest(t *testing.T, fixture *dockerProxyTest) {
	t.Helper()
	result, err := fixture.proxy.RunOwnedBuild(t.Context(), fixture.buildRequest)
	if err != nil || !result.Acknowledged || result.ExitCode != 0 {
		t.Fatal("synthetic build did not publish a completed acknowledgment", err)
	}
}

// All measurement parents refuse create before retirement while preflight
// remains available. Durable retirement authorizes create only with fresh proof.
func TestBuildRetirementGateBlocksMeasurementUntilDurableNativeProof(t *testing.T) {
	fixture, kernel, calls := newBuildRetirementProxyTest(t)
	kernel.before = func(_ context.Context, operation string) error {
		if !fixture.proxy.stateLock.TryLock() {
			return errors.New("native retirement waited while holding owner state publication lock")
		}
		fixture.proxy.stateLock.Unlock()
		if operation == "freeze" {
			if _, err := os.Stat(filepath.Join(fixture.plan.StateDirectory, "build-retiring.json")); err != nil {
				return errors.New("native retirement preceded its durable owner/build intent")
			}
		}
		return nil
	}
	upstream := dockerPolicyTestDaemon(t, fixture)
	if response := dockerPolicyTestCreate(t, fixture, nil); response.Code != http.StatusCreated {
		t.Fatal("retirement blocked bounded preflight")
	}
	for _, name := range fixture.plan.CgroupNames {
		if strings.HasSuffix(name, "-preflight.slice") {
			continue
		}
		response := dockerPolicyTestCreate(t, fixture, func(object map[string]any) { object["HostConfig"].(map[string]any)["CgroupParent"] = name })
		if response.Code == http.StatusCreated || upstream.Load() != 1 {
			t.Fatal("measurement reached daemon before build retirement", name)
		}
	}
	acknowledgeBuildRetirementTest(t, fixture)
	response, result := requestBuildRetirementTest(t, fixture)
	if response.Code != http.StatusOK || !result.Acknowledged || result.ExitCode != 0 || result.BuildRetirement == nil ||
		result.BuildRetirement.RetainedMemoryMaximumBytes != 3*1024*1024*1024 || !digestPattern.MatchString(result.BuildRetirement.ReceiptSha256) {
		t.Fatal("retirement did not retain a finite bounded native receipt", response.Code, result)
	}
	name := fixture.plan.CgroupNames[1]
	response = dockerPolicyTestCreate(t, fixture, func(object map[string]any) { object["HostConfig"].(map[string]any)["CgroupParent"] = name })
	if response.Code != http.StatusCreated || upstream.Load() != 2 {
		t.Fatal("verified retirement did not authorize measured create", response.Code)
	}
	kernel.stateLock.Lock()
	kernel.limit.MemoryBytes++
	kernel.stateLock.Unlock()
	response = dockerPolicyTestCreate(t, fixture, func(object map[string]any) { object["HostConfig"].(map[string]any)["CgroupParent"] = name })
	if response.Code == http.StatusCreated || upstream.Load() != 2 {
		t.Fatal("old receipt hid a changed kernel retention cap")
	}
	if _, err := fixture.proxy.RunOwnedBuild(t.Context(), fixture.buildRequest); err == nil || calls.Load() != 1 {
		t.Fatal("retirement reopened build admission")
	}
}

// The final authoritative clock is resampled after native retirement. A late
// manager/kernel ACK cannot publish measurement authority or reopen the build.
func TestBuildRetirementCannotPublishAfterOriginalDeadline(t *testing.T) {
	fixture, kernel, calls := newBuildRetirementProxyTest(t)
	acknowledgeBuildRetirementTest(t, fixture)
	now := fixture.plan.DeadlineAt.Add(-time.Second)
	fixture.proxy.settings.Now = func() time.Time { return now }
	kernel.before = func(_ context.Context, operation string) error {
		if operation == "memory" {
			now = fixture.plan.DeadlineAt
		}
		return nil
	}
	_, result := requestBuildRetirementTest(t, fixture)
	if result.Acknowledged || !fixture.proxy.buildAdmissionsClosed || calls.Load() != 1 {
		t.Fatal("late native completion became measurement or rebuild authority")
	}
	if _, err := os.Stat(filepath.Join(fixture.plan.StateDirectory, "build-retired.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("retirement published past its original immutable deadline")
	}
}

// Receipt custody is tied to the exact root owner plan and original build ACK.
// A different scope or altered retained journal cannot replay an old proof.
func TestBuildRetirementRecoveryRejectsAlteredOwnerOrBuildAckTuple(t *testing.T) {
	fixture, _, _ := newBuildRetirementProxyTest(t)
	acknowledgeBuildRetirementTest(t, fixture)
	_, result := requestBuildRetirementTest(t, fixture)
	if !result.Acknowledged {
		t.Fatal("control retirement did not complete")
	}
	if err := fixture.proxy.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(fixture.plan.StateDirectory, "build-retiring.json")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	intent := buildRetirementIntent{}
	if json.Unmarshal(content, &intent) != nil {
		t.Fatal("control retirement intent is malformed")
	}
	intent.BuildAckSha256 = strings.Repeat("e", 64)
	content, err = json.Marshal(intent)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
	if recovered, err := newDockerProxy(fixture.plan, fixture.settings, false); err == nil {
		_ = recovered.Close()
		t.Fatal("altered build acknowledgment replayed a previously authorized retirement")
	}
}

// A successful retirement reattachment does no native mutation and no rebuild.
// Restart recovery binds the same original deadline, owner plan and build ACK.
func TestBuildRetirementReceiptReplayAndRecoveryNeverRebuildOrResetClock(t *testing.T) {
	fixture, kernel, calls := newBuildRetirementProxyTest(t)
	acknowledgeBuildRetirementTest(t, fixture)
	_, first := requestBuildRetirementTest(t, fixture)
	_, replay := requestBuildRetirementTest(t, fixture)
	if first.BuildRetirement == nil || replay.BuildRetirement == nil || *first.BuildRetirement != *replay.BuildRetirement {
		t.Fatal("retirement replay changed its committed proof")
	}
	before := kernel.events.snapshot()
	if err := fixture.proxy.Close(); err != nil {
		t.Fatal(err)
	}
	recovered, err := newDockerProxy(fixture.plan, fixture.settings, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recovered.Close() })
	recovered.buildRetirer = &hostBuildRetirement{settings: DefaultBuildRetirementSettings(), kernel: kernel}
	if !recovered.plan.DeadlineAt.Equal(fixture.plan.DeadlineAt) || !recovered.buildAdmissionsClosed {
		t.Fatal("retirement recovery reset original custody/clock")
	}
	if err := recovered.requireBuildRetired(t.Context(), fixture.plan.CgroupNames[1]); err != nil {
		t.Fatal("recovered exact receipt lost native verification", err)
	}
	for _, event := range kernel.events.snapshot()[len(before):] {
		if event == "freeze" || event == "kill" || event == "lower" {
			t.Fatal("receipt recovery repeated destructive retirement")
		}
	}
	if _, err := recovered.RunOwnedBuild(t.Context(), fixture.buildRequest); err == nil || calls.Load() != 1 {
		t.Fatal("recovered receipt reran build")
	}
}

// Failed/unknown retirement remains held in memory and on restart. Lowering
// may have happened, but no failure can become permission to rebuild or measure.
func TestBuildRetirementFailureNeverReopensAfterRetryOrOwnerRestart(t *testing.T) {
	fixture, kernel, calls := newBuildRetirementProxyTest(t)
	acknowledgeBuildRetirementTest(t, fixture)
	kernel.before = func(_ context.Context, operation string) error {
		if operation == "lower" {
			return errors.New("synthetic lost manager acknowledgment")
		}
		return nil
	}
	_, result := requestBuildRetirementTest(t, fixture)
	if result.Acknowledged {
		t.Fatal("unknown retirement was acknowledged")
	}
	if _, err := os.Stat(filepath.Join(fixture.plan.StateDirectory, "build-retiring.json")); err != nil {
		t.Fatal("native retirement preceded durable closed-build intent", err)
	}
	if _, err := os.Stat(filepath.Join(fixture.plan.StateDirectory, "build-retired.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed retirement published completion")
	}
	if _, err := fixture.proxy.RunOwnedBuild(t.Context(), fixture.buildRequest); err == nil {
		t.Fatal("failed retirement reopened build")
	}
	if err := fixture.proxy.Close(); err != nil {
		t.Fatal(err)
	}
	recovered, err := newDockerProxy(fixture.plan, fixture.settings, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recovered.Close() })
	if !recovered.buildAdmissionsClosed || recovered.requireBuildRetired(t.Context(), fixture.plan.CgroupNames[1]) == nil {
		t.Fatal("interrupted retirement became measurement permission")
	}
	if _, err := recovered.RunOwnedBuild(t.Context(), fixture.buildRequest); err == nil || calls.Load() != 1 {
		t.Fatal("interrupted retirement rebuilt or reset attempt")
	}
}

// Build headers alone do not count. Refusing this finite request is permanent
// within the owner; no native action or receipt can be manufactured from it.
func TestBuildRetirementRejectsMissingCompletedSyncedBuildAck(t *testing.T) {
	fixture, kernel, calls := newBuildRetirementProxyTest(t)
	done := make(chan struct{})
	close(done)
	// In-memory success alone is not a durable terminal build record.
	fixture.proxy.build = &dockerProxyBuild{issued: true, finished: true, requestSha256: strings.Repeat("a", 64),
		result: DockerBuildResult{Acknowledged: true, ExitCode: 0}, done: done}
	_, result := requestBuildRetirementTest(t, fixture)
	if result.Acknowledged || len(kernel.events.snapshot()) != 0 {
		t.Fatal("retirement acted without completed synced build ACK")
	}
	if _, err := fixture.proxy.RunOwnedBuild(t.Context(), fixture.buildRequest); err == nil || calls.Load() != 0 {
		t.Fatal("failed retirement allowed a later first build")
	}
}

// An authenticated whole-image cache hit issues no build in this run. Honest
// no-issued provenance still freezes, kills, caps and verifies the exact parent.
func TestBuildRetirementAcceptsExplicitNoBuildIssuedAndRecoversCacheProof(t *testing.T) {
	fixture, kernel, calls := newBuildRetirementProxyTest(t)
	_, result := requestBuildRetirementTest(t, fixture)
	if !result.Acknowledged || result.BuildRetirement == nil || result.BuildRetirement.OutcomeKind != "no-build-issued" || calls.Load() != 0 {
		t.Fatal("whole-image cache provenance was stranded or falsely called an acknowledged build", result)
	}
	content, err := os.ReadFile(filepath.Join(fixture.plan.StateDirectory, "build-retiring.json"))
	intent := buildRetirementIntent{}
	if err != nil || json.Unmarshal(content, &intent) != nil || intent.OutcomeKind != "no-build-issued" || intent.BuildAckSha256 != "" {
		t.Fatal("cache provenance manufactured a build acknowledgment")
	}
	if _, err := fixture.proxy.RunOwnedBuild(t.Context(), fixture.buildRequest); err == nil {
		t.Fatal("cache retirement allowed a later first build")
	}
	if err := fixture.proxy.Close(); err != nil {
		t.Fatal(err)
	}
	recovered, err := newDockerProxy(fixture.plan, fixture.settings, false)
	if err != nil {
		t.Fatal("durable no-issued provenance did not survive exact owner recovery", err)
	}
	t.Cleanup(func() { _ = recovered.Close() })
	recovered.buildRetirer = &hostBuildRetirement{settings: DefaultBuildRetirementSettings(), kernel: kernel}
	if err := recovered.requireBuildRetired(t.Context(), fixture.plan.CgroupNames[1]); err != nil {
		t.Fatal(err)
	}
	if _, err := recovered.RunOwnedBuild(t.Context(), fixture.buildRequest); err == nil || calls.Load() != 0 {
		t.Fatal("cache recovery issued a build or reset its attempt")
	}
}

// Absence is positive provenance only when every owner journal/session is
// clean. A partial directory, existing record or unknown intent cannot qualify.
func TestBuildRetirementNoIssuedRejectsPartialOrUnknownBuildProvenance(t *testing.T) {
	for _, fault := range []string{"directory", "record", "unknown"} {
		fixture, kernel, calls := newBuildRetirementProxyTest(t)
		switch fault {
		case "directory":
			if err := os.Mkdir(fixture.proxy.buildDirectory(), 0700); err != nil {
				t.Fatal(err)
			}
		case "record":
			if err := os.WriteFile(filepath.Join(fixture.plan.StateDirectory, "build.json"), []byte("{}"), 0600); err != nil {
				t.Fatal(err)
			}
		case "unknown":
			fixture.proxy.stateLock.Lock()
			fixture.proxy.unknown, fixture.proxy.accepting = 1, false
			fixture.proxy.stateLock.Unlock()
		}
		_, result := requestBuildRetirementTest(t, fixture)
		if result.Acknowledged || len(kernel.events.snapshot()) != 0 {
			t.Fatal("partial or unknown build became no-issued provenance", fault)
		}
		if _, err := fixture.proxy.RunOwnedBuild(t.Context(), fixture.buildRequest); err == nil || calls.Load() != 0 {
			t.Fatal("unproven cache retirement allowed a new build", fault)
		}
	}
}

// Read-only build RPCs also own admission through their terminal body. A
// completed build cannot start another read behind the retirement handoff.
func TestBuildRetirementHandoffRefusesLateBuildReadBeforeDaemonDial(t *testing.T) {
	fixture, _, _ := newBuildRetirementProxyTest(t)
	acknowledgeBuildRetirementTest(t, fixture)
	fixture.proxy.stateLock.Lock()
	fixture.proxy.fileHandoff = true
	fixture.proxy.stateLock.Unlock()
	var dials atomic.Int64
	fixture.proxy.dialDaemon = func(context.Context) (net.Conn, error) {
		dials.Add(1)
		return nil, errors.New("synthetic read crossed retirement handoff")
	}
	response := httptest.NewRecorder()
	fixture.proxy.forwardGrpcRead(response, httptest.NewRequest(http.MethodPost, "http://owner/moby.buildkit.v1.Control/Info", nil))
	if response.Code != http.StatusServiceUnavailable || dials.Load() != 0 {
		t.Fatal("late read bypassed owner handoff admission", response.Code)
	}
	response = httptest.NewRecorder()
	fixture.proxy.serveGrpcUpgrade(response, httptest.NewRequest(http.MethodPost, "http://owner/grpc", nil))
	if response.Code != http.StatusServiceUnavailable || dials.Load() != 0 {
		t.Fatal("delayed build protocol prelude crossed the retirement handoff", response.Code)
	}
	response = httptest.NewRecorder()
	fixture.proxy.relayUpgrade(response, httptest.NewRequest(http.MethodPost, "http://owner/session", nil), true)
	if response.Code != http.StatusServiceUnavailable || dials.Load() != 0 {
		t.Fatal("delayed root session crossed the retirement handoff", response.Code)
	}
}

// Fixed operation requests cannot select a path, target, unrelated run or cap.
func TestBuildRetirementFiniteRequestAndProtectedPolicyAreMandatory(t *testing.T) {
	fixture, kernel, _ := newBuildRetirementProxyTest(t)
	for _, mutate := range []func(*HostFilesRequest){
		func(r *HostFilesRequest) { r.Path = "/synthetic/foreign" },
		func(r *HostFilesRequest) { r.ContainerId = strings.Repeat("a", 64) },
		func(r *HostFilesRequest) { r.NetworkId = strings.Repeat("b", 64) },
		func(r *HostFilesRequest) { r.RunId = strings.Repeat("c", 64) },
	} {
		request := HostFilesRequest{Schema: Schema, RunId: fixture.plan.RunId, Operation: "retire-build"}
		mutate(&request)
		if fixture.proxy.validateBuildRetirementRequest(request) == nil {
			t.Fatal("retirement expanded into a caller-selected host operation")
		}
	}
	if len(kernel.events.snapshot()) != 0 {
		t.Fatal("invalid typed request touched native boundary")
	}
	plan := fixture.plan
	plan.BuildRetirement = nil
	if validateDockerBuildRetirementPlan(plan, true) == nil {
		t.Fatal("protected executor omitted mandatory retirement gate")
	}
}

// The retirement handoff joins a previously admitted real HTTP mutation at an
// explicit daemon barrier. Neither a pending upload nor a global state lock can
// be confused with completed daemon custody.
func TestBuildRetirementJoinsPriorMutationBeforeNativeWork(t *testing.T) {
	fixture, kernel, _ := newBuildRetirementProxyTest(t)
	acknowledgeBuildRetirementTest(t, fixture)
	entered, release, handoff := make(chan struct{}), make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	fixture.daemon(t, func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
		}
		w.WriteHeader(http.StatusCreated)
	})
	fixture.proxy.beforeHandoffWait = func() { close(handoff) }
	mutationDone, retirementDone := make(chan struct{}), make(chan struct{})
	go func() { defer close(mutationDone); dockerPolicyTestCreate(t, fixture, nil) }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("daemon mutation did not enter barrier")
	}
	var response *httptest.ResponseRecorder
	var result HostFilesResult
	go func() { defer close(retirementDone); response, result = requestBuildRetirementTest(t, fixture) }()
	var releaseOnce sync.Once
	t.Cleanup(func() {
		cancel()
		releaseOnce.Do(func() { close(release) })
		for _, done := range []<-chan struct{}{mutationDone, retirementDone} {
			select {
			case <-done:
			case <-time.After(30 * time.Second):
				t.Error("retirement owner did not join")
			}
		}
	})
	select {
	case <-handoff:
	case <-ctx.Done():
		t.Fatal("retirement did not close mutation handoff")
	}
	if len(kernel.events.snapshot()) != 0 {
		t.Fatal("native retirement crossed unacknowledged daemon request")
	}
	// Snapshot obtains the same real state lock while retirement waits.
	if snapshot := fixture.proxy.Snapshot(); snapshot.Inflight < 2 {
		t.Fatal("retirement failed to retain both outstanding owners")
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case <-retirementDone:
	case <-ctx.Done():
		t.Fatal("retirement did not join completed daemon request")
	}
	if response.Code != http.StatusOK || !result.Acknowledged {
		t.Fatal("joined retirement did not complete", response.Code, result)
	}
}
