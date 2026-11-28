//go:build linux

// Synthetic signed admission reproduces an attempted owner failure before the
// evaluator unit exists. Only native observations are replaced, not cleanup.
package competitionagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Test proof owners expose deterministic failure boundaries, never sleeps.
type legacyPrelaunchTestHost struct {
	placementErr error
	closeErr     error
	verify       func(int) ([]byte, error)
	verifies     int
	closed       bool
}

func (self *legacyPrelaunchTestHost) Placement(context.Context) error { return self.placementErr }
func (self *legacyPrelaunchTestHost) CloseControls(context.Context) error {
	self.closed = true
	return self.closeErr
}
func (self *legacyPrelaunchTestHost) Verify(context.Context) ([]byte, error) {
	self.verifies++
	if !self.closed {
		return nil, errors.New("control admission was not closed")
	}
	if self.verify != nil {
		return self.verify(self.verifies)
	}
	return []byte("synthetic positive closed launch surface"), nil
}

// The old guard remains red for the exact missing-evaluator symptom. The new
// explicit permit is written only after that failure has retained custody.
func newLegacyPrelaunchTestFixture(t *testing.T) (*agentTestFixture, *Agent, LegacyPrelaunchAuthorization, string, string) {
	t.Helper()
	fixture := newAgentTestFixture(t)
	fixture.runner.unknownLaunch = true
	if _, err := fixture.call(t, "start", 1, &fixture.manifest); err != nil {
		t.Fatal(err)
	}
	configBytes, err := json.Marshal(fixture.config)
	if err != nil {
		t.Fatal(err)
	}
	writeRecoveryTestAuthority(t, fixture, fixture.config.configPath, configBytes)
	fixture.config.configSha256 = digestDockerBytes(configBytes)
	fixture.agent.config.configSha256 = fixture.config.configSha256
	agent := recoveryTestAgent(t, fixture)
	agent.runner = &recoveryTestRunner{Runner: fixture.runner, status: &UnitStatus{}}
	if err := agent.RecoverActive(t.Context()); err == nil {
		t.Fatal("ordinary recovery accepted the missing evaluator")
	}
	state, err := agent.loadState(fixture.manifest.Identity.RunId)
	if err != nil || !state.RecoveryActive || state.ExecutionStarted || fixture.cleaner.count() != 0 {
		t.Fatal("old guard did not preserve prelaunch uncertainty", err)
	}
	stateBytes, err := os.ReadFile(filepath.Join(agent.stateDirectory(fixture.manifest.Identity.RunId), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	pin := fixture.config.Agent
	authorization := LegacyPrelaunchAuthorization{Schema: Schema, Kind: "competition-legacy-prelaunch-closure/v1", ApprovalSha256: strings.Repeat("a", 64),
		HostId: fixture.config.HostId, RunId: fixture.manifest.Identity.RunId, StateSha256: digestDockerBytes(stateBytes), ManifestSha256: state.ManifestSha256,
		CleanupPlanSha256: state.CleanupPlanSha256, ClockSha256: digestDockerBytes(recoveryClockBytes(t, fixture)), BootId: "33333333-3333-4333-8333-333333333333",
		OriginalConfig: Pin{Path: fixture.config.configPath, Sha256: fixture.config.configSha256}, OriginalRecoveryServiceArchive: fixture.config.RecoveryService,
		RecoveryAgent: pin, RecoveryService: fixture.config.RecoveryService, IngressWrapper: pin, OriginalIngressWrapper: pin, OriginalIngressWrapperArchive: pin,
		AuthorizedKeys: pin, Sshd: pin, ControlUser: "synthetic-control", OwnerStartDenial: pin}
	authorizationBytes, err := json.Marshal(authorization)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(fixture.config.BaseDirectory, "legacy-authorization.json")
	writeRecoveryTestAuthority(t, fixture, path, authorizationBytes)
	return fixture, agent, authorization, path, digestDockerBytes(authorizationBytes)
}

func TestLegacyPrelaunchClosesMissingEvaluatorOnlyAfterFullCleanup(t *testing.T) {
	fixture, agent, authorization, path, digest := newLegacyPrelaunchTestFixture(t)
	clock := recoveryClockBytes(t, fixture)
	manifestBytes, err := os.ReadFile(filepath.Join(agent.stateDirectory(authorization.RunId), "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	host := &legacyPrelaunchTestHost{}
	if err := agent.closeLegacyPrelaunch(t.Context(), path, authorization, digest, host); err != nil {
		t.Fatal(err)
	}
	state, err := agent.loadState(authorization.RunId)
	if err != nil || state.State != "failed" || state.ErrorCode != "legacy_prelaunch_infrastructure_failure" || state.ExitCode != -1 || state.ExecutionStarted || !state.LaunchAttempted || state.RecoveryActive || !state.CleanupVerified || !state.StopCleanupVerified {
		t.Fatal("closure fabricated execution or released unjoined custody", state, err)
	}
	if fixture.cleaner.count() != 1 || fixture.runner.launchCount() != 1 || host.verifies != 2 || !bytes.Equal(clock, recoveryClockBytes(t, fixture)) {
		t.Fatal("closure replayed work, omitted proof or changed the original clock")
	}
	if !reflect.DeepEqual(fixture.containment.snapshot(), []string{"prepare", "start", "freeze", "quiesce", "verify", "release"}) {
		t.Fatal("closure bypassed or reordered original full cleanup", fixture.containment.snapshot())
	}
	retainedManifest, err := os.ReadFile(filepath.Join(agent.stateDirectory(authorization.RunId), "manifest.json"))
	if err != nil || !bytes.Equal(manifestBytes, retainedManifest) {
		t.Fatal("closure relabeled historical execution identity", err)
	}
	if fixture.agent.Execute(t.Context(), authorization.RunId) == nil || fixture.runner.launchCount() != 1 {
		t.Fatal("closed attempted execution was relaunched")
	}
	closed, err := os.ReadFile(filepath.Join(agent.stateDirectory(authorization.RunId), "legacy-prelaunch-closed.json"))
	if err != nil || !bytes.Contains(closed, []byte("competition-legacy-prelaunch-closed/v1")) {
		t.Fatal("closure omitted its distinct durable operator receipt", err)
	}
}

func TestLegacyPrelaunchRefusesUnclosedLaunchSurfaceBeforeCleanup(t *testing.T) {
	for _, phase := range []string{"placement", "withdrawal", "proof"} {
		fixture, agent, authorization, path, digest := newLegacyPrelaunchTestFixture(t)
		host := &legacyPrelaunchTestHost{}
		synthetic := errors.New("synthetic unknown launch surface")
		switch phase {
		case "placement":
			host.placementErr = synthetic
		case "withdrawal":
			host.closeErr = synthetic
		case "proof":
			host.verify = func(int) ([]byte, error) { return nil, synthetic }
		}
		if agent.closeLegacyPrelaunch(t.Context(), path, authorization, digest, host) == nil {
			t.Fatal("unclosed native surface granted cleanup", phase)
		}
		state, err := agent.loadState(authorization.RunId)
		if err != nil || !state.RecoveryActive || state.State != "accepted" || state.CleanupVerified || state.StopCleanupVerified || fixture.cleaner.count() != 0 || fixture.runner.launchCount() != 1 {
			t.Fatal("unknown proof released or replayed original custody", phase, err)
		}
	}
}

func TestLegacyPrelaunchRetainsRecoveryAfterFailedCleanupAndResumes(t *testing.T) {
	fixture, agent, authorization, path, digest := newLegacyPrelaunchTestFixture(t)
	fixture.cleaner.err = errors.New("synthetic private daemon unavailable")
	if agent.closeLegacyPrelaunch(t.Context(), path, authorization, digest, &legacyPrelaunchTestHost{}) == nil {
		t.Fatal("failed concrete cleanup became a positive closure")
	}
	state, err := agent.loadState(authorization.RunId)
	if err != nil || !state.RecoveryActive || state.CleanupVerified || state.StopCleanupVerified {
		t.Fatal("failed cleanup released unknown custody", err)
	}
	fixture.cleaner.err = nil
	if err := agent.closeLegacyPrelaunch(t.Context(), path, authorization, digest, &legacyPrelaunchTestHost{}); err != nil {
		t.Fatal("same original cleanup could not resume", err)
	}
	if fixture.runner.launchCount() != 1 {
		t.Fatal("cleanup retry relaunched the attempted owner/evaluator")
	}
}

func TestLegacyPrelaunchPostCleanupProofFailureRetainsCustodyAndResumes(t *testing.T) {
	fixture, agent, authorization, path, digest := newLegacyPrelaunchTestFixture(t)
	host := &legacyPrelaunchTestHost{verify: func(count int) ([]byte, error) {
		if count == 2 {
			return nil, errors.New("synthetic new queued manager generation")
		}
		return []byte("synthetic closed surface"), nil
	}}
	if agent.closeLegacyPrelaunch(t.Context(), path, authorization, digest, host) == nil {
		t.Fatal("changed post-cleanup manager generation published terminal closure")
	}
	state, err := agent.loadState(authorization.RunId)
	if err != nil || !state.RecoveryActive || !state.CleanupVerified || !state.StopCleanupVerified || state.State != "accepted" {
		t.Fatal("post-cleanup uncertainty did not retain physical custody", err)
	}
	if err := agent.closeLegacyPrelaunch(t.Context(), path, authorization, digest, &legacyPrelaunchTestHost{}); err != nil {
		t.Fatal("retained positive cleanup could not finish after fresh proof", err)
	}
	if fixture.cleaner.count() != 1 || fixture.runner.launchCount() != 1 {
		t.Fatal("resumption repeated resource deletion or execution")
	}
}

func TestLegacyPrelaunchRejectsExecutedChangedClockOrForeignSlot(t *testing.T) {
	for _, fault := range []string{"executed", "clock", "slot", "state", "ordinary"} {
		fixture, agent, authorization, path, digest := newLegacyPrelaunchTestFixture(t)
		switch fault {
		case "executed":
			state, err := agent.loadState(authorization.RunId)
			if err != nil {
				t.Fatal(err)
			}
			state.ExecutionStarted = true
			if err := agent.storeState(authorization.RunId, state); err != nil {
				t.Fatal(err)
			}
		case "clock":
			authorization.ClockSha256 = strings.Repeat("f", 64)
		case "slot":
			if err := agent.storeSlot(strings.Repeat("e", 64)); err != nil {
				t.Fatal(err)
			}
		case "state":
			authorization.StateSha256 = strings.Repeat("f", 64)
		case "ordinary":
			agent.recoveryOnly = false
		}
		if agent.closeLegacyPrelaunch(t.Context(), path, authorization, digest, &legacyPrelaunchTestHost{}) == nil || fixture.cleaner.count() != 0 || fixture.runner.launchCount() != 1 {
			t.Fatal("changed or unowned original custody authorized closure", fault)
		}
	}
}

func TestLegacyPrelaunchAuthorizationRejectsDuplicateUnknownAndWritableAuthority(t *testing.T) {
	fixture, _, _, path, _ := newLegacyPrelaunchTestFixture(t)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, fault := range []string{"duplicate", "unknown", "writable", "link"} {
		candidate := filepath.Join(fixture.config.BaseDirectory, "authorization-"+fault+".json")
		content := append([]byte(nil), original...)
		if fault == "duplicate" {
			content = append([]byte(`{"schema":1,`), original[1:]...)
		} else if fault == "unknown" {
			content = append([]byte(`{"waive_cleanup":true,`), original[1:]...)
		}
		writeRecoveryTestAuthority(t, fixture, candidate, content)
		if fault == "writable" {
			if err := os.Chmod(candidate, 0600); err != nil {
				t.Fatal(err)
			}
		} else if fault == "link" {
			if err := os.Link(candidate, candidate+".alias"); err != nil {
				t.Fatal(err)
			}
		}
		if _, _, err := readLegacyPrelaunchAuthorization(candidate, fixture.settings, false); err == nil {
			t.Fatal("malformed or mutable operator authority was accepted", fault)
		}
	}
}

func TestLegacyPrelaunchResumeAuthenticatesRetainedOriginalStateBytes(t *testing.T) {
	fixture, agent, authorization, path, digest := newLegacyPrelaunchTestFixture(t)
	if agent.closeLegacyPrelaunch(t.Context(), path, authorization, digest, &legacyPrelaunchTestHost{closeErr: errors.New("synthetic stop before cleanup")}) == nil {
		t.Fatal("synthetic interrupted closure unexpectedly completed")
	}
	startedPath := filepath.Join(agent.stateDirectory(authorization.RunId), "legacy-prelaunch-started.json")
	content, err := os.ReadFile(startedPath)
	started := legacyPrelaunchStarted{}
	if err != nil || decodeStrict(content, &started) != nil {
		t.Fatal("starting receipt unavailable", err)
	}
	started.OriginalState.FencingToken++
	if err := agent.storeState(authorization.RunId, &started.OriginalState); err != nil {
		t.Fatal(err)
	}
	content, err = json.Marshal(started)
	if err != nil {
		t.Fatal(err)
	}
	writeRecoveryTestAuthority(t, fixture, startedPath, content)
	if agent.closeLegacyPrelaunch(t.Context(), path, authorization, digest, &legacyPrelaunchTestHost{}) == nil || fixture.cleaner.count() != 0 {
		t.Fatal("retained digest string authenticated altered original snapshot bytes")
	}
}

func TestLegacyPrelaunchRetainedClosedReceiptKeepsEveryImmutableBinding(t *testing.T) {
	for _, fault := range []string{"schema", "kind", "approval", "manifest", "plan", "clock", "boot", "before", "after", "resources", "run", "provenance", "started", "stderr"} {
		fixture, agent, authorization, path, digest := newLegacyPrelaunchTestFixture(t)
		if err := agent.closeLegacyPrelaunch(t.Context(), path, authorization, digest, &legacyPrelaunchTestHost{}); err != nil {
			t.Fatal(err)
		}
		startedBytes, err := os.ReadFile(filepath.Join(agent.stateDirectory(authorization.RunId), "legacy-prelaunch-started.json"))
		started := legacyPrelaunchStarted{}
		if err != nil || decodeStrict(startedBytes, &started) != nil {
			t.Fatal(err)
		}
		// Reproduce a crash after the closure receipt fsync but before the
		// final state write; no native result or marker is synthesized.
		state := started.OriginalState
		state.CleanupVerified, state.StopCleanupVerified = true, true
		if err := agent.storeState(authorization.RunId, &state); err != nil {
			t.Fatal(err)
		}
		closedPath := filepath.Join(agent.stateDirectory(authorization.RunId), "legacy-prelaunch-closed.json")
		content, err := os.ReadFile(closedPath)
		closed := legacyPrelaunchClosed{}
		if err != nil || decodeStrict(content, &closed) != nil {
			t.Fatal(err)
		}
		switch fault {
		case "schema":
			closed.Schema++
		case "kind":
			closed.Kind = "synthetic-foreign-proof"
		case "approval":
			closed.ApprovalSha256 = strings.Repeat("f", 64)
		case "manifest":
			closed.ManifestSha256 = strings.Repeat("f", 64)
		case "plan":
			closed.CleanupPlanSha256 = strings.Repeat("f", 64)
		case "clock":
			closed.ClockSha256 = strings.Repeat("f", 64)
		case "boot":
			closed.BootId = "44444444-4444-4444-8444-444444444444"
		case "before":
			closed.BeforeSha256 = "missing"
		case "after":
			closed.AfterSha256 = "missing"
		case "resources":
			closed.ResourcesSha256 = strings.Repeat("f", 64)
		case "run":
			closed.RunId = strings.Repeat("f", 64)
		case "provenance":
			closed.Provenance = "synthetic-evaluator-output"
		case "started":
			closed.EvaluatorStarted = true
		case "stderr":
			closed.OriginalEvaluatorStderrExists = true
		}
		content, err = json.Marshal(closed)
		if err != nil {
			t.Fatal(err)
		}
		writeRecoveryTestAuthority(t, fixture, closedPath, content)
		if agent.closeLegacyPrelaunch(t.Context(), path, authorization, digest, &legacyPrelaunchTestHost{}) == nil {
			t.Fatal("corrupt immutable closure receipt permitted resumption", fault)
		}
		retained, err := agent.loadState(authorization.RunId)
		if err != nil || !retained.RecoveryActive || retained.State != "accepted" || fixture.runner.launchCount() != 1 {
			t.Fatal("corrupt receipt released physical custody", fault, err)
		}
	}
}

func TestLegacyPrelaunchCancellationAfterCleanupCannotPublishClosure(t *testing.T) {
	fixture, agent, authorization, path, digest := newLegacyPrelaunchTestFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	host := &legacyPrelaunchTestHost{verify: func(count int) ([]byte, error) {
		if count == 2 {
			cancel()
		}
		return []byte("synthetic joined positive proof"), nil
	}}
	if err := agent.closeLegacyPrelaunch(ctx, path, authorization, digest, host); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled publication completed or lost its cause", err)
	}
	state, err := agent.loadState(authorization.RunId)
	if err != nil || !state.RecoveryActive || !state.CleanupVerified || !state.StopCleanupVerified || state.State != "accepted" || fixture.cleaner.count() != 1 {
		t.Fatal("canceled owner released retained cleanup custody", err)
	}
	if _, err := os.Stat(filepath.Join(agent.stateDirectory(authorization.RunId), "legacy-prelaunch-closed.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("canceled owner published a closure receipt", err)
	}
}

func TestLegacyPrelaunchDiagnosticIsHonestTransportableFailureEvidence(t *testing.T) {
	fixture, agent, authorization, path, digest := newLegacyPrelaunchTestFixture(t)
	if err := agent.closeLegacyPrelaunch(t.Context(), path, authorization, digest, &legacyPrelaunchTestHost{}); err != nil {
		t.Fatal(err)
	}
	stderrPath := filepath.Join(agent.workDirectory(authorization.RunId), "worker.stderr.log")
	content, err := os.ReadFile(stderrPath)
	diagnostic := legacyPrelaunchDiagnostic{}
	if err != nil || decodeStrict(content, &diagnostic) != nil || diagnostic.Schema != Schema || diagnostic.Provenance != "operator_legacy_prelaunch_recovery" || diagnostic.EvaluatorStarted || diagnostic.OriginalEvaluatorStderrExists || len(diagnostic.Closure) == 0 {
		t.Fatal("compatibility filename misrepresented diagnostic provenance", err)
	}
	closed, err := os.ReadFile(filepath.Join(agent.stateDirectory(authorization.RunId), "legacy-prelaunch-closed.json"))
	if err != nil || !bytes.Equal(diagnostic.Closure, closed) || bytes.Contains(content, []byte(strings.Repeat("d", 64))) {
		t.Fatal("retained diagnostic omitted exact closure proof or exposed hidden input", err)
	}
	response, err := fixture.call(t, "artifacts", 1, nil)
	if err != nil || response.State != "failed" || response.ErrorCode != "legacy_prelaunch_infrastructure_failure" {
		t.Fatal("original authenticated terminal transport refused closure evidence", err)
	}
	found := false
	for _, file := range response.Files {
		if file.Path == "worker.stderr.log" {
			found = file.Sha256 == digestDockerBytes(content) && file.Bytes == int64(len(content))
		}
	}
	if !found {
		t.Fatal("signed artifact inventory omitted the retained compatibility diagnostic")
	}
	for _, name := range []string{"worker-result.json", "score.json"} {
		if _, err := os.Stat(filepath.Join(agent.workDirectory(authorization.RunId), name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("operator closure manufactured evaluator scoring output", name, err)
		}
	}
}

func TestLegacyPrelaunchDiagnosticNeverOverwritesOriginalStderr(t *testing.T) {
	fixture, agent, authorization, path, digest := newLegacyPrelaunchTestFixture(t)
	stderrPath := filepath.Join(agent.workDirectory(authorization.RunId), "worker.stderr.log")
	original := []byte("synthetic existing evaluator stderr\n")
	if err := os.WriteFile(stderrPath, original, 0400); err != nil {
		t.Fatal(err)
	}
	if agent.closeLegacyPrelaunch(t.Context(), path, authorization, digest, &legacyPrelaunchTestHost{}) == nil || fixture.cleaner.count() != 0 {
		t.Fatal("existing stderr was treated as absent and replaced")
	}
	retained, err := os.ReadFile(stderrPath)
	if err != nil || !bytes.Equal(retained, original) {
		t.Fatal("operator closure overwrote original stderr", err)
	}
}

func TestLegacyPrelaunchDiagnosticCrashReplayRequiresIdenticalReceipt(t *testing.T) {
	for _, fault := range []string{"identical", "changed"} {
		fixture, agent, authorization, path, digest := newLegacyPrelaunchTestFixture(t)
		if err := agent.closeLegacyPrelaunch(t.Context(), path, authorization, digest, &legacyPrelaunchTestHost{}); err != nil {
			t.Fatal(err)
		}
		startedBytes, err := os.ReadFile(filepath.Join(agent.stateDirectory(authorization.RunId), "legacy-prelaunch-started.json"))
		started := legacyPrelaunchStarted{}
		if err != nil || decodeStrict(startedBytes, &started) != nil {
			t.Fatal(err)
		}
		state := started.OriginalState
		state.CleanupVerified, state.StopCleanupVerified = true, true
		if err := agent.storeState(authorization.RunId, &state); err != nil {
			t.Fatal(err)
		}
		stderrPath := filepath.Join(agent.workDirectory(authorization.RunId), "worker.stderr.log")
		before, err := os.ReadFile(stderrPath)
		if err != nil {
			t.Fatal(err)
		}
		if fault == "changed" {
			before = []byte("synthetic different permit or receipt\n")
			writeRecoveryTestAuthority(t, fixture, stderrPath, before)
		}
		host := &legacyPrelaunchTestHost{verify: func(int) ([]byte, error) { return []byte("fresh temporal proof bytes"), nil }}
		err = agent.closeLegacyPrelaunch(t.Context(), path, authorization, digest, host)
		if (err == nil) != (fault == "identical") {
			t.Fatal("crash replay did not require byte-identical original diagnostic", fault, err)
		}
		after, readErr := os.ReadFile(stderrPath)
		if readErr != nil || !bytes.Equal(before, after) || fixture.cleaner.count() != 1 || fixture.runner.launchCount() != 1 {
			t.Fatal("crash replay overwrote diagnostic or repeated execution/cleanup", fault, readErr)
		}
		if fault == "changed" {
			retained, err := agent.loadState(authorization.RunId)
			if err != nil || !retained.RecoveryActive || retained.State != "accepted" {
				t.Fatal("changed diagnostic released recovery custody", err)
			}
		}
	}
}
