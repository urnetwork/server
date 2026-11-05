// Signed physical-custody tests use private agent files and a fake service
// manager only; they never contact a daemon, mount or native host cgroup.
package competitionagent

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
)

func TestAgentAvailabilityRequiresTerminalCleanupAndServiceJoin(t *testing.T) {
	fixture := newAgentTestFixture(t)
	assertState := func(want string) {
		t.Helper()
		response, err := fixture.call(t, "availability", 0, nil)
		if err != nil || response.State != want || response.RunId != "" || response.FencingToken != 0 || response.Identity != nil || len(response.Files) != 0 || len(response.SelfCheck) != 0 {
			t.Fatal("physical observation lost custody or exposed another run", response, err)
		}
	}
	assertState("available")
	if _, err := fixture.call(t, "start", 1, &fixture.manifest); err != nil {
		t.Fatal(err)
	}
	assertState("occupied")
	if err := fixture.agent.Execute(t.Context(), fixture.manifest.Identity.RunId); err != nil {
		t.Fatal(err)
	}
	assertState("occupied")
	if err := fixture.agent.Cleanup(t.Context(), fixture.manifest.Identity.RunId); err != nil {
		t.Fatal(err)
	}
	assertState("occupied")
	fixture.runner.stateLock.Lock()
	fixture.runner.units[unitName(fixture.manifest.Identity.RunId)] = UnitStatus{Exists: true}
	fixture.runner.stateLock.Unlock()
	assertState("available")
	if fixture.runner.launchCount() != 1 {
		t.Fatal("physical observation launched an evaluator")
	}
}

func TestAgentAvailabilityReconcilesHardExitButNeverGuessesUnknownCleanup(t *testing.T) {
	fixture := newAgentTestFixture(t)
	fixture.runner.unknownLaunch = true
	if _, err := fixture.call(t, "start", 1, &fixture.manifest); err != nil {
		t.Fatal(err)
	}
	fixture.now = fixture.manifest.Identity.DeadlineAt
	fixture.cleaner.err = errors.New("synthetic native cleanup remains unknown")
	response, err := fixture.call(t, "availability", 0, nil)
	if err != nil || response.State != "occupied" {
		t.Fatal("expired unknown cleanup released physical custody", response, err)
	}
	fixture.cleaner.err = nil
	response, err = fixture.call(t, "availability", 0, nil)
	if err != nil || response.State != "available" {
		t.Fatal("joined hard exit could not reconcile its existing physical custody", response, err)
	}
	state, err := fixture.agent.loadState(fixture.manifest.Identity.RunId)
	if err != nil || !state.StopCleanupVerified || state.ExecutionStarted || state.State != "expired" || fixture.runner.launchCount() != 1 {
		t.Fatal("physical reconciliation replayed or fabricated execution", state, err)
	}
	manifest, err := fixture.agent.loadManifest(fixture.manifest.Identity.RunId, state.ManifestSha256)
	if err != nil || !manifest.Identity.StartedAt.Equal(fixture.manifest.Identity.StartedAt) || !manifest.Identity.DeadlineAt.Equal(fixture.manifest.Identity.DeadlineAt) {
		t.Fatal("physical reconciliation reset the original signed clock", err)
	}
}

// Only the read-only service observation can be delayed in this fake. Cleanup
// must retain independent publication access while that observation is blocked.
type availabilityTestRunner struct {
	Runner
	beforeStatus func()
}

func (self *availabilityTestRunner) Status(ctx context.Context, name string) (UnitStatus, error) {
	self.beforeStatus()
	return self.Runner.Status(ctx, name)
}

func TestAgentAvailabilityCannotLockCleanupAndRechecksChangedSlot(t *testing.T) {
	fixture := newAgentTestFixture(t)
	if _, err := fixture.call(t, "start", 1, &fixture.manifest); err != nil {
		t.Fatal(err)
	}
	if err := fixture.agent.Execute(t.Context(), fixture.manifest.Identity.RunId); err != nil {
		t.Fatal(err)
	}
	fixture.joinStoppedRun(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	fixture.agent.runner = &availabilityTestRunner{Runner: fixture.runner, beforeStatus: func() {
		close(entered)
		<-release
	}}
	done, finished := make(chan Response, 1), make(chan struct{})
	go func() {
		defer close(finished)
		response, err := fixture.agent.availability(t.Context())
		if err != nil {
			response.State = "synthetic-error"
		}
		done <- response
	}()
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }); <-finished })
	<-entered
	if err := fixture.agent.Cleanup(t.Context(), fixture.manifest.Identity.RunId); err != nil {
		t.Fatal("blocked service observation retained cleanup's publication lock", err)
	}
	lock, err := fixture.agent.lock(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	err = fixture.agent.storeSlot(RunId("synthetic-new-physical-owner", 0, 1))
	lock.Close()
	if err != nil {
		t.Fatal(err)
	}
	releaseOnce.Do(func() { close(release) })
	if response := <-done; response.State != "occupied" {
		t.Fatal("old service join qualified a concurrently changed physical owner", response)
	}
}

func TestAgentAvailabilityRejectsCallerSelectedRunAuthority(t *testing.T) {
	fixture := newAgentTestFixture(t)
	request := Request{Schema: Schema, HostId: fixture.config.HostId, Operation: "availability", RunId: fixture.manifest.Identity.RunId, FencingToken: 1}
	if _, err := fixture.agent.availability(t.Context()); err != nil {
		t.Fatal(err)
	}
	// The ordinary signed framing helper cannot intentionally preserve run
	// fields on this no-argument role, so construct its bounded request directly.
	if err := SignRequest(&request, fixture.coordinatorKey); err != nil {
		t.Fatal(err)
	}
	input, output := &bytes.Buffer{}, &bytes.Buffer{}
	if err := WriteDocument(input, request, fixture.config.Limits.DocumentBytes); err != nil {
		t.Fatal(err)
	}
	if err := fixture.agent.Serve(t.Context(), input, output); err == nil || output.Len() != 0 || fixture.runner.launchCount() != 0 {
		t.Fatal("physical observation accepted caller-selected run authority", err)
	}
}
