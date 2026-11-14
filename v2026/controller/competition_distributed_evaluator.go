// Durable agents execute only frozen bundles. The main coordinator independently
// authenticates their request/results and verifies immutable retention before it
// returns an outcome eligible for the database's completion transaction.
package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/competitionagent"
)

// Retention links execution and its current coordinator custody without placing
// a hidden seed, a signing key or an SSH credential in the artifact manifest.
type competitionExecutorArtifactIdentity struct {
	HostId              string `json:"host_id"`
	SlotId              int    `json:"slot_id"`
	SlotFencingToken    int64  `json:"slot_fencing_token"`
	RunId               string `json:"run_id"`
	StartManifestSha256 string `json:"start_manifest_sha256"`
	AgentReceiptSha256  string `json:"agent_receipt_sha256"`
}

// Definitive peer integrity failures withdraw custody without redispatching the
// unchanged execution. The database event keeps this hold across coordinator death.
var errCompetitionExecutorIntegrityRejected = errors.New("executor integrity rejected; original run remains held")

// A selected host has one evaluation owner. Transport failures only reconnect
// the same stable run; they never become a second infrastructure attempt.
type competitionAgentEvaluator struct {
	host      CompetitionExecutorHostPolicy
	transport competitionAgentTransport
	// Nil uses the real clock/poll delay. Tests drive exact acknowledgement,
	// deadline and cancellation edges without wall-clock sleeps.
	now  func() time.Time
	poll <-chan time.Time
	// Qualification retains a successful local result directory for additional
	// per-run metric authentication; ordinary jobs do not need this callback.
	afterRetained func(string, competitionagent.Response, EvaluationOutcome) error
}

// Signed checks retain the existing containment and exact-round rebaseline
// gates. An agent being reachable is not evidence that it is eligible.
func (self *competitionAgentEvaluator) SelfCheck(ctx context.Context, settings *Settings) (HostSelfCheck, error) {
	response, err := self.transport.Exchange(ctx, competitionagent.Request{Operation: "check"}, "", "")
	if err != nil {
		return HostSelfCheck{}, err
	}
	check, err := decodeHostSelfCheck(response.SelfCheck)
	if err != nil || response.State != "ready" || !check.Eligible(settings) || validateCompetitionPrivateHostProbe(check) != nil {
		return check, errors.New("authenticated executor did not pass containment and exact-round readiness")
	}
	return check, nil
}

// Private readiness retains a qualified native-run receipt; it does not
// silently relabel that historical map evidence as a new unowned live probe.
func validateCompetitionPrivateHostProbe(check HostSelfCheck) error {
	if check.NamespaceProbeSource != "retained_native_containment" || check.NamespaceProbeFresh || !sha256Pattern.MatchString(check.PrivateRuntimeBoundarySha256) {
		return errors.New("private executor did not retain an explicit native qualification boundary")
	}
	return nil
}

// The deadline starts in the first PostgreSQL claim and is persisted unchanged.
// Detaching SSH leaves systemd in charge; parent cancellation relinquishes only
// coordinator custody so a fenced successor can reconcile the existing run.
func (self *competitionAgentEvaluator) Evaluate(parent context.Context, settings *Settings, job *queuedJob) EvaluationOutcome {
	failure := func(code, message string) EvaluationOutcome {
		return EvaluationOutcome{Error: terminalInfrastructureError(code, message)}
	}
	if !competitionDistributedEnabled(settings) || job == nil || job.ExecutionDeadline == nil || job.StartedAt == nil || job.SlotFencingToken <= 0 || job.ExecutorHostId != self.host.HostId || job.RoundBaseline == nil {
		return failure("executor_identity_invalid", "distributed evaluation is missing its frozen claim authority")
	}
	frozen, err := settingsForFrozenRound(settings, &job.Round)
	if err != nil || validateRoundBaselineRecord(frozen, &job.Round, job.RoundBaseline) != nil {
		return failure("round_policy_mismatch", "distributed evaluation does not match its immutable round and shared baseline")
	}
	settings = frozen
	if err := validateCompetitionSecurityToolchainScope(settings, &job.Round, job.RoundBaseline); err != nil {
		return failure("round_policy_mismatch", "security toolchain exception does not match the retained epoch control")
	}
	ctx, cancel := context.WithDeadline(parent, *job.ExecutionDeadline)
	defer cancel()
	manifest, inputDirectory, err := prepareCompetitionAgentBundle(ctx, settings, job, self.host)
	if err != nil {
		return failure("executor_bundle_unavailable", "authenticated frozen evaluation inputs are unavailable")
	}
	defer os.RemoveAll(inputDirectory)
	manifestDigest, err := competitionagent.ManifestDigest(manifest)
	if err != nil {
		return failure("executor_bundle_invalid", "frozen execution bundle could not be authenticated")
	}
	request := competitionagent.Request{Operation: "start", RunId: manifest.Identity.RunId, FencingToken: job.SlotFencingToken, Start: &manifest}
	for {
		if ctx.Err() != nil {
			now := server.NowUtc()
			if self.now != nil {
				now = self.now()
			}
			if now.Before(*job.ExecutionDeadline) && parent.Err() != nil {
				return failure("executor_custody_relinquished", "coordinator relinquished custody of the original remote execution")
			}
			self.cancelExpiredRun(settings, job, manifest.Identity.RunId)
			return failure("evaluation_time_budget_exhausted", "submission exhausted its total evaluation time budget")
		}
		response, exchangeErr := self.transport.Exchange(ctx, request, inputDirectory, "")
		if errors.Is(exchangeErr, competitionagent.ErrIntegrity) {
			return EvaluationOutcome{Error: terminalInfrastructureError("executor_integrity_rejected", "authenticated executor transport rejected altered or invalid evidence"), distributedIntegrityRejected: true}
		}
		if exchangeErr == nil && response.ManifestSha256 == manifestDigest && sameCompetitionAgentIdentity(response.Identity, &manifest.Identity) {
			competitionExecutorRemoteState.WithLabelValues(self.host.HostId).Set(competitionAgentStateNumber(response.State))
			switch response.State {
			case "succeeded", "failed", "canceled", "expired":
				directory, custody, createErr := createCompetitionReceive(settings.ArtifactRoot, job, manifest.Identity, manifest.Inputs)
				if createErr == nil {
					outcome, completed := func() (returned EvaluationOutcome, completed bool) {
						defer func() {
							if custody != nil && returned.receiveCustody != custody {
								if err := custody.Close(); err != nil {
									competitionReceiveCleanupFailures.WithLabelValues("close").Inc()
								}
							}
						}()
						artifactResponse, artifactErr := self.transport.Exchange(ctx, competitionagent.Request{Operation: "artifacts", RunId: request.RunId, FencingToken: request.FencingToken}, "", directory)
						if errors.Is(artifactErr, competitionagent.ErrIntegrity) {
							_ = os.RemoveAll(directory)
							return EvaluationOutcome{Error: terminalInfrastructureError("executor_integrity_rejected", "authenticated executor artifact transfer rejected altered or invalid evidence"), distributedIntegrityRejected: true}, true
						}
						if artifactErr == nil && artifactResponse.ManifestSha256 == manifestDigest && sameCompetitionAgentIdentity(artifactResponse.Identity, &manifest.Identity) && artifactResponse.State == response.State {
							outcome := authenticateDistributedCompetitionAttempt(ctx, settings, job, manifest, artifactResponse, directory)
							if outcome.distributedArchiveVerified {
								outcome, err := self.retainedReceiveOutcome(directory, custody, artifactResponse, outcome)
								if err != nil {
									return failure("executor_qualification_failed", "retained no-op evidence did not pass the approved admission screen"), true
								}
								return outcome, true
							}
						}
						return EvaluationOutcome{}, false
					}()
					if completed {
						return outcome
					}
					// An incomplete copy/retention is not completion authority.
					// Retry the same terminal execution inside its original budget.
					_ = os.Chmod(directory, 0700)
					_ = os.RemoveAll(directory)
				}
			}
		}
		if exchangeErr != nil {
			competitionExecutorTransportFailures.WithLabelValues(self.host.HostId).Inc()
		}
		// Repeating start is an idempotent reconciliation: the agent persists
		// launch_attempted before systemd launch, including a lost start reply.
		request.Operation, request.Start = "status", nil
		if exchangeErr == nil && (response.State == "not_found" || response.State == "unknown" && response.ErrorCode == "run_not_found" && response.Identity == nil) {
			request.Operation, request.Start = "start", &manifest
		}
		next := self.poll
		if next == nil {
			next = time.After(settings.distributedCoordinator.Config.Settings.PollInterval)
		}
		select {
		case <-ctx.Done():
		case <-next:
		}
	}
}

// Cancellation never gives an execution a fresh budget. Cleanup is bounded
// separately, and no score may publish after the original absolute deadline.
func (self *competitionAgentEvaluator) cancelExpiredRun(settings *Settings, job *queuedJob, runId string) {
	ctx, cancel := context.WithTimeout(context.Background(), settings.distributedCoordinator.Config.Settings.CancelTimeout)
	defer cancel()
	_, _ = self.transport.Exchange(ctx, competitionagent.Request{Operation: "cancel", RunId: runId, FencingToken: job.SlotFencingToken}, "", "")
}

// Input materialization contains only local-fixture data and the immutable
// workload/patch/shared baseline. The seed exists solely in the signed request,
// which crosses encrypted transport and is never included in MinIO artifacts.
func prepareCompetitionAgentBundle(ctx context.Context, settings *Settings, job *queuedJob, host CompetitionExecutorHostPolicy) (manifest competitionagent.StartManifest, directory string, resultErr error) {
	manifest, err := competitionAgentManifestForJob(settings, job, host)
	if err != nil {
		return manifest, "", err
	}
	providers, err := readRoundWorkload(ctx, settings, &job.Round)
	if err != nil {
		return manifest, "", err
	}
	defer clear(providers)
	seed, err := revealRoundSecret(settings, &job.Round)
	if err != nil {
		return manifest, "", err
	}
	directory, err = os.MkdirTemp(settings.ArtifactRoot, ".executor-dispatch-")
	if err != nil {
		return manifest, "", err
	}
	defer func() {
		if resultErr != nil {
			_ = os.RemoveAll(directory)
		}
	}()
	for _, input := range []struct {
		name    string
		content []byte
	}{
		{name: "canonical.patch", content: job.Patch}, {name: "providers.yml", content: providers}, {name: "round-baseline.json", content: job.RoundBaseline.Json},
	} {
		if err := writeExclusiveFile(filepath.Join(directory, input.name), input.content, 0400); err != nil {
			return manifest, directory, err
		}
		digest := sha256.Sum256(input.content)
		manifest.Inputs = append(manifest.Inputs, competitionagent.File{Path: input.name, Sha256: hex.EncodeToString(digest[:]), Bytes: int64(len(input.content))})
	}
	request := evaluatorRequestForJob(settings, job, seed, ".", "canonical.patch", "providers.yml")
	request.RoundBaselinePath, request.ConfigLocalDirectory, request.VaultLocalDirectory = "round-baseline.json", "@config-local", "@vault-local"
	request.EvaluationPolicy.HostQualificationSha256 = manifest.QualificationSha256
	request.EvaluatorImageDigest = manifest.EvaluatorImageDigest
	if value := competitionExecutionSecurityToolchain(settings); value != nil {
		request.EvaluationPolicy.SimulatorSha256 = value.InstrumentSha256
		request.EvaluationPolicy.ScorerSha256 = value.InstrumentSha256
	}
	manifest.EvaluatorRequest, err = json.Marshal(request)
	request.RoundSeedHex = ""
	return manifest, directory, err
}

// UTC JSON round trips may change time locations, never the instants themselves.
func sameCompetitionAgentIdentity(left, right *competitionagent.Identity) bool {
	if left == nil || right == nil {
		return false
	}
	copy := *left
	copy.StartedAt, copy.DeadlineAt = right.StartedAt, right.DeadlineAt
	return copy == *right && left.StartedAt.Equal(right.StartedAt) && left.DeadlineAt.Equal(right.DeadlineAt)
}

// Stable, bounded numeric states avoid candidate/run identifiers in monitoring.
func competitionAgentStateNumber(state string) float64 {
	switch state {
	case "accepted":
		return 1
	case "running":
		return 2
	case "succeeded":
		return 3
	case "failed", "expired", "canceled":
		return 4
	default:
		return 0
	}
}

// Agent signatures prove origin, not scoring correctness. This second boundary
// rechecks exact request bytes, shared baseline, result schema, policy, security,
// every declared artifact and retained object before granting completion proof.
func authenticateDistributedCompetitionAttempt(ctx context.Context, settings *Settings, job *queuedJob, start competitionagent.StartManifest, response competitionagent.Response, directory string) EvaluationOutcome {
	failure := func(code, message string) EvaluationOutcome {
		return archiveDistributedCompetitionFailure(ctx, settings, job, start, response, directory, terminalInfrastructureError(code, message))
	}
	requestBytes, err := readRegularFile(filepath.Join(directory, "worker-request.json"), settings.distributedCoordinator.Config.Settings.Limits.DocumentBytes)
	if err != nil {
		return EvaluationOutcome{Error: terminalInfrastructureError("executor_request_missing", "executor omitted its authenticated request")}
	}
	defer clear(requestBytes)
	digest := sha256.Sum256(requestBytes)
	if hex.EncodeToString(digest[:]) != response.RequestSha256 {
		return EvaluationOutcome{Error: terminalInfrastructureError("executor_request_mismatch", "executor request digest did not match its signed receipt")}
	}
	request := evaluatorRequest{}
	if err := decodeFailureEvidenceJson(requestBytes, &request); err != nil || !filepath.IsAbs(request.ArtifactDirectory) ||
		request.PatchPath != filepath.Join(request.ArtifactDirectory, "canonical.patch") || request.ProvidersPath != filepath.Join(request.ArtifactDirectory, "providers.yml") ||
		request.RoundBaselinePath != filepath.Join(request.ArtifactDirectory, "round-baseline.json") || !filepath.IsAbs(request.ConfigLocalDirectory) || !filepath.IsAbs(request.VaultLocalDirectory) ||
		!strings.HasSuffix(request.ConfigLocalDirectory, "/config/local") || !strings.HasSuffix(request.VaultLocalDirectory, "/vault/local") {
		return failure("executor_request_invalid", "executor changed the frozen request's path boundary")
	}
	request.ArtifactDirectory, request.PatchPath, request.ProvidersPath = ".", "canonical.patch", "providers.yml"
	request.RoundBaselinePath, request.ConfigLocalDirectory, request.VaultLocalDirectory = "round-baseline.json", "@config-local", "@vault-local"
	normalized, err := json.Marshal(request)
	request.RoundSeedHex = ""
	if err != nil || !bytes.Equal(normalized, start.EvaluatorRequest) {
		clear(normalized)
		return failure("executor_request_invalid", "executor changed the frozen seed, source, policy or inputs")
	}
	clear(normalized)
	if response.State != "succeeded" || response.ExitCode != 0 {
		return failure("executor_process_failed", "executor terminated without a complete trusted evaluation")
	}
	resultBytes, err := readRegularFile(filepath.Join(directory, "worker-result.json"), maxEvaluatorResultBytes)
	if err != nil {
		return failure("evaluator_result_missing", "executor did not return a complete result")
	}
	result := evaluatorResult{}
	if decodeCompetitionEvidenceJson(resultBytes, &result, func(path []string) bool {
		return len(path) >= 2 && path[0] == "score" && (path[1] == "gates" || path[1] == "diagnostics")
	}) != nil || result.Schema != 1 || result.JobId != job.JobId.String() || (result.Score == nil) == (result.EvalError == nil) {
		return failure("evaluator_result_invalid", "executor result identity or schema is invalid")
	}
	if result.Score != nil && (validateScore(result.Score) != nil || !scoreMatchesFrozenMargin(result.Score, settings.EvaluationPolicy.TakeoverMargin) || !scoreMatchesFrozenG2Policy(result.Score, settings.EvaluationPolicy.G2VolumePolicy, settings.EvaluationPolicy.WorkCoveragePolicy)) ||
		result.EvalError != nil && validateEvaluationError(result.EvalError) != nil || !result.Security.passedFor(result.EvalError) {
		return failure("containment_gate_failed", "executor result did not prove every frozen scoring and containment gate")
	}
	artifacts, err := authenticateResultArtifacts(directory, result.Artifacts, result.EvalError)
	if err != nil {
		return failure("artifact_authentication_failed", "executor result artifacts failed authentication")
	}
	baseline, err := readRegularFile(filepath.Join(directory, "baseline.json"), maxRoundBaselineBytes)
	if err != nil || !bytes.Equal(baseline, job.RoundBaseline.Json) {
		return failure("round_baseline_changed", "executor changed the immutable shared baseline")
	}
	baselineDigest, err := validateRoundBaselineJson(settings, &job.Round, baseline)
	if err != nil || baselineDigest != job.RoundBaseline.Sha256 || result.Score != nil && result.Score.Diagnostics["baseline_sha256"] != baselineDigest {
		return failure("round_baseline_invalid", "executor score is not bound to the immutable shared baseline")
	}
	artifacts, err = distributedCompetitionEvidence(directory, job, artifacts, response.Files, settings.distributedCoordinator.Config.Settings.Limits.DocumentBytes)
	if err != nil {
		return failure("artifact_authentication_failed", "executor raw evidence failed authentication")
	}
	if err := authenticateDistributedCompetitionSources(settings, job, directory, artifacts, result.Score != nil); err != nil {
		return failure("executor_source_mismatch", "executor source evidence did not match the exact frozen ten-repository graph")
	}
	receipt, executor, err := materializeCompetitionExecutorReceipt(directory, job, start, response)
	if err != nil {
		return failure("artifact_manifest_failed", "executor receipt could not be materialized")
	}
	artifacts = append(artifacts, receipt)
	resultDigest := sha256.Sum256(resultBytes)
	patchHash, _, patchErr := hashRegularFile(filepath.Join(directory, "canonical.patch"))
	stderrHash, _, stderrErr := hashRegularFile(filepath.Join(directory, "worker.stderr.log"))
	if patchErr != nil || stderrErr != nil || patchHash != job.PatchSha256 {
		return failure("artifact_authentication_failed", "executor changed the canonical patch or omitted stderr")
	}
	manifest := artifactManifest{Schema: 1, JobId: job.JobId.String(), RoundId: job.RoundId.String(), SourceEpoch: evaluationSourceEpoch(&job.Round), Attempt: job.AttemptCount,
		SecurityToolchainException: competitionExecutionSecurityToolchain(settings), ExecutionEvaluatorImageDigest: competitionSecurityToolchainExecutionDigest(settings),
		EvaluatorImageDigest: job.EvaluatorImageDigest, ApiImageDigest: job.ApiImageDigest, WorkerImageDigest: job.WorkerImageDigest, EvaluatorCommandSha256: start.EvaluatorCommandSha256,
		RequestSha256: response.RequestSha256, PatchSha256: patchHash, StderrSha256: stderrHash, ResultSha256: hex.EncodeToString(resultDigest[:]), Security: result.Security, Artifacts: artifacts, Executor: executor}
	if err := sealArtifactDirectory(directory); err != nil || settings.artifactArchive == nil {
		return EvaluationOutcome{Error: terminalInfrastructureError("artifact_archive_unavailable", "authenticated executor evidence could not be sealed and retained")}
	}
	archived, err := settings.artifactArchive.ArchiveAttempt(ctx, settings, job, directory, manifest)
	if err != nil {
		return EvaluationOutcome{Error: terminalInfrastructureError("artifact_archive_failed", "authenticated executor evidence failed immutable retention"), archiveFailure: competitionArtifactArchiveFailureDetails("archive_attempt", err)}
	}
	return EvaluationOutcome{Score: result.Score, Error: result.EvalError, ArtifactManifest: archived, RoundBaselineJson: baseline, RoundBaselineSha256: baselineDigest, distributedArchiveVerified: true}
}

// Retains full safe regular-file raw evidence, not just the seven summary files.
// The seed-bearing worker request, config/vault and environment files are never
// included in this independently authenticated retention list.
func distributedCompetitionEvidence(directory string, job *queuedJob, artifacts []evaluationArtifact, files []competitionagent.File, maximum int64) ([]evaluationArtifact, error) {
	content, err := readRegularFile(filepath.Join(directory, "evidence-manifest.json"), maximum)
	if err != nil {
		return nil, err
	}
	evidence := struct {
		Schema    int                  `json:"schema"`
		Kind      string               `json:"kind"`
		JobId     string               `json:"job_id"`
		RoundId   string               `json:"round_id"`
		Artifacts []evaluationArtifact `json:"artifacts"`
	}{}
	if decodeFailureEvidenceJson(content, &evidence) != nil || evidence.Schema != 1 || evidence.Kind != "sim-latency-evidence-manifest" || evidence.JobId != job.JobId.String() || evidence.RoundId != job.RoundId.String() {
		return nil, errors.New("invalid raw evidence manifest")
	}
	declared := map[string]competitionagent.File{}
	for _, file := range files {
		declared[file.Path] = file
	}
	seen := map[string]bool{}
	for _, artifact := range artifacts {
		seen[artifact.Path] = true
	}
	inventoryDigest := sha256.Sum256(content)
	inventory := evaluationArtifact{Path: "evidence-manifest.json", Sha256: hex.EncodeToString(inventoryDigest[:]), Bytes: int64(len(content))}
	signedInventory, present := declared[inventory.Path]
	if !present || signedInventory.Sha256 != inventory.Sha256 || signedInventory.Bytes != inventory.Bytes {
		return nil, errors.New("raw evidence inventory is not bound to the signed executor receipt")
	}
	if !seen[inventory.Path] {
		artifacts = append(artifacts, inventory)
		seen[inventory.Path] = true
	}
	for _, artifact := range evidence.Artifacts {
		file, present := declared[artifact.Path]
		if !present || artifact.Sha256 != file.Sha256 || artifact.Bytes != file.Bytes || seen[artifact.Path] || !strings.HasPrefix(artifact.Path, "evidence/") {
			return nil, errors.New("raw evidence is not bound to the signed executor receipt")
		}
		seen[artifact.Path] = true
		allowed := true
		for _, component := range strings.Split(artifact.Path, "/") {
			if component == "config" || component == "vault" || component == "secrets" || component == "worker-request.json" || strings.HasSuffix(component, ".env") || strings.Contains(component, ".env.") {
				allowed = false
			}
		}
		if allowed {
			artifacts = append(artifacts, artifact)
		}
	}
	return artifacts, nil
}

// A signed receipt is safe to retain: no evaluator request, seed or input bytes.
func materializeCompetitionExecutorReceipt(directory string, job *queuedJob, start competitionagent.StartManifest, response competitionagent.Response) (evaluationArtifact, *competitionExecutorArtifactIdentity, error) {
	content, err := json.Marshal(response)
	if err != nil {
		return evaluationArtifact{}, nil, err
	}
	path := filepath.Join(directory, "executor-receipt.json")
	if err := writeExclusiveFile(path, content, 0400); err != nil {
		return evaluationArtifact{}, nil, err
	}
	digest := sha256.Sum256(content)
	manifestDigest, err := competitionagent.ManifestDigest(start)
	if err != nil {
		return evaluationArtifact{}, nil, err
	}
	identity := &competitionExecutorArtifactIdentity{HostId: job.ExecutorHostId, SlotId: job.SlotId, SlotFencingToken: job.SlotFencingToken, RunId: start.Identity.RunId,
		StartManifestSha256: manifestDigest, AgentReceiptSha256: hex.EncodeToString(digest[:])}
	return evaluationArtifact{Path: "executor-receipt.json", Sha256: identity.AgentReceiptSha256, Bytes: int64(len(content))}, identity, nil
}

// Definite signed failures retain their receipt and safe diagnostics, never a
// result/score that failed authentication. Archive failures remain uncompleted
// and reconcile the same execution until the original deadline expires.
func archiveDistributedCompetitionFailure(ctx context.Context, settings *Settings, job *queuedJob, start competitionagent.StartManifest, response competitionagent.Response, directory string, failure *CompetitionError) EvaluationOutcome {
	if settings.artifactArchive == nil {
		return EvaluationOutcome{Error: failure}
	}
	receipt, executor, err := materializeCompetitionExecutorReceipt(directory, job, start, response)
	if err != nil {
		return EvaluationOutcome{Error: failure}
	}
	patchHash, _, patchErr := hashRegularFile(filepath.Join(directory, "canonical.patch"))
	stderrHash, _, stderrErr := hashRegularFile(filepath.Join(directory, "worker.stderr.log"))
	if patchErr != nil || stderrErr != nil || patchHash != job.PatchSha256 {
		return EvaluationOutcome{Error: failure}
	}
	manifest := artifactManifest{Schema: 1, JobId: job.JobId.String(), RoundId: job.RoundId.String(), SourceEpoch: evaluationSourceEpoch(&job.Round), Attempt: job.AttemptCount,
		SecurityToolchainException: competitionExecutionSecurityToolchain(settings), ExecutionEvaluatorImageDigest: competitionSecurityToolchainExecutionDigest(settings),
		EvaluatorImageDigest: job.EvaluatorImageDigest, ApiImageDigest: job.ApiImageDigest, WorkerImageDigest: job.WorkerImageDigest, EvaluatorCommandSha256: start.EvaluatorCommandSha256,
		RequestSha256: response.RequestSha256, PatchSha256: patchHash, StderrSha256: stderrHash, Executor: executor, Failure: failure, Artifacts: []evaluationArtifact{receipt}}
	archived, err := settings.artifactArchive.ArchiveAttempt(ctx, settings, job, directory, manifest)
	if err != nil {
		return EvaluationOutcome{Error: failure, archiveFailure: competitionArtifactArchiveFailureDetails("archive_attempt", err)}
	}
	return EvaluationOutcome{Error: failure, ArtifactManifest: archived, distributedArchiveVerified: true}
}

// Keeps synthetic qualification jobs out of public admissions while deriving a
// stable identity from the exact frozen round, host and policy approval.
func competitionExecutorQualificationJobId(roundId server.Id, hostId, approval string) server.Id {
	digest := sha256.Sum256([]byte(fmt.Sprintf("competition-executor-noop/v1\x00%s\x00%s\x00%s", roundId, hostId, approval)))
	var id server.Id
	copy(id[:], digest[:len(id)])
	return id
}
