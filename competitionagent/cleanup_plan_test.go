// Deterministic post-stop tests prove state/slot ordering without accessing a
// host daemon or privileged cgroup. Concrete resource checks have their own
// fake-backed tests; actual systemd/hard-kill/bomb qualification stays separate.
package competitionagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// Acknowledged cleanup barriers replace timing assumptions about kernel joins.
type agentTestCleaner struct {
	stateLock sync.Mutex
	plans     []cleanupPlan
	err       error
	before    func(context.Context, cleanupPlan) error
}

// Keeps resource cleanup injectable while the complete durable agent is real.
func (self *agentTestCleaner) Cleanup(ctx context.Context, plan cleanupPlan) error {
	self.stateLock.Lock()
	self.plans = append(self.plans, plan)
	before, err := self.before, self.err
	self.stateLock.Unlock()
	if before != nil {
		return before(ctx, plan)
	}
	return err
}

// Reads the fake only after its acknowledged callback or completed operation.
func (self *agentTestCleaner) count() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return len(self.plans)
}

// Models systemd's post-stop helper followed by the inactive unit transition.
func (self *agentTestFixture) joinStoppedRun(t *testing.T) {
	t.Helper()
	if err := self.agent.Cleanup(t.Context(), self.manifest.Identity.RunId); err != nil {
		t.Fatal(err)
	}
	self.runner.stateLock.Lock()
	self.runner.units[unitName(self.manifest.Identity.RunId)] = UnitStatus{Exists: true, Active: false}
	self.runner.stateLock.Unlock()
}

// No callback discovers its build cgroup from a candidate-writable file.
func TestAgentCleanupPlanIsDurableBeforeDetachedLaunch(t *testing.T) {
	fixture := newAgentTestFixture(t)
	fixture.runner.beforeStart = func(launch Launch) {
		state, err := fixture.agent.loadState(launch.RunId)
		if err != nil || !state.LaunchAttempted || state.StopCleanupVerified {
			t.Fatal("launch preceded immutable cleanup authority", err)
		}
		plan, err := fixture.agent.loadCleanupPlan(launch.RunId, state, &fixture.manifest)
		if err != nil || len(plan.CgroupNames) != 31 {
			t.Fatal("launch did not retain the complete finite full-R9 cleanup plan", err)
		}
		policyBytes := []byte("{\"allowed_paths\":[\"server/example.go\"],\"forbidden_paths\":[\"server/connect/sim-latency/**\"],\"max_patch_bytes\":1024}\n")
		policy := sha256.Sum256(policyBytes)
		image := sha256.Sum256([]byte(fixture.manifest.EvaluatorImageDigest + "\x00" + fixture.manifest.Inputs[0].Sha256 + "\x00" + hex.EncodeToString(policy[:]) + "\x00" + fixture.config.SubmissionBuilderSha256))
		if plan.CgroupNames[0] != "urnetwork-submission-build-"+hex.EncodeToString(image[:])[:32]+".slice" || plan.JobId != fixture.manifest.Identity.JobId || plan.RoundId != fixture.manifest.Identity.RoundId {
			t.Fatal("cleanup build identity differs from the authenticated evaluator image/patch/policy/builder tuple")
		}
	}
	if _, err := fixture.call(t, "start", 1, &fixture.manifest); err != nil {
		t.Fatal(err)
	}
	if fixture.runner.launchCount() != 1 {
		t.Fatal("cleanup preparation changed one-shot dispatch")
	}
}

// A trusted normal evaluator result is not proof that the daemon's separate
// resource trees have joined. Both post-stop cleanup and inactive unit matter.
func TestAgentTerminalArtifactsWaitForCleanupAndServiceJoin(t *testing.T) {
	fixture := newAgentTestFixture(t)
	if _, err := fixture.call(t, "start", 1, &fixture.manifest); err != nil {
		t.Fatal(err)
	}
	if err := fixture.agent.Execute(t.Context(), fixture.manifest.Identity.RunId); err != nil {
		t.Fatal(err)
	}
	if response, err := fixture.call(t, "status", 2, nil); err != nil || response.State != "unknown" || response.ErrorCode != "stop_cleanup_pending" {
		t.Fatal("unjoined normal exit was published as terminal", response, err)
	}
	if _, err := fixture.call(t, "artifacts", 2, nil); err == nil {
		t.Fatal("terminal artifacts were published before post-stop cleanup")
	}
	other := fixture.anotherManifest(t, "synthetic-other-job", 1)
	if _, err := fixture.call(t, "start", 3, &other); err == nil {
		t.Fatal("new work overlapped absent post-stop proof")
	}
	if err := fixture.agent.Cleanup(t.Context(), fixture.manifest.Identity.RunId); err != nil {
		t.Fatal(err)
	}
	if response, err := fixture.call(t, "status", 3, nil); err != nil || terminalState(response.State) {
		t.Fatal("cleanup command return substituted for joined systemd unit", response, err)
	}
	if _, err := fixture.call(t, "start", 3, &other); err == nil {
		t.Fatal("new work overlapped an active post-stop unit")
	}
	fixture.joinStoppedRun(t)
	if response, err := fixture.call(t, "status", 4, nil); err != nil || response.State != "succeeded" {
		t.Fatal("joined normal exit lost its original terminal result", response, err)
	}
	if _, err := fixture.call(t, "start", 5, &other); err != nil || fixture.runner.launchCount() != 2 {
		t.Fatal("verified joined cleanup did not release physical custody", err)
	}
}

// A service killed before committing any result must join concrete cleanup
// before failure evidence or new physical custody can be acknowledged.
func TestAgentHardExitCleanupJoinsBeforeTerminalCustody(t *testing.T) {
	fixture := newAgentTestFixture(t)
	if _, err := fixture.call(t, "start", 1, &fixture.manifest); err != nil {
		t.Fatal(err)
	}
	state, err := fixture.agent.loadState(fixture.manifest.Identity.RunId)
	if err != nil {
		t.Fatal(err)
	}
	state.ExecutionStarted, state.State = true, "running"
	if err := fixture.agent.storeState(fixture.manifest.Identity.RunId, state); err != nil {
		t.Fatal(err)
	}
	fixture.runner.units[unitName(fixture.manifest.Identity.RunId)] = UnitStatus{Exists: true, Failed: true, ExitCode: 137}
	entered, release := make(chan struct{}), make(chan struct{})
	fixture.cleaner.before = func(ctx context.Context, plan cleanupPlan) error {
		close(entered)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	type result struct {
		response Response
		err      error
	}
	done := make(chan result, 1)
	ctx, cancel := context.WithTimeout(t.Context(), fixture.settings.ControlTimeout)
	defer cancel()
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		response, err := fixture.agent.status(ctx, Request{Schema: Schema, HostId: fixture.config.HostId, RunId: fixture.manifest.Identity.RunId, FencingToken: 2})
		done <- result{response: response, err: err}
	}()
	var releaseOnce sync.Once
	defer func() {
		releaseOnce.Do(func() { close(release) })
		select {
		case <-joined:
		case <-ctx.Done():
			t.Error("hard-exit cleanup did not join its owned test lifetime")
		}
	}()
	select {
	case <-entered:
	case completed := <-done:
		t.Fatal("hard-exit status bypassed its concrete cleanup barrier", completed)
	case <-ctx.Done():
		t.Fatal("hard-exit status never reached its owned cleanup barrier")
	}
	state, err = fixture.agent.loadState(fixture.manifest.Identity.RunId)
	if err != nil || state.CleanupVerified || state.StopCleanupVerified {
		t.Fatal("kill request was mistaken for completed resource cleanup", state, err)
	}
	if err := fixture.agent.requireFreeSlot(t.Context(), RunId("synthetic-next", 0, 1)); err == nil {
		t.Fatal("physical custody transferred before concrete cleanup joined")
	}
	select {
	case completed := <-done:
		t.Fatal("hard-exit status returned before cleanup joined", completed)
	default:
	}
	releaseOnce.Do(func() { close(release) })
	var completed result
	select {
	case completed = <-done:
	case <-ctx.Done():
		t.Fatal("hard-exit cleanup did not return after its acknowledged join")
	}
	if completed.err != nil || completed.response.State != "failed" || completed.response.ErrorCode != "executor_exit_uncommitted" || completed.response.ExitCode != 137 || fixture.runner.launchCount() != 1 {
		t.Fatal("hard-exit reconciliation replayed or fabricated success", completed)
	}
	state, err = fixture.agent.loadState(fixture.manifest.Identity.RunId)
	if err != nil || !state.CleanupVerified || !state.StopCleanupVerified {
		t.Fatal("joined hard-exit cleanup proof was not persisted", state, err)
	}
	if err := fixture.agent.Execute(t.Context(), fixture.manifest.Identity.RunId); err == nil {
		t.Fatal("hard-exit reconciliation authorized a second evaluator execution")
	}
}

// Self-check's retained qualification marker says nothing about live leftovers.
func TestAgentFailedHardExitCleanupCannotUseOldQualificationMarker(t *testing.T) {
	fixture := newAgentTestFixture(t)
	if _, err := fixture.call(t, "start", 1, &fixture.manifest); err != nil {
		t.Fatal(err)
	}
	fixture.runner.units[unitName(fixture.manifest.Identity.RunId)] = UnitStatus{Exists: true, Failed: true, ExitCode: 137}
	fixture.cleaner.err = errors.New("synthetic resource tree has not joined")
	response, err := fixture.call(t, "status", 2, nil)
	if err != nil || response.State != "unknown" || response.ErrorCode != "cleanup_not_verified" {
		t.Fatal("old self-check marker released unverified hard-exit custody", response, err)
	}
	other := fixture.anotherManifest(t, "synthetic-other-job", 1)
	if _, err := fixture.call(t, "start", 3, &other); err == nil {
		t.Fatal("failed cleanup released the host for another job")
	}
	fixture.cleaner.err = nil
	response, err = fixture.call(t, "status", 3, nil)
	if err != nil || response.State != "failed" || fixture.runner.launchCount() != 1 || response.Identity == nil || !response.Identity.DeadlineAt.Equal(fixture.manifest.Identity.DeadlineAt) {
		t.Fatal("cleanup recovery reset or replayed the original job", response, err)
	}
}

// Root state authenticates cleanup scope even if a workspace contains forged
// paths, and an old stopped run cannot target resources after slot transfer.
func TestAgentCleanupRejectsChangedScopeOrCustody(t *testing.T) {
	for _, name := range []string{"plan", "slot", "unlaunched"} {
		fixture := newAgentTestFixture(t)
		if _, err := fixture.call(t, "start", 1, &fixture.manifest); err != nil {
			t.Fatal(err)
		}
		runId := fixture.manifest.Identity.RunId
		switch name {
		case "plan":
			if err := os.WriteFile(filepath.Join(fixture.agent.stateDirectory(runId), "cleanup-plan.json"), []byte("{\"schema\":1,\"evidence_directory\":\"/synthetic/foreign\"}"), 0600); err != nil {
				t.Fatal(err)
			}
		case "slot":
			if err := fixture.agent.storeSlot(RunId("synthetic-foreign", 0, 1)); err != nil {
				t.Fatal(err)
			}
		case "unlaunched":
			state, err := fixture.agent.loadState(runId)
			if err != nil {
				t.Fatal(err)
			}
			state.LaunchAttempted = false
			if err := fixture.agent.storeState(runId, state); err != nil {
				t.Fatal(err)
			}
		}
		if err := fixture.agent.Cleanup(t.Context(), runId); err == nil || fixture.cleaner.count() != 0 {
			t.Fatal("unauthenticated cleanup scope reached privileged resource management", name, err)
		}
	}
}

// A misinstalled helper must fail before launch, not leave a guessed build
// identity for a post-stop routine to clean some other submission's cgroup.
func TestAgentCleanupIdentityMismatchBlocksLaunch(t *testing.T) {
	for _, kind := range []string{"builder", "formatter"} {
		fixture := newAgentTestFixture(t)
		if kind == "builder" {
			fixture.agent.config.SubmissionBuilderSha256 = hex.EncodeToString(make([]byte, 32))
		} else {
			path := filepath.Join(fixture.config.BaseDirectory, "wrong-formatter")
			if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '%s\\n' '{}'\n"), 0700); err != nil {
				t.Fatal(err)
			}
			fixture.agent.config.JsonFormatter = testAgentPin(t, path)
		}
		if _, err := fixture.call(t, "start", 1, &fixture.manifest); err == nil || fixture.runner.launchCount() != 0 {
			t.Fatal("mismatched cleanup release was launched", kind, err)
		}
	}
}

// A persisted plan hash is bound to the complete JSON bytes, not just names.
func TestAgentCleanupPlanDoesNotTrustWorkspaceSidecars(t *testing.T) {
	fixture := newAgentTestFixture(t)
	if _, err := fixture.call(t, "start", 1, &fixture.manifest); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.agent.workDirectory(fixture.manifest.Identity.RunId), "cleanup-plan.json"), []byte("{\"foreign\":true}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := fixture.agent.Cleanup(t.Context(), fixture.manifest.Identity.RunId); err != nil {
		t.Fatal(err)
	}
	fixture.cleaner.stateLock.Lock()
	plan := fixture.cleaner.plans[0]
	fixture.cleaner.stateLock.Unlock()
	content, err := json.Marshal(plan)
	if err != nil || len(content) == 0 || plan.JobId != fixture.manifest.Identity.JobId || plan.EvidenceDirectory != filepath.Join(fixture.agent.workDirectory(fixture.manifest.Identity.RunId), ".evidence-runtime") {
		t.Fatal("workspace sidecar supplied privileged cleanup authority", err)
	}
}
