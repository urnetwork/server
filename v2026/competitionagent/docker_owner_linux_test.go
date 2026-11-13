// Composed agent ownership tests replace only native boundaries. They exercise
// real signed admission, immutable plans, slot custody and root-only receipts.
package competitionagent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// Hooks execute outside its state lock, allowing barriers and durable-agent
// reads without a test-only reentrant lock or a scheduler-dependent assertion.
type agentTestContainment struct {
	stateLock sync.Mutex
	events    []string
	before    func(context.Context, string, cleanupPlan, bool) error
}

func (self *agentTestContainment) event(ctx context.Context, operation string, plan cleanupPlan, reaped bool) error {
	self.stateLock.Lock()
	self.events = append(self.events, operation)
	before := self.before
	self.stateLock.Unlock()
	if before != nil {
		return before(ctx, operation, plan, reaped)
	}
	return nil
}

func (self *agentTestContainment) snapshot() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]string(nil), self.events...)
}

func (self *agentTestContainment) Prepare(ctx context.Context, plan cleanupPlan) error {
	if err := self.event(ctx, "prepare", plan, false); err != nil {
		return err
	}
	return os.Mkdir(filepath.Dir(plan.DockerOwner.SocketPath), 0710)
}

func (self *agentTestContainment) Start(ctx context.Context, plan cleanupPlan) error {
	return self.event(ctx, "start", plan, false)
}

func (self *agentTestContainment) Quiesce(ctx context.Context, plan cleanupPlan, reaped bool) error {
	return self.event(ctx, "quiesce", plan, reaped)
}

func (self *agentTestContainment) VerifyRetained(ctx context.Context, plan cleanupPlan) error {
	return self.event(ctx, "verify", plan, false)
}

func (self *agentTestContainment) Release(ctx context.Context, plan cleanupPlan) error {
	return self.event(ctx, "release", plan, false)
}

func (self *agentTestContainment) EmergencyFreeze(ctx context.Context, plan cleanupPlan) error {
	return self.event(ctx, "freeze", plan, false)
}

// Both root plans precede one owner ready boundary and one evaluator launch.
// Reconnects advance fencing but never repeat prepare/start or reset its clock.
func TestAgentPreparesContainmentAndDockerOwnerBeforeEvaluator(t *testing.T) {
	fixture := newAgentTestFixture(t)
	fixture.runner.beforeStart = func(launch Launch) {
		if !reflect.DeepEqual(fixture.containment.snapshot(), []string{"prepare", "start"}) || launch.OwnerUnitName != dockerOwnerUnitName(launch.RunId) || launch.ManagementCpuSet != "6,8" {
			t.Error("evaluator preceded independent prepared/ready ownership")
		}
		state, err := fixture.agent.loadState(launch.RunId)
		if err != nil {
			t.Fatal(err)
		}
		plan, err := fixture.agent.loadCleanupPlan(launch.RunId, state, &fixture.manifest)
		if err != nil || plan.DockerOwner == nil || plan.DockerOwner.RunId != launch.RunId || plan.DockerOwner.JobId != fixture.manifest.Identity.JobId ||
			plan.DockerOwner.UpstreamSocketPath != fixture.config.DockerSocketPath || plan.DockerOwner.ResourcePolicySha256 != fixture.config.DockerRole.ResourcePolicy.Sha256 ||
			!plan.DockerOwner.DeadlineAt.Equal(fixture.manifest.Identity.DeadlineAt) {
			t.Fatal("Docker owner was not bound to the committed source/resource/original-clock tuple", err)
		}
		if plan.SliceResources == nil || plan.SliceResources.ResourcePolicySha256 != fixture.config.DockerRole.ResourcePolicy.Sha256 ||
			plan.SliceResources.BuildTasksMaximum != fixture.settings.BuildTasksMaximum || plan.SliceResources.CpuPeriodMicros != fixture.settings.ContainmentCpuPeriodMicros {
			t.Fatal("aggregate resource ceilings were not root-committed before launch")
		}
	}
	for fence := int64(1); fence <= 2; fence++ {
		if _, err := fixture.call(t, "start", fence, &fixture.manifest); err != nil {
			t.Fatal(err)
		}
	}
	if fixture.runner.launchCount() != 1 || !reflect.DeepEqual(fixture.containment.snapshot(), []string{"prepare", "start"}) {
		t.Fatal("reconnect repeated containment/daemon/evaluator launch")
	}
}

// A failed prepare or unknown owner ready response never dispatches evaluator
// code, clears its launch marker, or releases its original physical custody.
func TestAgentUncertainDockerOwnerCannotLaunchOrReplayEvaluator(t *testing.T) {
	for _, failure := range []string{"prepare", "start"} {
		fixture := newAgentTestFixture(t)
		fixture.containment.before = func(_ context.Context, operation string, _ cleanupPlan, _ bool) error {
			if operation == failure {
				return errors.New("synthetic lost native acknowledgment")
			}
			return nil
		}
		for fence := int64(1); fence <= 2; fence++ {
			response, err := fixture.call(t, "start", fence, &fixture.manifest)
			if err != nil || response.State != "unknown" {
				t.Fatal("uncertain ownership was not reconciled as unknown", failure, response, err)
			}
		}
		state, err := fixture.agent.loadState(fixture.manifest.Identity.RunId)
		if err != nil || !state.LaunchAttempted || state.CleanupVerified || fixture.runner.launchCount() != 0 {
			t.Fatal("uncertain owner reset or dispatched evaluator authority", failure, err)
		}
		if err := fixture.agent.requireFreeSlot(t.Context(), strings.Repeat("f", 64)); err == nil {
			t.Fatal("uncertain owner released the physical executor")
		}
		wanted := []string{"prepare"}
		if failure == "start" {
			wanted = append(wanted, "start")
		}
		if !reflect.DeepEqual(fixture.containment.snapshot(), wanted) {
			t.Fatal("reconnect repeated a native ownership attempt")
		}
	}
}

// A zero-unknown daemon barrier must precede concrete census/reap, and the
// retained frozen proof and durable receipt must precede slice retirement.
func TestAgentCleanupOrdersQuiesceCensusProofAndRetirement(t *testing.T) {
	fixture := newAgentTestFixture(t)
	if _, err := fixture.call(t, "start", 1, &fixture.manifest); err != nil {
		t.Fatal(err)
	}
	fixture.cleaner.before = func(_ context.Context, _ cleanupPlan) error {
		if !reflect.DeepEqual(fixture.containment.snapshot(), []string{"prepare", "start", "quiesce"}) {
			t.Error("resource census preceded its independent daemon barrier")
		}
		return nil
	}
	fixture.containment.before = func(_ context.Context, operation string, plan cleanupPlan, _ bool) error {
		if operation == "release" {
			state, err := fixture.agent.loadState(plan.RunId)
			if err != nil {
				t.Fatal(err)
			}
			reaped, err := fixture.agent.loadResourcesReaped(plan.RunId, state.CleanupPlanSha256)
			if err != nil || !reaped || state.StopCleanupVerified || fixture.cleaner.count() != 1 {
				t.Error("slice retirement preceded its durable current-run completed reap proof")
			}
		}
		return nil
	}
	if err := fixture.agent.Cleanup(t.Context(), fixture.manifest.Identity.RunId); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fixture.containment.snapshot(), []string{"prepare", "start", "quiesce", "verify", "release"}) {
		t.Fatal("cleanup ownership stages were omitted or reordered")
	}
}

// Unknown daemon outcomes cannot be made successful by an empty Docker/cgroup
// census, an old self-check, terminal evaluator JSON or a slice stop request.
func TestAgentUnknownDockerMutationPreventsCensusAndSlotRelease(t *testing.T) {
	fixture := newAgentTestFixture(t)
	if _, err := fixture.call(t, "start", 1, &fixture.manifest); err != nil {
		t.Fatal(err)
	}
	fixture.containment.before = func(_ context.Context, operation string, _ cleanupPlan, _ bool) error {
		if operation == "quiesce" {
			return ErrDockerProxyUnknown
		}
		return nil
	}
	if err := fixture.agent.Cleanup(t.Context(), fixture.manifest.Identity.RunId); !errors.Is(err, ErrDockerProxyUnknown) || fixture.cleaner.count() != 0 {
		t.Fatal("unknown mutation was replaced with an empty resource census", err)
	}
	state, err := fixture.agent.loadState(fixture.manifest.Identity.RunId)
	if err != nil || state.CleanupVerified || state.StopCleanupVerified {
		t.Fatal("unknown mutation published cleanup success", err)
	}
	if err := fixture.agent.requireFreeSlot(t.Context(), strings.Repeat("f", 64)); err == nil {
		t.Fatal("unknown mutation released the physical executor")
	}
}

// A partially retired set may contain missing parents. Its exact durable reap
// proof permits only another retirement, not another census or evaluator run.
func TestAgentRetriesInterruptedRetirementWithoutLosingCompletedReap(t *testing.T) {
	fixture := newAgentTestFixture(t)
	if _, err := fixture.call(t, "start", 1, &fixture.manifest); err != nil {
		t.Fatal(err)
	}
	releases, barriers := 0, []bool{}
	fixture.containment.before = func(_ context.Context, operation string, _ cleanupPlan, reaped bool) error {
		if operation == "quiesce" {
			barriers = append(barriers, reaped)
		}
		if operation == "release" {
			releases++
			if releases == 1 {
				return errors.New("synthetic interrupted slice retirement")
			}
		}
		return nil
	}
	if err := fixture.agent.Cleanup(t.Context(), fixture.manifest.Identity.RunId); err == nil {
		t.Fatal("interrupted retirement was published as joined")
	}
	if err := fixture.agent.requireFreeSlot(t.Context(), strings.Repeat("f", 64)); err == nil {
		t.Fatal("interrupted retirement transferred physical custody")
	}
	if err := fixture.agent.Cleanup(t.Context(), fixture.manifest.Identity.RunId); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(barriers, []bool{false, true}) || fixture.cleaner.count() != 1 || releases != 2 || fixture.runner.launchCount() != 1 {
		t.Fatal("retirement retry lost its current-run proof or repeated execution")
	}
	if err := fixture.agent.Execute(t.Context(), fixture.manifest.Identity.RunId); err == nil {
		t.Fatal("late evaluator launch ignored completed cleanup custody")
	}
}

// Emergency post-stop does not wait on admission or ordinary cleanup locks.
// Those exact held locks make the progress proof independent of scheduling.
func TestAgentEmergencyFreezeDoesNotAcquirePublicationOrCleanupLocks(t *testing.T) {
	fixture := newAgentTestFixture(t)
	if _, err := fixture.call(t, "start", 1, &fixture.manifest); err != nil {
		t.Fatal(err)
	}
	publication, err := fixture.agent.lock(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer publication.Close()
	cleanup, err := fixture.agent.lockFile(t.Context(), filepath.Join(fixture.agent.stateDirectory(fixture.manifest.Identity.RunId), "cleanup.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup.Close()
	ctx, cancel := context.WithTimeout(t.Context(), fixture.settings.ControlTimeout)
	defer cancel()
	if err := fixture.agent.EmergencyFreeze(ctx, fixture.manifest.Identity.RunId); err != nil {
		t.Fatal("held non-kill locks delayed emergency containment", err)
	}
	if !reflect.DeepEqual(fixture.containment.snapshot(), []string{"prepare", "start", "freeze"}) {
		t.Fatal("emergency path skipped its direct independent freeze boundary")
	}
}

// Resource strings are not approximated: duplicate, overlapping, range-form,
// aliased or unproven CPU identities cannot become systemd/Docker authority.
func TestRunResourceIdentityRejectsUnprovenOrNoncanonicalSplit(t *testing.T) {
	valid := testRunResourceIdentityJson(t)
	if _, err := decodeRunResourceIdentity([]byte(valid)); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{strings.Replace(valid, "6,8", "4,8", 1), strings.Replace(valid, "2,4", "02,4", 1), strings.Replace(valid, "2,4", "2-4", 1),
		strings.Replace(valid, "6,8", "6,6", 1), strings.Replace(valid, `"disjoint_cpu_sets":true`, `"disjoint_cpu_sets":false`, 1),
		strings.Replace(valid, `"memory_capacity_passed":true`, `"memory_capacity_passed":false`, 1), strings.Replace(valid, `"schema":1`, `"Schema":1`, 1),
		strings.Replace(valid, `"schema":1`, `"schema":1,"schema":1`, 1)} {
		if _, err := decodeRunResourceIdentity([]byte(invalid)); err == nil {
			t.Error("unproven resource split was accepted")
		}
	}
}
