// Composed worker regressions force admission failure, claim-only hold and
// detached native custody using explicit barriers. No database/native host runs.
package controller

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/competitionagent"
	"github.com/urnetwork/server/v2026/model"
)

// Every ordinary evaluator/preparation method is a deterministic tripwire.
type competitionExistingGuardEvaluator struct {
	forbidden atomic.Int32
	checked   chan struct{}
}

func (self *competitionExistingGuardEvaluator) SelfCheck(context.Context, *Settings) (HostSelfCheck, error) {
	if self.checked != nil {
		self.checked <- struct{}{}
	}
	return HostSelfCheck{}, errors.New("synthetic fresh admission lacks private headroom")
}

func (self *competitionExistingGuardEvaluator) Evaluate(context.Context, *Settings, *queuedJob) EvaluationOutcome {
	self.forbidden.Add(1)
	return EvaluationOutcome{Error: terminalInfrastructureError("synthetic_forbidden_start", "synthetic fresh evaluation tripwire")}
}

func (self *competitionExistingGuardEvaluator) PrepareRoundBaseline(context.Context, *Settings, *roundBaselinePreparation) (*preparedRoundBaseline, error) {
	self.forbidden.Add(1)
	return nil, errors.New("synthetic preparation tripwire")
}

// Claim/heartbeat/handback preserve the same exact authority checks as the
// production store boundary; separate PostgreSQL regressions exercise its SQL.
type competitionExistingWorkerStore struct {
	Store
	stateLock                                         sync.Mutex
	job                                               *queuedJob
	held                                              bool
	claims, hostRegistrations, completions, handbacks int
	heartbeat                                         chan struct{}
	claimed                                           chan *queuedJob
	unexpected                                        chan struct{}
	budgetCompletion                                  chan EvaluationOutcome
}

func (self *competitionExistingWorkerStore) CurrentRound(context.Context, *Settings) (*roundRecord, error) {
	if self.held {
		return nil, ErrAutomationHeld
	}
	return nil, nil
}

func (self *competitionExistingWorkerStore) CurrentStagingRound(context.Context, *Settings) (*roundRecord, error) {
	return nil, nil
}

func (self *competitionExistingWorkerStore) existingCompetitionJob(context.Context, *Settings) (*queuedJob, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	copy := *self.job
	return &copy, nil
}

func (self *competitionExistingWorkerStore) Claim(_ context.Context, settings *Settings, workerId, _ string) (*queuedJob, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	witness := settings.executorExistingWitness
	if witness == nil {
		self.unexpected <- struct{}{}
		return nil, ErrAutomationHeld
	}
	if !witness.consumed.CompareAndSwap(false, true) || !witness.matches(settings, self.job, server.NowUtc()) {
		return nil, ErrLeaseLost
	}
	self.claims++
	copy := *self.job
	expires := server.NowUtc().Add(time.Duration(settings.WorkerLeaseSeconds) * time.Second)
	copy.SlotFencingToken++
	copy.LeaseOwner, copy.LeaseExpiresAt, copy.executorSettings = workerId, &expires, settings
	self.job = &copy
	self.claimed <- &copy
	return &copy, nil
}

func (self *competitionExistingWorkerStore) RegisterHost(context.Context, *Settings, HostSelfCheck) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.hostRegistrations++
	return errors.New("synthetic host eligibility tripwire")
}

func (self *competitionExistingWorkerStore) Heartbeat(ctx context.Context, _ *Settings, _ string, jobId server.Id) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	authority, ok := ctx.Value(competitionJobAuthorityKey{}).(competitionJobAuthority)
	if !ok || authority.JobId != jobId || authority.SlotFencingToken != self.job.SlotFencingToken {
		return ErrLeaseLost
	}
	self.heartbeat <- struct{}{}
	return nil
}

func (self *competitionExistingWorkerStore) Complete(_ context.Context, _ *Settings, _ string, _ server.Id, outcome EvaluationOutcome) (bool, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.completions++
	if self.budgetCompletion != nil {
		self.budgetCompletion <- outcome
		return false, nil
	}
	return false, errors.New("synthetic premature completion tripwire")
}

func (self *competitionExistingWorkerStore) HandBack(ctx context.Context, _ string, jobId server.Id, _ string) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	authority, ok := ctx.Value(competitionJobAuthorityKey{}).(competitionJobAuthority)
	if !ok || authority.JobId != jobId || authority.SlotFencingToken != self.job.SlotFencingToken {
		return ErrLeaseLost
	}
	expires := server.NowUtc().Add(-time.Second)
	self.job.LeaseExpiresAt = &expires
	self.handbacks++
	return nil
}

// This closed synthetic transport authenticates actual Ed25519 frames. Its
// ordinary route is a tripwire, and its existing route has no input-body field.
type competitionExistingTestTransport struct {
	t          testing.TB
	manifest   competitionagent.StartManifest
	connection competitionExecutorConnection
	ordinary   atomic.Int32
	observed   chan struct{}
	attached   chan struct{}
	gate       sync.Once
	firstFence int64
	unknown    bool
	cancels    atomic.Int32
	mutate     func(*competitionagent.Response)
	keySeed    byte
}

func (self *competitionExistingTestTransport) Exchange(context.Context, competitionagent.Request, string, string) (competitionagent.Response, error) {
	self.ordinary.Add(1)
	return competitionagent.Response{}, errors.New("synthetic ordinary start/upload transport tripwire")
}

func (self *competitionExistingTestTransport) ExchangeExisting(ctx context.Context, request competitionagent.Request, outputDirectory string) (competitionagent.Response, error) {
	if request.Start != nil || request.Operation != "status" && request.Operation != "cancel" || outputDirectory != "" {
		self.t.Error("attach attempted start, upload or premature artifact transfer")
	}
	identity := self.manifest.Identity
	digest, _ := competitionagent.ManifestDigest(self.manifest)
	response := competitionagent.Response{Schema: competitionagent.Schema, HostId: identity.HostId, RunId: identity.RunId, FencingToken: request.FencingToken,
		Identity: &identity, ManifestSha256: digest, RequestSha256: strings.Repeat("a", 64), State: "running",
		ExistingRun: &competitionagent.ExistingRunEvidence{ApprovalSha256: self.connection.ExistingControl.ApprovalSha256, OriginalConfigSha256: self.connection.AgentConfigSha256,
			ResourceBoundarySha256: self.connection.ExistingControl.ResourceBoundarySha256, CleanupPlanSha256: strings.Repeat("b", 64), NativeProofSha256: strings.Repeat("c", 64), ExecutionStarted: true}}
	if request.Operation == "cancel" {
		self.cancels.Add(1)
		response.State, response.ExistingRun.CleanupVerified, response.ExistingRun.StopCleanupVerified = "expired", true, true
	} else if request.FencingToken > self.firstFence {
		self.gate.Do(func() { close(self.attached) })
		if self.unknown {
			response.State, response.ErrorCode, response.Identity, response.ExistingRun = "unknown", "run_not_found", nil, nil
		} else {
			<-ctx.Done()
			return competitionagent.Response{}, ctx.Err()
		}
	} else if self.observed != nil {
		self.observed <- struct{}{}
	}
	if self.mutate != nil {
		self.mutate(&response)
	}
	keySeed := self.keySeed
	if keySeed == 0 {
		keySeed = 2
	}
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{keySeed}, ed25519.SeedSize))
	if err := competitionagent.SignResponse(&response, key); err != nil {
		self.t.Fatal(err)
	}
	if err := competitionagent.VerifyResponse(response, key.Public().(ed25519.PublicKey)); err != nil {
		self.t.Fatal(err)
	}
	return response, nil
}

// All clocks, source data, keys and native digests are visibly synthetic.
func newCompetitionExistingWorkerFixture(t *testing.T, held bool) (*Worker, *competitionAgentEvaluator, *competitionExistingWorkerStore, *competitionExistingGuardEvaluator, *competitionExistingTestTransport) {
	t.Helper()
	return newCompetitionExistingWorkerFixtureWithSettings(t, held, automationApiSettings())
}

// Explicit policy variants use the same frozen baseline and signed observation.
func newCompetitionExistingWorkerFixtureWithSettings(t *testing.T, held bool, settings *Settings) (*Worker, *competitionAgentEvaluator, *competitionExistingWorkerStore, *competitionExistingGuardEvaluator, *competitionExistingTestTransport) {
	t.Helper()
	settings.FutureEpochPolicy = g2FuturePolicyForTest(t)
	settings.FutureEpochPolicy.DistributedEvaluationPolicy = competitionDistributedSlotTestPolicy(t)
	var err error
	settings, err = settings.ForCompetitionEpoch(2)
	if err != nil {
		t.Fatal(err)
	}
	settings.ArtifactRoot = t.TempDir()
	host := settings.DistributedEvaluationPolicy.Hosts[1]
	settings.executorHostId, settings.executorSlotId, settings.distributedRoundEpoch = host.HostId, host.SlotId, 2
	settings.automationLease = &model.CompetitionSupervisorLease{OwnerId: "synthetic-existing-supervisor", FencingToken: 3}
	connection := competitionExecutorConnection{HostId: host.HostId, AgentConfigSha256: strings.Repeat("d", 64), ExistingControl: &competitionExistingControlConnection{ApprovalSha256: strings.Repeat("e", 64), ResourceBoundarySha256: strings.Repeat("f", 64)}}
	settings.distributedCoordinator = &competitionDistributedCoordinatorProfile{Config: competitionDistributedCoordinatorConfig{Settings: defaultCompetitionDistributedCoordinatorSettings(), Hosts: []competitionExecutorConnection{connection}}}
	round, _ := sealedTestRound(t, settings)
	round.Epoch = 2
	settings.automationRoundId = round.RoundId
	providers := []byte("synthetic-existing-workload\n")
	digest := sha256.Sum256(providers)
	round.ProvidersSha256 = hex.EncodeToString(digest[:])
	round.ProvidersPath = filepath.Join(settings.ArtifactRoot, "rounds", round.RoundId.String(), "providers.yml")
	if err := os.MkdirAll(filepath.Dir(round.ProvidersPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeExclusiveFile(round.ProvidersPath, providers, 0400); err != nil {
		t.Fatal(err)
	}
	started := server.NowUtc().Add(-2 * time.Minute).Truncate(time.Microsecond)
	deadline, lease := started.Add(time.Duration(settings.EvaluationPolicy.ScoreTimeoutSeconds)*time.Second), server.NowUtc().Add(-time.Minute)
	rawScores := make([]float64, settings.EvaluationPolicy.Replicates)
	for index := range rawScores {
		rawScores[index] = 100
	}
	baseline := testRoundBaseline(t, settings, round, rawScores)
	baselineHash := sha256.Sum256(baseline)
	job := &queuedJob{ScoreJobResult: ScoreJobResult{JobId: server.NewId(), RoundId: round.RoundId, StartedAt: &started, State: "running", EvaluatorImageDigest: settings.EvaluatorImageDigest,
		ApiImageDigest: testApiImageDigest(), WorkerImageDigest: testWorkerImageDigest(), PatchSha256: strings.Repeat("1", 64)}, Round: *round, AttemptCount: 1, LeaseOwner: "synthetic-old-owner", LeaseExpiresAt: &lease,
		ExecutorHostId: host.HostId, SlotId: host.SlotId, SlotFencingToken: 7, ExecutionDeadline: &deadline,
		RoundBaseline: &roundBaselineRecord{RoundId: round.RoundId, Json: baseline, Sha256: hex.EncodeToString(baselineHash[:]), BaseSha: settings.BaseSha, ProvidersSha256: round.ProvidersSha256,
			EvaluatorImageDigest: settings.EvaluatorImageDigest, ScorerVersion: settings.scorerVersion(), HardwareId: settings.EvaluationPolicy.HardwareId, HostQualificationSha256: settings.EvaluationPolicy.HostQualificationSha256,
			SourceJobId: server.NewId(), SourceAttempt: 1, SourceArtifactManifestSha256: strings.Repeat("2", 64), CreatedAt: started}}
	manifest, err := competitionExistingManifest(t.Context(), settings, job, host)
	if err != nil {
		t.Fatal(err)
	}
	transport := &competitionExistingTestTransport{t: t, manifest: manifest, connection: connection, firstFence: job.SlotFencingToken, observed: make(chan struct{}, 4), attached: make(chan struct{})}
	evaluator := &competitionAgentEvaluator{host: host, transport: transport, poll: make(chan time.Time)}
	guard := &competitionExistingGuardEvaluator{checked: make(chan struct{}, 4)}
	store := &competitionExistingWorkerStore{job: job, held: held, heartbeat: make(chan struct{}, 4), claimed: make(chan *queuedJob, 4), unexpected: make(chan struct{}, 4)}
	worker, err := newWorkerWithImageDigest(settings, store, guard, "synthetic-existing-worker", testWorkerImageDigest())
	if err != nil {
		t.Fatal(err)
	}
	return worker, evaluator, store, guard, transport
}

// Both historical and production-sized fixtures retain their frozen count;
// changing only the baseline count still fails the real identity validator.
func TestCompetitionExistingWorkerFixtureBindsFrozenReplicatePolicy(t *testing.T) {
	for _, replicateCount := range []int{3, 9} {
		settings := automationApiSettings()
		settings.EvaluationPolicy.Replicates = replicateCount
		worker, evaluator, store, guard, transport := newCompetitionExistingWorkerFixtureWithSettings(t, true, settings)
		policy, err := decodeRoundPolicySnapshot(store.job.Round.PolicyJson)
		if err != nil || policy.EvaluationPolicy.Replicates != replicateCount || worker.settings.EvaluationPolicy.Replicates != replicateCount {
			t.Fatalf("configured %d-replicate policy was not frozen: %v", replicateCount, err)
		}
		var baseline roundBaselineManifest
		if err := json.Unmarshal(store.job.RoundBaseline.Json, &baseline); err != nil {
			t.Fatal(err)
		}
		if len(baseline.Replicates) != replicateCount {
			t.Fatalf("frozen %d-replicate policy received %d baseline samples", replicateCount, len(baseline.Replicates))
		}
		for _, replicate := range baseline.Replicates {
			if replicate.RawScore != 100 {
				t.Fatal("synthetic baseline score changed")
			}
		}
		if digest, err := validateRoundBaselineJson(worker.settings, &store.job.Round, store.job.RoundBaseline.Json); err != nil || digest != store.job.RoundBaseline.Sha256 {
			t.Fatal("matching frozen baseline was refused", err)
		}
		var request evaluatorRequest
		if err := json.Unmarshal(transport.manifest.EvaluatorRequest, &request); err != nil || request.EvaluationPolicy.Replicates != replicateCount {
			t.Fatal("existing manifest changed the frozen replicate count", err)
		}
		if witness, err := evaluator.observeExisting(t.Context(), worker.settings, store.job); err != nil || witness == nil {
			t.Fatal("matching fixture could not authenticate its existing observation", err)
		}
		wrongCount := 9
		if replicateCount == 9 {
			wrongCount = 3
		}
		wrong := baseline
		wrong.Replicates = make([]roundBaselineReplicate, wrongCount)
		for index := range wrong.Replicates {
			wrong.Replicates[index] = baseline.Replicates[0]
		}
		wrongJson, err := json.Marshal(wrong)
		if err != nil {
			t.Fatal(err)
		}
		if digest, err := validateRoundBaselineJson(worker.settings, &store.job.Round, wrongJson); err == nil || err.Error() != "round baseline identity does not match the frozen round" || digest != "" {
			t.Fatal("wrong baseline count escaped the unchanged frozen identity gate", err)
		}
		if store.claims != 0 || store.hostRegistrations != 0 || store.completions != 0 || guard.forbidden.Load() != 0 || transport.ordinary.Load() != 0 {
			t.Fatal("fixture validation admitted fresh work or eligibility")
		}
	}
}

// Private opt-in metadata is absent by default and cannot change frozen public
// policy bytes, executor host identity or retained original connection pins.
func TestCompetitionExistingControlPreservesFrozenPolicyAndLegacyDefault(t *testing.T) {
	worker, _, _, _, _ := newCompetitionExistingWorkerFixture(t, false)
	settings := worker.settings
	before, err := json.Marshal(settings.DistributedEvaluationPolicy)
	if err != nil {
		t.Fatal(err)
	}
	connection := &settings.distributedCoordinator.Config.Hosts[0]
	originalConfig := connection.AgentConfigSha256
	connection.ExistingControl.AgentConfigSha256 = strings.Repeat("8", 64)
	connection.ExistingControl.AgentCommandSha256 = strings.Repeat("9", 64)
	after, err := json.Marshal(settings.DistributedEvaluationPolicy)
	if err != nil || !bytes.Equal(before, after) || connection.AgentConfigSha256 != originalConfig {
		t.Fatal("observer pins rewrote frozen policy or original agent identity", err)
	}
	connection.ExistingControl = nil
	if competitionExistingControlEnabled(settings) {
		t.Fatal("legacy ordinary connection enabled existing-only authority")
	}
	content, err := json.Marshal(connection)
	if err != nil || bytes.Contains(content, []byte("existing_control")) {
		t.Fatal("legacy connection wire contract acquired an observer role", err)
	}
	settings.distributedCoordinator.Config.Settings.ExistingWitnessMaximumAge = 0
	if err := validateCompetitionExistingControlConnection(*connection, settings.DistributedEvaluationPolicy.Hosts[1], settings.distributedCoordinator.Config.Settings); err != nil {
		t.Fatal("disabled optional role changed ordinary validation", err)
	}
}

// The real coordinator entry bypasses a refusing preflight ONLY for positive
// existing native custody, even while the normal claim-only hold is active.
func TestCompetitionExistingWorkerAttachesUnderPressureAndClaimHold(t *testing.T) {
	worker, evaluator, store, guard, transport := newCompetitionExistingWorkerFixture(t, true)
	if err := worker.Preflight(t.Context()); err == nil {
		t.Fatal("synthetic fresh admission was not refused")
	}
	<-guard.checked
	original := *store.job
	parent, cancelParent := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelParent()
	child, cancelChild := context.WithCancel(parent)
	ticks := make(chan time.Time)
	worker.evaluationTickForTest = ticks
	done := make(chan error, 1)
	go func() { done <- runCompetitionExecutorWorker(child, worker, evaluator) }()
	defer cancelChild()
	select {
	case <-transport.attached:
	case <-guard.checked:
		t.Fatal("existing recovery was stranded behind fresh preflight")
	case <-store.unexpected:
		t.Fatal("existing recovery entered ordinary/expired-only claim")
	case err := <-done:
		t.Fatal("existing coordinator returned before attachment", err)
	case <-parent.Done():
		t.Fatal("existing observer did not attach")
	}
	claimed := <-store.claimed
	ticks <- server.NowUtc()
	<-store.heartbeat
	cancelChild()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal("existing observer did not relinquish joined custody", err)
	}
	if parent.Err() != nil {
		t.Fatal("canceling attached child canceled healthy shared ownership")
	}
	if claimed.AttemptCount != original.AttemptCount || !claimed.StartedAt.Equal(*original.StartedAt) || !claimed.ExecutionDeadline.Equal(*original.ExecutionDeadline) || claimed.WorkerImageDigest != original.WorkerImageDigest || claimed.SlotFencingToken != original.SlotFencingToken+1 {
		t.Fatal("existing attachment changed execution history, source identity or original clocks")
	}
	if store.claims != 1 || store.handbacks != 1 || store.hostRegistrations != 0 || store.completions != 0 || guard.forbidden.Load() != 0 || transport.ordinary.Load() != 0 {
		t.Fatal("attach reached fresh eligibility, evaluation, preparation, upload or premature completion")
	}
	entries, err := os.ReadDir(worker.settings.ArtifactRoot)
	if err != nil || len(entries) != 1 || entries[0].Name() != "rounds" {
		t.Fatal("running attach prepared a new local execution bundle", err)
	}
}

// A signed not-found after adoption stays uncertain. It cannot reactivate the
// ordinary evaluator's lost-start fallback or manufacture a terminal success.
func TestCompetitionExistingWorkerMissingRunNeverFallsBackToStart(t *testing.T) {
	worker, evaluator, store, guard, transport := newCompetitionExistingWorkerFixture(t, true)
	transport.unknown = true
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := worker.attachExisting(ctx, evaluator); done <- err }()
	select {
	case <-transport.attached:
	case err := <-done:
		t.Fatal("attachment returned before the unknown observation", err)
	case <-ctx.Done():
		t.Fatal("existing observer did not reach the explicit barrier")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if store.hostRegistrations != 0 || store.completions != 0 || transport.ordinary.Load() != 0 || guard.forbidden.Load() != 0 {
		t.Fatal("unknown existing state became fresh work or synthetic success")
	}
}

// A second worker observes the retained native run again and obtains another
// custody fence, never another attempt or deadline, after explicit disconnect.
func TestCompetitionExistingWorkerRestartKeepsOriginalExecution(t *testing.T) {
	worker, evaluator, store, guard, transport := newCompetitionExistingWorkerFixture(t, true)
	identity := transport.manifest.Identity
	for range 2 {
		transport.firstFence = store.job.SlotFencingToken
		transport.attached, transport.gate = make(chan struct{}), sync.Once{}
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		done := make(chan error, 1)
		go func() { _, err := worker.attachExisting(ctx, evaluator); done <- err }()
		select {
		case <-transport.attached:
		case err := <-done:
			cancel()
			t.Fatal("restart returned before native attachment", err)
		case <-ctx.Done():
			cancel()
			t.Fatal("restart did not reach the explicit barrier")
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		if !sameCompetitionExistingJob(store.job, identity) {
			t.Fatal("restart changed the original run/cycle/attempt/clock")
		}
	}
	if store.claims != 2 || store.handbacks != 2 || store.hostRegistrations != 0 || store.completions != 0 || transport.ordinary.Load() != 0 || guard.forbidden.Load() != 0 {
		t.Fatal("restart admitted or renewed fresh execution")
	}
}

// An attachment after the original deadline can only stop/record exhausted
// infrastructure time. It never gives the native execution another clock.
func TestCompetitionExistingWorkerOriginalDeadlineCannotRestartOrPublishScore(t *testing.T) {
	worker, evaluator, store, guard, transport := newCompetitionExistingWorkerFixture(t, true)
	deadline := server.NowUtc().Add(-time.Second).Truncate(time.Microsecond)
	started := deadline.Add(-time.Duration(worker.settings.EvaluationPolicy.ScoreTimeoutSeconds) * time.Second)
	store.job.StartedAt, store.job.ExecutionDeadline = &started, &deadline
	manifest, err := competitionExistingManifest(t.Context(), worker.settings, store.job, evaluator.host)
	if err != nil {
		t.Fatal(err)
	}
	transport.manifest = manifest
	store.budgetCompletion = make(chan EvaluationOutcome, 1)
	occupied, err := worker.attachExisting(t.Context(), evaluator)
	if err != nil || !occupied {
		t.Fatal("expired existing custody could not be finished within its original clock", err)
	}
	outcome := <-store.budgetCompletion
	if outcome.Score != nil || outcome.Error == nil || outcome.Error.Code != "evaluation_time_budget_exhausted" || transport.cancels.Load() != 1 || guard.forbidden.Load() != 0 || transport.ordinary.Load() != 0 || store.hostRegistrations != 0 || !store.job.ExecutionDeadline.Equal(deadline) {
		t.Fatal("existing deadline became fresh execution, eligibility or late success")
	}
}

// Actual observation/minting rejects signed wrong host/run/cycle/attempt,
// clocks, config or proof, rather than constructing a reusable bypass token.
func TestCompetitionExistingObservationRejectsWrongOriginalAuthority(t *testing.T) {
	for _, mutate := range []func(*competitionagent.Response){
		func(response *competitionagent.Response) { response.HostId = "synthetic-other-host" },
		func(response *competitionagent.Response) { response.RunId = strings.Repeat("0", 64) },
		func(response *competitionagent.Response) { response.FencingToken++ },
		func(response *competitionagent.Response) { response.Identity.Cycle++ },
		func(response *competitionagent.Response) { response.Identity.Attempt++ },
		func(response *competitionagent.Response) { response.Identity.SourceEpoch++ },
		func(response *competitionagent.Response) {
			response.Identity.DeadlineAt = response.Identity.DeadlineAt.Add(time.Second)
		},
		func(response *competitionagent.Response) {
			response.ExistingRun.OriginalConfigSha256 = strings.Repeat("0", 64)
		},
		func(response *competitionagent.Response) {
			response.ExistingRun.ResourceBoundarySha256 = strings.Repeat("0", 64)
		},
		func(response *competitionagent.Response) { response.ExistingRun.NativeProofSha256 = "" },
		func(response *competitionagent.Response) { response.ExistingRun = nil },
		func(response *competitionagent.Response) {
			response.State, response.ExistingRun.ExecutionStarted = "accepted", false
		},
		func(response *competitionagent.Response) {
			response.State, response.ExistingRun.ExecutionStarted = "succeeded", false
			response.ExistingRun.CleanupVerified, response.ExistingRun.StopCleanupVerified = true, true
		},
	} {
		worker, evaluator, store, guard, transport := newCompetitionExistingWorkerFixture(t, true)
		transport.mutate = mutate
		if witness, err := evaluator.observeExisting(t.Context(), worker.settings, store.job); err == nil || witness != nil {
			t.Fatal("wrong original authority minted an attachment capability")
		}
		if store.claims != 0 || store.hostRegistrations != 0 || guard.forbidden.Load() != 0 || transport.ordinary.Load() != 0 {
			t.Fatal("invalid observation reached admission or eligibility")
		}
	}
}

// The RPC's actual start bounds freshness; a delayed reply does not obtain a
// fresh age allowance merely because authentication eventually succeeds.
func TestCompetitionExistingObservationAgeIncludesWholeRpc(t *testing.T) {
	worker, evaluator, store, _, transport := newCompetitionExistingWorkerFixture(t, true)
	now := server.NowUtc()
	evaluator.now = func() time.Time { return now }
	transport.mutate = func(*competitionagent.Response) {
		now = now.Add(worker.settings.distributedCoordinator.Config.Settings.ExistingWitnessMaximumAge)
	}
	if witness, err := evaluator.observeExisting(t.Context(), worker.settings, store.job); !errors.Is(err, ErrLeaseLost) || witness != nil {
		t.Fatal("late authenticated reply refreshed its own observation age", err)
	}
}

// A transport implementation is not itself proof. Minting independently
// verifies the original frozen host key before constructing private authority.
type competitionExistingUnsignedTransport struct {
	*competitionExistingTestTransport
}

func (self competitionExistingUnsignedTransport) ExchangeExisting(ctx context.Context, request competitionagent.Request, directory string) (competitionagent.Response, error) {
	response, err := self.competitionExistingTestTransport.ExchangeExisting(ctx, request, directory)
	response.Signature = ""
	return response, err
}

func TestCompetitionExistingObservationCannotMintUnsignedTransportWitness(t *testing.T) {
	worker, evaluator, store, _, transport := newCompetitionExistingWorkerFixture(t, true)
	evaluator.transport = competitionExistingUnsignedTransport{competitionExistingTestTransport: transport}
	if witness, err := evaluator.observeExisting(t.Context(), worker.settings, store.job); !errors.Is(err, competitionagent.ErrIntegrity) || witness != nil {
		t.Fatal("arbitrary transport data minted an existing-run capability", err)
	}
}
