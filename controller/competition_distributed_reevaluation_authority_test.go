// Synthetic signed authority controls isolate every new distributed grant
// boundary from historical singleton classes and private native inventories.
package controller

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/competitionagent"
	"github.com/urnetwork/server/model"
)

// No production identity, receipt, key, clock or incident enters this fixture.
func competitionDistributedRecoveryRequestForTest(t testing.TB) (*Settings, *Principal, model.CompetitionJobReevaluationRequest) {
	t.Helper()
	settings, principal, request := newCompetitionLegacyArchiveRequestFixture(t)
	a := &request.Authorization
	a.Schema, a.Kind, a.IncidentClass = 2, model.CompetitionDistributedJobReevaluationAuthorizationKind, model.CompetitionJobReevaluationDistributedInfrastructure
	a.ExpectedAttemptCount, a.IncidentAttempt = 1, 1
	policy := competitionDistributedSlotTestPolicy(t)
	settings.DistributedEvaluationPolicy = policy
	originalStarted := a.AuthorizedAt.Add(-4 * time.Hour)
	a.Distributed = &model.CompetitionDistributedJobReevaluationAuthorization{Schema: 1, Reason: model.CompetitionDistributedReevaluationRetentionBudget,
		OriginalHostId: policy.Hosts[0].HostId, OriginalSlotId: policy.Hosts[0].SlotId, OriginalFencingToken: 3, NativeFencingToken: 2,
		OriginalRunId: competitionagent.RunId(a.JobId.String(), 0, 1), OriginalStartedAt: originalStarted, OriginalDeadlineAt: originalStarted.Add(3 * time.Hour),
		OriginalManifestSha256: strings.Repeat("c", 64), OriginalRequestSha256: strings.Repeat("d", 64),
		SourceLockSha256: strings.Repeat("a", 64), EvaluatorImageDigest: settings.EvaluatorImageDigest, PolicyApprovalSha256: policy.ApprovalSha256,
		ClosureEvidenceSha256: strings.Repeat("b", 64), SupervisorOwnerId: "synthetic-supervisor", SupervisorFencingToken: 1,
		IdleSlots: []model.CompetitionDistributedReevaluationIdleSlot{{HostId: policy.Hosts[0].HostId, SlotId: 1, FencingToken: 3}, {HostId: policy.Hosts[1].HostId, SlotId: 2, FencingToken: 5}}}
	request.AuthorizationSha256 = competitionRecoveryTestDigest(t, a)
	return settings, principal, request
}

// Typed canonical copies keep pointer/slice mutation controls independent.
func competitionRecoveryTestCopy[T any](t testing.TB, original T) T {
	t.Helper()
	content, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var copy T
	if err := json.Unmarshal(content, &copy); err != nil {
		t.Fatal(err)
	}
	return copy
}

// A malformed proof must not gain authority merely by recomputing its digest.
func competitionRecoveryTestDigest(t testing.TB, value any) string {
	t.Helper()
	digest, err := model.CompetitionAutomationDigest(value)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

// The transport returns actual Ed25519 envelopes and enforces the finite
// read-only operation vocabulary. Mutation hooks run at an exact call boundary.
type competitionDistributedRecoveryTestTransport struct {
	t              testing.TB
	host           CompetitionExecutorHostPolicy
	auth           model.CompetitionJobReevaluationAuthorization
	round          *roundRecord
	mutate         func(*competitionagent.Response)
	before         func(competitionagent.Request)
	calls          *int
	originalStatus *competitionagent.Response
}

// Signed responses are generated using the visibly synthetic policy seeds.
func (self competitionDistributedRecoveryTestTransport) Exchange(ctx context.Context, request competitionagent.Request, inputDirectory, outputDirectory string) (competitionagent.Response, error) {
	if inputDirectory != "" || outputDirectory != "" || (request.Operation != "availability" && request.Operation != "status") {
		self.t.Fatal("recovery attempted a mutating native operation or file transfer")
	}
	if self.calls != nil {
		*self.calls++
	}
	if self.before != nil {
		self.before(request)
	}
	if err := ctx.Err(); err != nil {
		return competitionagent.Response{}, err
	}
	d := self.auth.Distributed
	response := competitionagent.Response{Schema: 1, HostId: self.host.HostId, State: "available"}
	if request.Operation == "status" {
		response.RunId, response.FencingToken = request.RunId, request.FencingToken
		response.Identity = &competitionagent.Identity{HostId: self.host.HostId, RunId: d.OriginalRunId, JobId: self.auth.JobId.String(), RoundId: self.auth.RoundId.String(), RoundEpoch: self.round.Epoch, SourceEpoch: evaluationSourceEpoch(self.round), Cycle: 0, Attempt: self.auth.ExpectedAttemptCount, StartedAt: d.OriginalStartedAt, DeadlineAt: d.OriginalDeadlineAt}
		response.State, response.ManifestSha256, response.RequestSha256 = "succeeded", d.OriginalManifestSha256, d.OriginalRequestSha256
		response.Files = []competitionagent.File{{Path: "synthetic-original/worker-result.json", Sha256: strings.Repeat("e", 64), Bytes: 21}}
		if d.Reason == model.CompetitionDistributedReevaluationLegacyPrelaunch {
			response.State, response.ExitCode, response.ErrorCode = "failed", -1, "legacy_prelaunch_infrastructure_failure"
		}
		if d.Reason == model.CompetitionJobReevaluationOperatorRequestedPatchRetry {
			response.State, response.ExitCode, response.ErrorCode = "failed", 1, "evaluator_process_failed"
		}
		if self.originalStatus != nil {
			response = competitionRecoveryTestCopy(self.t, *self.originalStatus)
		}
	}
	if self.mutate != nil {
		self.mutate(&response)
	}
	seedByte := byte(1)
	if self.host.SlotId == 2 {
		seedByte = 2
	}
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{seedByte}, ed25519.SeedSize))
	if err := competitionagent.SignResponse(&response, key); err != nil {
		self.t.Fatal(err)
	}
	return response, nil
}

// Request validation rejects singleton/unknown/mixed authority before any SQL
// or native transport can be reached, even with a fresh canonical outer hash.
func TestCompetitionDistributedRecoveryRequestHasSeparateAuthority(t *testing.T) {
	settings, principal, request := competitionDistributedRecoveryRequestForTest(t)
	if err := validateCompetitionDistributedRecoveryRequest(settings, principal, request); err != nil {
		t.Fatal(err)
	}
	for _, change := range []struct {
		name   string
		mutate func(*model.CompetitionJobReevaluationAuthorization)
	}{
		{name: "singleton-schema", mutate: func(a *model.CompetitionJobReevaluationAuthorization) { a.Schema = 1 }},
		{name: "singleton-kind", mutate: func(a *model.CompetitionJobReevaluationAuthorization) {
			a.Kind = model.CompetitionJobReevaluationAuthorizationKind
		}},
		{name: "singleton-incident", mutate: func(a *model.CompetitionJobReevaluationAuthorization) {
			a.IncidentClass = model.CompetitionJobReevaluationHeartbeatBudget
		}},
		{name: "missing-closure", mutate: func(a *model.CompetitionJobReevaluationAuthorization) { a.Distributed.ClosureEvidenceSha256 = "" }},
		{name: "unknown-diagnosis", mutate: func(a *model.CompetitionJobReevaluationAuthorization) { a.Distributed.Reason = "synthetic-unknown" }},
		{name: "another-run", mutate: func(a *model.CompetitionJobReevaluationAuthorization) {
			a.Distributed.OriginalRunId = strings.Repeat("f", 64)
		}},
		{name: "another-cycle", mutate: func(a *model.CompetitionJobReevaluationAuthorization) { a.ExpectedCycle = 1 }},
		{name: "zero-native-fence", mutate: func(a *model.CompetitionJobReevaluationAuthorization) { a.Distributed.NativeFencingToken = 0 }},
		{name: "different-deadline", mutate: func(a *model.CompetitionJobReevaluationAuthorization) {
			a.Distributed.OriginalDeadlineAt = a.Distributed.OriginalDeadlineAt.Add(time.Second)
		}},
		{name: "missing-host", mutate: func(a *model.CompetitionJobReevaluationAuthorization) {
			a.Distributed.IdleSlots = a.Distributed.IdleSlots[:1]
		}},
		{name: "duplicate-host", mutate: func(a *model.CompetitionJobReevaluationAuthorization) {
			a.Distributed.IdleSlots[1].HostId = a.Distributed.IdleSlots[0].HostId
		}},
		{name: "duplicate-slot", mutate: func(a *model.CompetitionJobReevaluationAuthorization) { a.Distributed.IdleSlots[1].SlotId = 1 }},
		{name: "mutable-image", mutate: func(a *model.CompetitionJobReevaluationAuthorization) {
			a.Distributed.EvaluatorImageDigest = "synthetic-image:latest"
		}},
		{name: "not-single-use", mutate: func(a *model.CompetitionJobReevaluationAuthorization) { a.SingleUse = false }},
		{name: "borrowed-singleton-inventory", mutate: func(a *model.CompetitionJobReevaluationAuthorization) {
			a.FailureManifestSha256 = strings.Repeat("c", 64)
		}},
	} {
		changed := competitionRecoveryTestCopy(t, request)
		change.mutate(&changed.Authorization)
		changed.AuthorizationSha256 = competitionRecoveryTestDigest(t, changed.Authorization)
		if err := validateCompetitionDistributedRecoveryRequest(settings, principal, changed); err == nil {
			t.Fatalf("%s accepted", change.name)
		}
	}
	if err := validateCompetitionReevaluationRequest(settings, principal, request); err == nil {
		t.Fatal("historical singleton validator accepted a distributed grant")
	}
}

// A separately qualified version cannot silently satisfy the old capability,
// or use an old runtime qualification to authorize this new distributed class.
func TestCompetitionDistributedRecoveryRuntimeCannotBorrowSingletonCapability(t *testing.T) {
	settings, _, request := competitionDistributedRecoveryRequestForTest(t)
	a := request.Authorization
	proof := model.CompetitionJobReevaluationRuntime{Schema: 2, Kind: model.CompetitionDistributedJobReevaluationRuntimeKind, Capability: model.CompetitionDistributedJobReevaluationCapability, Accepted: true,
		IncidentClasses: []string{model.CompetitionJobReevaluationDistributedInfrastructure}, SourceCommit: a.FixedSourceCommit, WorkerSha256: a.FixedWorkerSha256, WorkerImageDigest: a.FixedWorkerImageDigest,
		ApiImageDigest: a.ApiImageDigest, EvidenceSha256: strings.Repeat("f", 64), ApiCompatible: true, WorkerCompatible: true}
	for _, change := range []func(*model.CompetitionJobReevaluationRuntime){func(*model.CompetitionJobReevaluationRuntime) {}, func(p *model.CompetitionJobReevaluationRuntime) { p.Schema = 1 }, func(p *model.CompetitionJobReevaluationRuntime) {
		p.Capability = model.CompetitionJobReevaluationCapability
	}, func(p *model.CompetitionJobReevaluationRuntime) {
		p.IncidentClasses = []string{model.CompetitionJobReevaluationHeartbeatBudget}
	}} {
		copy := competitionRecoveryTestCopy(t, proof)
		change(&copy)
		err := BindCompetitionJobReevaluationRuntime(settings, copy, competitionRecoveryTestDigest(t, copy))
		if (copy.Schema == 2 && copy.Capability == model.CompetitionDistributedJobReevaluationCapability && copy.IncidentClasses[0] == model.CompetitionJobReevaluationDistributedInfrastructure) != (err == nil) {
			t.Fatal("runtime capability/class boundary changed")
		}
	}
	if err := BindCompetitionJobReevaluationRuntime(settings, proof, competitionRecoveryTestDigest(t, proof)); err != nil {
		t.Fatal(err)
	}
	if err := validateCompetitionReevaluationRuntime(settings, a); err == nil {
		t.Fatal("singleton runtime validator borrowed the distributed capability")
	}
}

// Each meaningful native state is tested after re-signing, so these controls
// exercise semantic closure and identity checks rather than signature errors.
func TestCompetitionDistributedRecoveryNativeClosureRequiresEveryExactHost(t *testing.T) {
	settings, _, request := competitionDistributedRecoveryRequestForTest(t)
	round := &roundRecord{RoundResult: RoundResult{RoundId: request.Authorization.RoundId, Epoch: 2}}
	for _, change := range []struct {
		name    string
		mutate  func(*competitionagent.Response)
		allowed bool
	}{
		{name: "closed", allowed: true},
		{name: "occupied-host", mutate: func(r *competitionagent.Response) {
			if r.Identity == nil {
				r.State = "occupied"
			}
		}},
		{name: "unknown-original", mutate: func(r *competitionagent.Response) {
			if r.Identity != nil {
				r.State = "unknown"
			}
		}},
		{name: "incomplete-stop", mutate: func(r *competitionagent.Response) {
			if r.Identity != nil {
				r.ErrorCode = "stop_cleanup_pending"
			}
		}},
		{name: "wrong-run-clock", mutate: func(r *competitionagent.Response) {
			if r.Identity != nil {
				r.Identity.DeadlineAt = r.Identity.DeadlineAt.Add(time.Second)
			}
		}},
		{name: "wrong-cycle", mutate: func(r *competitionagent.Response) {
			if r.Identity != nil {
				r.Identity.Cycle = 1
			}
		}},
		{name: "substituted-native-fence", mutate: func(r *competitionagent.Response) {
			if r.Identity != nil {
				r.FencingToken++
			}
		}},
		{name: "substituted-native-manifest", mutate: func(r *competitionagent.Response) {
			if r.Identity != nil {
				r.ManifestSha256 = strings.Repeat("f", 64)
			}
		}},
		{name: "wrong-host", mutate: func(r *competitionagent.Response) { r.HostId = "synthetic-other-host" }},
		{name: "unexpected-inventory", mutate: func(r *competitionagent.Response) {
			if r.Identity == nil {
				r.Files = []competitionagent.File{{Path: "synthetic-extra", Bytes: 1, Sha256: strings.Repeat("a", 64)}}
			}
		}},
	} {
		factory := func(_ *Settings, host CompetitionExecutorHostPolicy) (competitionAgentTransport, error) {
			return competitionDistributedRecoveryTestTransport{t: t, host: host, auth: request.Authorization, round: round, mutate: change.mutate}, nil
		}
		proof, err := observeCompetitionDistributedRecovery(t.Context(), settings, round, request, factory)
		if (err == nil) != change.allowed || change.allowed && (proof == nil || len(proof.Availability) != 2) {
			t.Fatalf("%s proof=%t error=%v", change.name, proof != nil, err)
		}
	}
}

// An authentically signed failure of the wrong stage cannot masquerade as the
// reviewed no-execution legacy closure, and neither reason grants cancellation.
func TestCompetitionDistributedRecoveryLegacyPrelaunchClosureIsDistinct(t *testing.T) {
	settings, _, request := competitionDistributedRecoveryRequestForTest(t)
	request.Authorization.Distributed.Reason = model.CompetitionDistributedReevaluationLegacyPrelaunch
	round := &roundRecord{RoundResult: RoundResult{RoundId: request.Authorization.RoundId, Epoch: 2}}
	factory := func(_ *Settings, host CompetitionExecutorHostPolicy) (competitionAgentTransport, error) {
		return competitionDistributedRecoveryTestTransport{t: t, host: host, auth: request.Authorization, round: round}, nil
	}
	if _, err := observeCompetitionDistributedRecovery(t.Context(), settings, round, request, factory); err != nil {
		t.Fatal(err)
	}
	factory = func(_ *Settings, host CompetitionExecutorHostPolicy) (competitionAgentTransport, error) {
		return competitionDistributedRecoveryTestTransport{t: t, host: host, auth: request.Authorization, round: round, mutate: func(r *competitionagent.Response) {
			if r.Identity != nil {
				r.ErrorCode = "synthetic_evaluator_failure"
			}
		}}, nil
	}
	if _, err := observeCompetitionDistributedRecovery(t.Context(), settings, round, request, factory); err == nil {
		t.Fatal("unrelated failed evaluator borrowed no-execution closure authority")
	}
}

// Exact positive lifetime and expiry boundaries use deterministic clocks,
// including a backwards database cut and future-origin proof control.
func TestCompetitionDistributedRecoveryProofAgeFailsClosed(t *testing.T) {
	start := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	for _, c := range []struct {
		end      time.Time
		lifetime time.Duration
		allowed  bool
	}{
		{end: start, lifetime: time.Minute, allowed: true}, {end: start.Add(time.Minute - time.Nanosecond), lifetime: time.Minute, allowed: true},
		{end: start.Add(time.Minute), lifetime: time.Minute}, {end: start.Add(time.Minute + time.Nanosecond), lifetime: time.Minute},
		{end: start.Add(-time.Nanosecond), lifetime: time.Minute}, {end: start, lifetime: 0}, {end: start, lifetime: -time.Second},
	} {
		if competitionDistributedRecoveryProofTimely(start, c.end, c.lifetime) != c.allowed {
			t.Fatal("native proof lifetime boundary changed")
		}
	}
	if competitionDistributedRecoveryProofTimely(time.Time{}, start, time.Minute) {
		t.Fatal("missing original database cut accepted")
	}
}

// Signature authenticity is an independent gate, not implied by a matching
// host name, terminal state, availability byte or a self-consistent digest.
func TestCompetitionDistributedRecoveryRejectsUnsignedAndWrongKeyEnvelope(t *testing.T) {
	settings, _, _ := competitionDistributedRecoveryRequestForTest(t)
	host := settings.DistributedEvaluationPolicy.Hosts[0]
	response := competitionagent.Response{Schema: 1, HostId: host.HostId, State: "available"}
	if err := validateCompetitionDistributedRecoveryEnvelope(response, host); err == nil {
		t.Fatal("unsigned availability accepted")
	}
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, ed25519.SeedSize))
	if err := competitionagent.SignResponse(&response, key); err != nil {
		t.Fatal(err)
	}
	if err := validateCompetitionDistributedRecoveryEnvelope(response, host); err == nil {
		t.Fatal("another executor key accepted")
	}
}

// Parent cancellation cannot create a partial native observation or grant.
func TestCompetitionDistributedRecoveryObservationPreservesCancellation(t *testing.T) {
	settings, _, request := competitionDistributedRecoveryRequestForTest(t)
	round := &roundRecord{RoundResult: RoundResult{RoundId: request.Authorization.RoundId, Epoch: 2}}
	ctx, cancel := context.WithCancel(t.Context())
	factory := func(_ *Settings, host CompetitionExecutorHostPolicy) (competitionAgentTransport, error) {
		return competitionDistributedRecoveryTestTransport{t: t, host: host, auth: request.Authorization, round: round, before: func(competitionagent.Request) { cancel() }}, nil
	}
	if proof, err := observeCompetitionDistributedRecovery(ctx, settings, round, request, factory); proof != nil || !errors.Is(ctx.Err(), context.Canceled) || err == nil {
		t.Fatal("canceled observation retained grant authority")
	}
}

// The separately approved execution hotfix is still selected for the new
// native identity without rewriting frozen baseline/image/source bytes.
func TestCompetitionDistributedRecoveryManifestKeepsApprovedSecurityException(t *testing.T) {
	fixture := securityToolchainFixture(t)
	beforeBaseline, err := json.Marshal(fixture.job.RoundBaseline)
	if err != nil {
		t.Fatal(err)
	}
	beforePolicy := append([]byte(nil), fixture.job.Round.PolicyJson...)
	originalStarted := *fixture.job.StartedAt
	cycleStarted := originalStarted.Add(24 * time.Hour)
	deadline := cycleStarted.Add(3 * time.Hour)
	fixture.job.EvaluationCycle, fixture.job.CycleAttemptOffset, fixture.job.AttemptCount = 1, 1, 2
	fixture.job.CycleStartedAt, fixture.job.ExecutionDeadline = &cycleStarted, &deadline
	manifest, err := competitionAgentManifestForJob(fixture.settings, fixture.job, fixture.host)
	if err != nil || manifest.Identity.Cycle != 1 || manifest.Identity.Attempt != 2 || !manifest.Identity.StartedAt.Equal(cycleStarted) || !manifest.Identity.DeadlineAt.Equal(deadline) ||
		manifest.Identity.RunId != competitionagent.RunId(fixture.job.JobId.String(), 1, 2) || manifest.Identity.RunId == competitionagent.RunId(fixture.job.JobId.String(), 0, 1) ||
		manifest.EvaluatorImageDigest != fixture.settings.securityToolchainException.EvaluatorImageDigest || manifest.RoundBaselineSha256 != fixture.job.RoundBaseline.Sha256 ||
		fixture.job.EvaluatorImageDigest != fixture.settings.securityToolchainException.PreviousEvaluatorImageDigest || !fixture.job.StartedAt.Equal(originalStarted) {
		t.Fatal("new cycle changed history or lost exact approved execution exception", err)
	}
	afterBaseline, err := json.Marshal(fixture.job.RoundBaseline)
	if err != nil || !bytes.Equal(beforeBaseline, afterBaseline) || !bytes.Equal(beforePolicy, fixture.job.Round.PolicyJson) {
		t.Fatal("new dispatch relabeled frozen control/source/image")
	}
}
