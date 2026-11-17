// Already-started native custody has a separate observation/attachment path.
// It creates neither execution inputs nor another eligibility/attempt clock.
package controller

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync/atomic"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/competitionagent"
)

// Only a successfully authenticated live existing-control RPC can construct
// this local, single-use claim capability. It is never deserialized from input.
type competitionExistingRunWitness struct {
	consumed              atomic.Bool
	evaluator             *competitionAgentEvaluator
	manifest              competitionagent.StartManifest
	response              competitionagent.Response
	identity              competitionagent.Identity
	observedAt, expiresAt time.Time
	leaseOwner            string
	leaseExpiresAt        time.Time
	slotFence             int64
	slotId                int
	workerImageDigest     string
	supervisorOwner       string
	supervisorFence       int64
}

// A finite snapshot names only an occupied frozen slot. The existing normal
// claim transaction supplies the independent exact compare-and-swap boundary.
type competitionExistingRunStore interface {
	existingCompetitionJob(context.Context, *Settings) (*queuedJob, error)
}

// Missing/unstarted/cleanup-in-progress is bounded uncertainty, never success
// or permission to restart. Only authenticated contradictions reject integrity.
var errCompetitionExistingUnknown = errors.New("existing native execution custody remains unknown")

// Opt-in deployment metadata cannot weaken the ordinary evaluator connection.
func competitionExistingControlConnectionForSettings(settings *Settings) (competitionExecutorConnection, bool) {
	if settings == nil || settings.distributedCoordinator == nil || settings.executorHostId == "" {
		return competitionExecutorConnection{}, false
	}
	for _, connection := range settings.distributedCoordinator.Config.Hosts {
		if connection.HostId == settings.executorHostId && connection.ExistingControl != nil {
			return connection, true
		}
	}
	return competitionExecutorConnection{}, false
}

// The historical path is unchanged unless a separate role was provisioned.
func competitionExistingControlEnabled(settings *Settings) bool {
	_, present := competitionExistingControlConnectionForSettings(settings)
	return present
}

// Retained evidence never supplies a host check. Only exact existing authority
// may skip claim-only hold and the new-attempt admission path.
func (self *Worker) attachExisting(ctx context.Context, evaluator *competitionAgentEvaluator) (bool, error) {
	if !competitionExistingControlEnabled(self.settings) {
		return false, nil
	}
	store, ok := self.store.(competitionExistingRunStore)
	if !ok {
		return false, errors.New("existing execution store capability is unavailable")
	}
	job, err := store.existingCompetitionJob(ctx, self.settings)
	if err != nil || job == nil {
		return false, err
	}
	if job.LeaseExpiresAt == nil || evaluator.existingNow().Before(*job.LeaseExpiresAt) {
		return true, ErrLeaseLost
	}
	witness, err := evaluator.observeExisting(ctx, self.settings, job)
	if err != nil {
		return true, err
	}
	defer clear(witness.manifest.EvaluatorRequest)
	selected := *self.settings
	selected.executorReconcileOnly, selected.executorReconcileExpiredOnly, selected.executorFreshOnly = true, false, false
	selected.executorExistingWitness = witness
	claimed, err := self.store.Claim(ctx, &selected, self.workerId, self.workerImageDigest)
	if err != nil || claimed == nil {
		if err == nil {
			err = ErrLeaseLost
		}
		return true, err
	}
	if claimed.SlotFencingToken != witness.slotFence+1 || !sameCompetitionExistingJob(claimed, witness.identity) {
		return true, ErrLeaseLost
	}
	return true, self.evaluateOneOwned(ctx, claimed, HostSelfCheck{}, witness)
}

// Local age belongs to the actual RPC start, not a peer timestamp. Fresh SSH
// command/host authentication plus the signed exact run/fence frame supplies
// origin freshness; a proof cannot be replayed into a second claim.
func (self *competitionAgentEvaluator) observeExisting(ctx context.Context, settings *Settings, job *queuedJob) (*competitionExistingRunWitness, error) {
	if settings.automationLease == nil || job.LeaseExpiresAt == nil {
		return nil, ErrLeaseLost
	}
	frozen, err := settingsForFrozenRound(settings, &job.Round)
	if err != nil || validateRoundBaselineRecord(frozen, &job.Round, job.RoundBaseline) != nil || validateCompetitionSecurityToolchainScope(frozen, &job.Round, job.RoundBaseline) != nil {
		return nil, ErrLeaseLost
	}
	settings = frozen
	manifest, err := competitionExistingManifest(ctx, settings, job, self.host)
	if err != nil {
		return nil, err
	}
	keep := false
	defer func() {
		if !keep {
			clear(manifest.EvaluatorRequest)
		}
	}()
	started := self.existingNow()
	maximumAge := settings.distributedCoordinator.Config.Settings.ExistingWitnessMaximumAge
	if maximumAge <= 0 {
		return nil, ErrLeaseLost
	}
	response, err := self.exchangeRun(ctx, competitionagent.Request{Operation: "status", RunId: manifest.Identity.RunId, FencingToken: job.SlotFencingToken}, "", "", true)
	if err != nil {
		return nil, err
	}
	if err := validateCompetitionExistingResponse(settings, job, manifest, response, self.host); err != nil {
		return nil, err
	}
	expiresAt := started.Add(maximumAge)
	if !self.existingNow().Before(expiresAt) {
		return nil, ErrLeaseLost
	}
	keep = true
	return &competitionExistingRunWitness{evaluator: self, manifest: manifest, response: response, identity: manifest.Identity,
		observedAt: started, expiresAt: expiresAt, leaseOwner: job.LeaseOwner, leaseExpiresAt: *job.LeaseExpiresAt,
		slotFence: job.SlotFencingToken, workerImageDigest: job.WorkerImageDigest,
		slotId:          job.SlotId,
		supervisorOwner: settings.automationLease.OwnerId, supervisorFence: settings.automationLease.FencingToken}, nil
}

// Tests own exact observation/CAS boundaries without wall-clock sleeps.
func (self *competitionAgentEvaluator) existingNow() time.Time {
	if self.now != nil {
		return self.now()
	}
	return server.NowUtc()
}

// No filesystem preparation, helper execution or upload occurs. Existing
// immutable local bytes are read only to independently authenticate the exact
// already-committed manifest and later artifact request normalization.
func competitionExistingManifest(ctx context.Context, settings *Settings, job *queuedJob, host CompetitionExecutorHostPolicy) (competitionagent.StartManifest, error) {
	manifest, err := competitionAgentManifestForJob(settings, job, host)
	if err != nil {
		return manifest, err
	}
	providers, err := readRoundWorkload(ctx, settings, &job.Round)
	if err != nil {
		return manifest, err
	}
	defer clear(providers)
	seed, err := revealRoundSecret(settings, &job.Round)
	if err != nil {
		return manifest, err
	}
	for _, input := range []struct {
		name    string
		content []byte
	}{
		{name: "canonical.patch", content: job.Patch}, {name: "providers.yml", content: providers}, {name: "round-baseline.json", content: job.RoundBaseline.Json},
	} {
		digest := sha256.Sum256(input.content)
		manifest.Inputs = append(manifest.Inputs, competitionagent.File{Path: input.name, Sha256: hex.EncodeToString(digest[:]), Bytes: int64(len(input.content))})
	}
	request := evaluatorRequestForJob(settings, job, seed, ".", "canonical.patch", "providers.yml")
	request.RoundBaselinePath, request.ConfigLocalDirectory, request.VaultLocalDirectory = "round-baseline.json", "@config-local", "@vault-local"
	request.EvaluationPolicy.HostQualificationSha256, request.EvaluatorImageDigest = manifest.QualificationSha256, manifest.EvaluatorImageDigest
	if value := competitionExecutionSecurityToolchain(settings); value != nil {
		request.EvaluationPolicy.SimulatorSha256, request.EvaluationPolicy.ScorerSha256 = value.InstrumentSha256, value.InstrumentSha256
	}
	manifest.EvaluatorRequest, err = json.Marshal(request)
	request.RoundSeedHex = ""
	return manifest, err
}

// A wire document alone never grants claim authority: the constructor above
// additionally requires the freshly invoked closed transport and local age.
func validateCompetitionExistingResponse(settings *Settings, job *queuedJob, manifest competitionagent.StartManifest, response competitionagent.Response, host CompetitionExecutorHostPolicy) error {
	key, signatureErr := base64.StdEncoding.DecodeString(host.AgentPublicKeyBase64)
	if signatureErr != nil || len(key) != ed25519.PublicKeySize || competitionagent.VerifyResponse(response, key) != nil {
		return errors.Join(competitionagent.ErrIntegrity, errors.New("existing observation lacks its original independent host signature"))
	}
	if response.State == "unknown" || response.State == "accepted" || response.State == "not_found" {
		return errCompetitionExistingUnknown
	}
	connection, present := competitionExistingControlConnectionForSettings(settings)
	evidence := response.ExistingRun
	digest, err := competitionagent.ManifestDigest(manifest)
	validState := response.State == "running" && evidence != nil && evidence.ExecutionStarted && !evidence.CleanupVerified && !evidence.StopCleanupVerified ||
		(response.State == "succeeded" || response.State == "failed" || response.State == "canceled" || response.State == "expired") && evidence != nil && evidence.CleanupVerified && evidence.StopCleanupVerified && (response.State != "succeeded" || evidence.ExecutionStarted)
	if !present || evidence == nil || err != nil || response.HostId != host.HostId || response.RunId != manifest.Identity.RunId || response.FencingToken != job.SlotFencingToken ||
		response.Schema != competitionagent.Schema || len(response.SelfCheck) != 0 || !sha256Pattern.MatchString(response.RequestSha256) ||
		!sameCompetitionAgentIdentity(response.Identity, &manifest.Identity) || response.ManifestSha256 != digest || !validState || response.ErrorCode != "" && response.State == "running" ||
		evidence.ApprovalSha256 != connection.ExistingControl.ApprovalSha256 || evidence.OriginalConfigSha256 != connection.AgentConfigSha256 ||
		evidence.ResourceBoundarySha256 != connection.ExistingControl.ResourceBoundarySha256 || !sha256Pattern.MatchString(evidence.CleanupPlanSha256) || !sha256Pattern.MatchString(evidence.NativeProofSha256) {
		return errors.Join(competitionagent.ErrIntegrity, errors.New("existing execution lacks exact native original-run evidence"))
	}
	return nil
}

// The SQL callback rechecks every snapshot field after all row/fence waits.
// Immutable admission and baseline gates remain independently mandatory.
func (self *competitionExistingRunWitness) matches(settings *Settings, job *queuedJob, now time.Time) bool {
	return self != nil && self.evaluator != nil && self.consumed.Load() && settings.automationLease != nil &&
		settings.automationLease.OwnerId == self.supervisorOwner && settings.automationLease.FencingToken == self.supervisorFence &&
		!now.Before(self.observedAt) && now.Before(self.expiresAt) && sameCompetitionExistingJob(job, self.identity) &&
		job.SlotFencingToken == self.slotFence && job.LeaseOwner == self.leaseOwner && job.LeaseExpiresAt != nil && job.LeaseExpiresAt.Equal(self.leaseExpiresAt) &&
		job.SlotId == self.slotId && job.SlotId == settings.executorSlotId && job.ExecutorHostId == settings.executorHostId &&
		job.WorkerImageDigest == self.workerImageDigest && !now.Before(*job.LeaseExpiresAt)
}

// UTC location changes cannot change the original execution clock.
func sameCompetitionExistingJob(job *queuedJob, identity competitionagent.Identity) bool {
	if job == nil || job.State != "running" || job.StartedAt == nil || job.ExecutionDeadline == nil || job.Round.Canceled || job.Round.FinalizedAt != nil || job.Round.AutomationSealedAt != nil {
		return false
	}
	startedAt := job.StartedAt
	if job.EvaluationCycle > 0 {
		startedAt = job.CycleStartedAt
	}
	return startedAt != nil && job.JobId.String() == identity.JobId && job.RoundId.String() == identity.RoundId &&
		job.Round.Epoch == identity.RoundEpoch && evaluationSourceEpoch(&job.Round) == identity.SourceEpoch && job.ExecutorHostId == identity.HostId &&
		job.EvaluationCycle == identity.Cycle && job.AttemptCount == identity.Attempt && startedAt.Equal(identity.StartedAt) && job.ExecutionDeadline.Equal(identity.DeadlineAt)
}

// An attached observer has no route to bundle materialization or start fallback.
func (self *competitionAgentEvaluator) finishExisting(ctx context.Context, settings *Settings, job *queuedJob, witness *competitionExistingRunWitness) EvaluationOutcome {
	if witness == nil || witness.evaluator != self || !witness.consumed.Load() || !sameCompetitionExistingJob(job, witness.identity) || job.SlotFencingToken != witness.slotFence+1 {
		return EvaluationOutcome{Error: terminalInfrastructureError("executor_identity_invalid", "existing execution lost its exact attachment authority"), distributedIntegrityRejected: true}
	}
	frozen, err := settingsForFrozenRound(settings, &job.Round)
	if err != nil || validateRoundBaselineRecord(frozen, &job.Round, job.RoundBaseline) != nil || validateCompetitionSecurityToolchainScope(frozen, &job.Round, job.RoundBaseline) != nil {
		return EvaluationOutcome{Error: terminalInfrastructureError("round_policy_mismatch", "existing execution differs from its immutable round and shared baseline")}
	}
	return self.observePreparedRun(ctx, ctx, frozen, job, witness.manifest, "", true)
}
