// An operator-authorized recovery is a new bounded execution cycle of the same
// paid job, not a new submission or permission to resample a scored outcome.
package model

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/urnetwork/server"
)

const (
	CompetitionJobReevaluationAuthorizationKind    = "competition-job-reevaluation-authorization/v1"
	CompetitionJobReevaluationRuntimeKind          = "competition-job-reevaluation-runtime-qualification/v1"
	CompetitionJobReevaluationCapability           = "competition-job-reevaluation/v1"
	CompetitionJobReevaluationArchiveInventory     = "archive_inventory_metadata_v1"
	CompetitionJobReevaluationLegacyArchiveBudget  = "operator_authorized_legacy_archive_budget_v1"
	CompetitionJobReevaluationHeartbeatBudget      = "operator_authorized_heartbeat_budget_v1"
	CompetitionJobReevaluationBackingService       = "operator_authorized_backing_service_v1"
	CompetitionJobReevaluationOperatorInterruption = "operator_authorized_interrupted_budget_v1"
)

// The private pinned document binds the original terminal row, actual incident
// event and separately qualified compatible release. It grants one cycle only.
type CompetitionJobReevaluationAuthorization struct {
	Schema                       int                                                     `json:"schema"`
	Kind                         string                                                  `json:"kind"`
	IncidentClass                string                                                  `json:"incident_class"`
	Environment                  string                                                  `json:"environment"`
	CompetitionId                string                                                  `json:"competition_id"`
	SeasonId                     string                                                  `json:"season_id"`
	JobId                        server.Id                                               `json:"job_id"`
	RoundId                      server.Id                                               `json:"round_id"`
	ExpectedCycle                int                                                     `json:"expected_cycle"`
	ExpectedAttemptCount         int                                                     `json:"expected_attempt_count"`
	ExpectedTerminalSnapshot     json.RawMessage                                         `json:"expected_terminal_snapshot"`
	ExpectedTerminalSha256       string                                                  `json:"expected_terminal_sha256"`
	EvidenceSha256               string                                                  `json:"evidence_sha256"`
	IncidentEventId              int64                                                   `json:"incident_event_id"`
	IncidentEventSha256          string                                                  `json:"incident_event_sha256"`
	IncidentAttempt              int                                                     `json:"incident_attempt"`
	FailureManifestSha256        string                                                  `json:"failure_manifest_sha256"`
	RejectedArtifactPath         string                                                  `json:"rejected_artifact_path"`
	RoundPolicySha256            string                                                  `json:"round_policy_sha256"`
	BaselineSha256               string                                                  `json:"baseline_sha256"`
	RuntimeQualificationSha256   string                                                  `json:"runtime_qualification_sha256"`
	FixedSourceCommit            string                                                  `json:"fixed_source_commit"`
	FixedWorkerSha256            string                                                  `json:"fixed_worker_sha256"`
	FixedWorkerImageDigest       string                                                  `json:"fixed_worker_image_digest"`
	ApiImageDigest               string                                                  `json:"api_image_digest"`
	AuthorizedAt                 time.Time                                               `json:"authorized_at"`
	ExpiresAt                    time.Time                                               `json:"expires_at"`
	SingleUse                    bool                                                    `json:"single_use"`
	PostCloseApproval            *CompetitionJobReevaluationPostCloseApproval            `json:"post_close_approval,omitempty"`
	BackingServiceEvidence       *CompetitionJobReevaluationBackingServiceEvidence       `json:"backing_service_evidence,omitempty"`
	OperatorInterruptionEvidence *CompetitionJobReevaluationOperatorInterruptionEvidence `json:"operator_interruption_evidence,omitempty"`
	Distributed                  *CompetitionDistributedJobReevaluationAuthorization     `json:"distributed,omitempty"`
}

// The scoped operator binds retained stop evidence to an actual interrupted
// claim. This attestation is not a manufactured evaluator or heartbeat event.
type CompetitionJobReevaluationOperatorInterruptionEvidence struct {
	Schema           int       `json:"schema"`
	Action           string    `json:"action"`
	StoppedAt        time.Time `json:"stopped_at"`
	ClaimEventId     int64     `json:"claim_event_id"`
	ClaimEventSha256 string    `json:"claim_event_sha256"`
	EvidenceSha256   string    `json:"evidence_sha256"`
}

// Retained bytes support a separate operator diagnosis. Neither this record
// nor a candidate-produced log can independently grant recovery authority.
type CompetitionJobReevaluationBackingServiceEvidence struct {
	Schema         int    `json:"schema"`
	Service        string `json:"service"`
	ArtifactPath   string `json:"artifact_path"`
	ArtifactSha256 string `json:"artifact_sha256"`
	ArtifactBytes  int64  `json:"artifact_bytes"`
}

// Explicit close-bound authority does not reopen intake or permit changing a
// sealed result. Omission retains the original open-round recovery contract.
type CompetitionJobReevaluationPostCloseApproval struct {
	Schema         int       `json:"schema"`
	ClosesAt       time.Time `json:"closes_at"`
	ExpiresAt      time.Time `json:"expires_at"`
	EvidenceSha256 string    `json:"evidence_sha256"`
}

// Qualification is a separately pinned operator receipt, not a model or
// candidate assertion. Its retained evidence binds both compatible consumers.
type CompetitionJobReevaluationRuntime struct {
	Schema     int    `json:"schema"`
	Kind       string `json:"kind"`
	Accepted   bool   `json:"accepted"`
	Capability string `json:"capability"`
	// Omission qualifies historical archive cycles only, never new incident kinds.
	IncidentClasses   []string `json:"incident_classes,omitempty"`
	SourceCommit      string   `json:"source_commit"`
	ApiImageDigest    string   `json:"api_image_digest"`
	WorkerImageDigest string   `json:"worker_image_digest"`
	WorkerSha256      string   `json:"worker_sha256"`
	EvidenceSha256    string   `json:"evidence_sha256"`
	ApiCompatible     bool     `json:"api_compatible"`
	WorkerCompatible  bool     `json:"worker_compatible"`
}

// An idempotency key binds one exact authorization; another key cannot grant
// another cycle, even after that authorized cycle itself terminates.
type CompetitionJobReevaluationRequest struct {
	IdempotencyKey      string                                  `json:"idempotency_key"`
	AuthorizationSha256 string                                  `json:"authorization_sha256"`
	Authorization       CompetitionJobReevaluationAuthorization `json:"authorization"`
}

// This message-free projection is returned by the authenticated job endpoint.
// ReceiptSha256 authenticates immutable field consistency, not a signature:
// authorization provenance remains the trusted API and its immutable audit.
type CompetitionJobReevaluationCycle struct {
	Schema                     int                                         `json:"schema"`
	Environment                string                                      `json:"environment"`
	CompetitionId              string                                      `json:"competition_id"`
	SeasonId                   string                                      `json:"season_id"`
	JobId                      server.Id                                   `json:"job_id"`
	RoundId                    server.Id                                   `json:"round_id"`
	PatchSha256                string                                      `json:"patch_sha256"`
	RoundPolicySha256          string                                      `json:"round_policy_sha256"`
	BaselineSha256             string                                      `json:"baseline_sha256"`
	Cycle                      int                                         `json:"cycle"`
	ReceiptSha256              string                                      `json:"receipt_sha256"`
	AuthorizationSha256        string                                      `json:"authorization_sha256"`
	EvidenceSha256             string                                      `json:"evidence_sha256"`
	PreviousTerminalSha256     string                                      `json:"previous_terminal_sha256"`
	PreviousFailure            CompetitionEvaluationFailure                `json:"previous_failure"`
	IncidentClass              string                                      `json:"incident_class,omitempty"`
	RuntimeQualificationSha256 string                                      `json:"runtime_qualification_sha256"`
	FixedSourceCommit          string                                      `json:"fixed_source_commit"`
	FixedWorkerSha256          string                                      `json:"fixed_worker_sha256"`
	FixedWorkerImageDigest     string                                      `json:"fixed_worker_image_digest"`
	ApiImageDigest             string                                      `json:"api_image_digest"`
	AuthorizedAt               time.Time                                   `json:"authorized_at"`
	StartedAt                  *time.Time                                  `json:"started_at,omitempty"`
	ExecutionTimeoutSeconds    int                                         `json:"execution_timeout_seconds"`
	Distributed                *CompetitionDistributedJobReevaluationCycle `json:"distributed,omitempty"`
}

// The receipt is permanent and retry-stable. StartedAt belongs to the eventual
// first claim. DistributedProof is private audit data, not public feedback or
// CLI output; public consumers receive only CompetitionJobReevaluationCycle.
type CompetitionJobReevaluationReceipt struct {
	CompetitionJobReevaluationCycle
	RequestSha256    string          `json:"request_sha256"`
	AttemptOffset    int             `json:"attempt_offset"`
	DistributedProof json.RawMessage `json:"distributed_proof,omitempty"`
}

// Excluding only first-claim time makes the public receipt independently
// checkable without disclosing the private terminal snapshot or incident log.
func CompetitionJobReevaluationCycleDigest(cycle CompetitionJobReevaluationCycle) (string, error) {
	cycle.ReceiptSha256, cycle.StartedAt = "", nil
	return CompetitionAutomationDigest(cycle)
}

// A recovery marker cannot authorize another job, scored result, canceled
// result or unknown infrastructure cause. The API authenticates its origin.
func ValidateCompetitionJobReevaluationCycle(cycle *CompetitionJobReevaluationCycle) error {
	if cycle == nil || (cycle.Schema != 1 && cycle.Schema != 2) || cycle.Cycle != 1 || cycle.JobId == (server.Id{}) || cycle.RoundId == (server.Id{}) ||
		!competitionAutomationScope.MatchString(cycle.Environment) || !competitionAutomationScope.MatchString(cycle.CompetitionId) || !competitionAutomationScope.MatchString(cycle.SeasonId) ||
		!competitionAutomationSha256.MatchString(cycle.PatchSha256) || !competitionAutomationSha256.MatchString(cycle.RoundPolicySha256) ||
		!competitionAutomationSha256.MatchString(cycle.BaselineSha256) || !competitionAutomationSha256.MatchString(cycle.AuthorizationSha256) ||
		!competitionAutomationSha256.MatchString(cycle.EvidenceSha256) || !competitionAutomationSha256.MatchString(cycle.PreviousTerminalSha256) ||
		!competitionAutomationSha256.MatchString(cycle.RuntimeQualificationSha256) || !competitionSourceCommitPattern.MatchString(cycle.FixedSourceCommit) ||
		!competitionAutomationSha256.MatchString(cycle.FixedWorkerSha256) || !validCompetitionReevaluationImage(cycle.FixedWorkerImageDigest) || !validCompetitionReevaluationImage(cycle.ApiImageDigest) ||
		cycle.AuthorizedAt.IsZero() || cycle.ExecutionTimeoutSeconds != 3*60*60 ||
		!validCompetitionReevaluationCycleClass(cycle) ||
		!CompetitionJobReevaluationFailureAllowed(cycle.IncidentClass, cycle.PreviousFailure) ||
		cycle.StartedAt != nil && (cycle.StartedAt.IsZero() || cycle.StartedAt.Before(cycle.AuthorizedAt)) {
		return errors.New("reevaluation cycle identity or bounded authority is invalid")
	}
	digest, err := CompetitionJobReevaluationCycleDigest(*cycle)
	if err != nil || cycle.ReceiptSha256 != digest {
		return errors.New("reevaluation cycle receipt digest does not match")
	}
	return nil
}

// A typed operator exception preserves the old public failure instead of
// relabelling it. Unknown classes and genuine scored successes remain closed.
func CompetitionJobReevaluationFailureAllowed(incidentClass string, failure CompetitionEvaluationFailure) bool {
	if failure.Retriable {
		return false
	}
	switch incidentClass {
	case "", CompetitionJobReevaluationArchiveInventory:
		return failure.Kind == "infrastructure" && (failure.Code == "artifact_archive_failed" || failure.Code == "evaluation_time_budget_exhausted")
	case CompetitionJobReevaluationLegacyArchiveBudget, CompetitionJobReevaluationHeartbeatBudget, CompetitionJobReevaluationOperatorInterruption:
		return failure.Kind == "infrastructure" && failure.Code == "evaluation_time_budget_exhausted"
	case CompetitionJobReevaluationBackingService:
		return failure.Kind == "submission" && failure.Code == "run_unstable"
	case CompetitionJobReevaluationDistributedInfrastructure:
		return failure.Kind == "infrastructure" && failure.Code == "evaluation_time_budget_exhausted"
	case CompetitionJobReevaluationOperatorRequestedPatchRetry:
		return failure.Kind == "infrastructure" && failure.Code == "executor_process_failed"
	default:
		return false
	}
}

// Image identities are immutable local OCI digests, never moving tags.
func validCompetitionReevaluationImage(value string) bool {
	return len(value) == len("sha256:")+64 && value[:len("sha256:")] == "sha256:" && competitionAutomationSha256.MatchString(value[len("sha256:"):])
}
