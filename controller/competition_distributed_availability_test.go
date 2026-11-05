// Physical admission tests use synthetic signed-response values and an in-memory
// store; no coordinator transport process or database connection is started.
package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/competitionagent"
)

// The actual transport authenticates these values before this finite consumer.
// Requests must remain the no-argument role, never a hidden start or cancel.
type competitionAvailabilityTransport struct {
	response competitionagent.Response
	err      error
	calls    int
	request  competitionagent.Request
}

func (self *competitionAvailabilityTransport) Exchange(_ context.Context, request competitionagent.Request, input, output string) (competitionagent.Response, error) {
	self.calls++
	self.request = request
	if input != "" || output != "" {
		return competitionagent.Response{}, errors.New("synthetic physical observation unexpectedly transferred files")
	}
	return self.response, self.err
}

func TestCompetitionDistributedAvailabilityRequiresExactFiniteResponse(t *testing.T) {
	transport := &competitionAvailabilityTransport{}
	evaluator := &competitionAgentEvaluator{transport: transport, host: CompetitionExecutorHostPolicy{HostId: "synthetic-physical-executor"}}
	t.Cleanup(func() { competitionExecutorPhysicalAvailable.Reset(); competitionExecutorPhysicalObservedAt.Reset() })
	for _, state := range []string{"available", "occupied"} {
		transport.response = competitionagent.Response{State: state}
		available, err := evaluator.Available(t.Context())
		if err != nil || available != (state == "available") || transport.request.Operation != "availability" || transport.request.RunId != "" || transport.request.Start != nil || transport.request.FencingToken != 0 {
			t.Fatal("physical observation lost its finite authenticated boundary", err)
		}
		value := testutil.ToFloat64(competitionExecutorPhysicalAvailable.WithLabelValues(evaluator.host.HostId))
		if value != map[string]float64{"available": 1, "occupied": 0}[state] || testutil.ToFloat64(competitionExecutorPhysicalObservedAt.WithLabelValues(evaluator.host.HostId)) <= 0 {
			t.Fatal("physical custody observation was not reflected independently of the database slot", value)
		}
	}
	for _, response := range []competitionagent.Response{
		{State: "ready"}, {State: "unavailable"}, {State: "available", ErrorCode: "synthetic-error"}, {State: "available", ExitCode: -1},
		{State: "available", Identity: &competitionagent.Identity{}}, {State: "available", ManifestSha256: "synthetic-other-run"},
		{State: "available", Files: []competitionagent.File{{Path: "synthetic-file"}}}, {State: "available", SelfCheck: []byte(`{}`)},
	} {
		transport.response = response
		if available, err := evaluator.Available(t.Context()); available || err == nil {
			t.Fatal("unavailable or expanded physical observation qualified fresh admission", response)
		}
		if value := testutil.ToFloat64(competitionExecutorPhysicalAvailable.WithLabelValues(evaluator.host.HostId)); value != -1 {
			t.Fatal("unavailable physical observation became a healthy zero", value)
		}
	}
	transport.err = errors.New("synthetic transport outcome remains unknown")
	if available, err := evaluator.Available(t.Context()); available || !errors.Is(err, transport.err) {
		t.Fatal("physical transport uncertainty became idle admission", err)
	}
}

// The store independently interprets reconcile-only as no new job admission.
// Existing work keeps its already-started absolute clock while hosts reconnect.
type competitionAvailabilityStore struct {
	fakeStore
	selected *Settings
	calls    int
	existing *queuedJob
	fresh    *queuedJob
}

func (self *competitionAvailabilityStore) Claim(_ context.Context, settings *Settings, _, _ string) (*queuedJob, error) {
	self.calls++
	self.selected = settings
	if self.existing != nil {
		return self.existing, nil
	}
	if settings.executorReconcileOnly {
		return nil, nil
	}
	return self.fresh, nil
}

// This synthetic capability never invokes evaluation or a self-check process.
type competitionAvailabilityEvaluator struct {
	fakeEvaluator
	available bool
	err       error
}

func (self *competitionAvailabilityEvaluator) Available(context.Context) (bool, error) {
	return self.available, self.err
}

func TestCompetitionDistributedOccupiedHostOnlyReconcilesOriginalClock(t *testing.T) {
	settings := validSettings()
	settings.DistributedEvaluationPolicy = competitionDistributedSlotTestPolicy(t)
	settings.distributedRoundEpoch = 2
	host := settings.DistributedEvaluationPolicy.Hosts[0]
	settings.executorHostId, settings.executorSlotId = host.HostId, host.SlotId
	started := time.Date(2035, 1, 2, 3, 4, 5, 0, time.UTC)
	deadline := started.Add(3 * time.Hour)
	fresh := &queuedJob{ScoreJobResult: ScoreJobResult{JobId: server.NewId()}}
	existing := &queuedJob{ScoreJobResult: ScoreJobResult{JobId: server.NewId(), StartedAt: &started}, AttemptCount: 4, ExecutionDeadline: &deadline}
	store, evaluator := &competitionAvailabilityStore{fresh: fresh}, &competitionAvailabilityEvaluator{}
	worker := &Worker{settings: settings, evaluator: evaluator, store: store}
	job, err := worker.claimWithPreparedRoundBaseline(t.Context(), HostSelfCheck{})
	if err != nil || job != nil || store.calls != 1 || !store.selected.executorReconcileOnly || settings.executorReconcileOnly {
		t.Fatal("busy remote host started a fresh job clock or leaked mutable selector state", err)
	}
	store.existing = existing
	job, err = worker.claimWithPreparedRoundBaseline(t.Context(), HostSelfCheck{})
	if err != nil || job != existing || job.AttemptCount != 4 || !job.StartedAt.Equal(started) || !job.ExecutionDeadline.Equal(deadline) || evaluator.calls != 0 {
		t.Fatal("busy host could not reconcile its unchanged existing execution", err)
	}
	store.existing, evaluator.available = nil, true
	job, err = worker.claimWithPreparedRoundBaseline(t.Context(), HostSelfCheck{})
	if err != nil || job != fresh || store.selected.executorReconcileOnly {
		t.Fatal("new positive physical observation retained a stale busy selector", err)
	}
}

func TestCompetitionDistributedAvailabilityFailureCannotReachClaim(t *testing.T) {
	settings := validSettings()
	settings.DistributedEvaluationPolicy = competitionDistributedSlotTestPolicy(t)
	settings.distributedRoundEpoch = 2
	host := settings.DistributedEvaluationPolicy.Hosts[0]
	settings.executorHostId, settings.executorSlotId = host.HostId, host.SlotId
	store := &competitionAvailabilityStore{}
	evaluator := &competitionAvailabilityEvaluator{err: errors.New("synthetic observation unavailable")}
	worker := &Worker{settings: settings, evaluator: evaluator, store: store}
	if job, err := worker.claimWithPreparedRoundBaseline(t.Context(), HostSelfCheck{}); job != nil || !errors.Is(err, evaluator.err) || store.calls != 0 {
		t.Fatal("unknown physical observation reached fresh claim", err)
	}
	worker.evaluator = &fakeEvaluator{}
	if job, err := worker.claimWithPreparedRoundBaseline(t.Context(), HostSelfCheck{}); job != nil || err == nil || store.calls != 0 {
		t.Fatal("distributed evaluator omitted physical-custody capability", err)
	}
	settings.DistributedEvaluationPolicy = nil
	if _, err := worker.claimWithPreparedRoundBaseline(t.Context(), HostSelfCheck{}); err != nil || store.calls != 1 || store.selected != settings {
		t.Fatal("prospective admission changed singleton claim semantics", err)
	}
}
