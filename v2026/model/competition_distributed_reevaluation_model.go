// Distributed recovery has separate authority from every historical singleton
// exception. Original admission and failure remain immutable; only one audited
// cycle may receive its own database-allocated three-hour execution clock.
package model

import (
	"time"
)

const (
	CompetitionDistributedJobReevaluationAuthorizationKind = "competition-distributed-job-reevaluation-authorization/v1"
	CompetitionDistributedJobReevaluationRuntimeKind       = "competition-distributed-job-reevaluation-runtime-qualification/v1"
	CompetitionDistributedJobReevaluationCapability        = "competition-distributed-job-reevaluation/v1"
	CompetitionJobReevaluationDistributedInfrastructure    = "operator_authorized_distributed_infrastructure_v1"
	CompetitionDistributedReevaluationRetentionBudget      = "retention_budget_exhausted"
	CompetitionDistributedReevaluationLegacyPrelaunch      = "closed_legacy_prelaunch"
)

// An exact census of every frozen host's empty database slot. A missing host,
// changed fence or substituted slot cannot borrow another host's idle proof.
type CompetitionDistributedReevaluationIdleSlot struct {
	HostId       string `json:"host_id"`
	SlotId       int    `json:"slot_id"`
	FencingToken int64  `json:"fencing_token"`
}

// The operator pins the actual old run and reviewed closure evidence, not a
// singleton absence or a terminal code alone. NativeFencingToken is the exact
// already observed old-run fence; inspection must not advance native custody.
type CompetitionDistributedJobReevaluationAuthorization struct {
	Schema                 int                                          `json:"schema"`
	Reason                 string                                       `json:"reason"`
	OriginalHostId         string                                       `json:"original_host_id"`
	OriginalSlotId         int                                          `json:"original_slot_id"`
	OriginalFencingToken   int64                                        `json:"original_fencing_token"`
	OriginalRunId          string                                       `json:"original_run_id"`
	OriginalManifestSha256 string                                       `json:"original_manifest_sha256"`
	OriginalRequestSha256  string                                       `json:"original_request_sha256"`
	NativeFencingToken     int64                                        `json:"native_fencing_token"`
	OriginalStartedAt      time.Time                                    `json:"original_started_at"`
	OriginalDeadlineAt     time.Time                                    `json:"original_deadline_at"`
	SourceLockSha256       string                                       `json:"source_lock_sha256"`
	EvaluatorImageDigest   string                                       `json:"evaluator_image_digest"`
	PolicyApprovalSha256   string                                       `json:"policy_approval_sha256"`
	ClosureEvidenceSha256  string                                       `json:"closure_evidence_sha256"`
	SupervisorOwnerId      string                                       `json:"supervisor_owner_id"`
	SupervisorFencingToken int64                                        `json:"supervisor_fencing_token"`
	IdleSlots              []CompetitionDistributedReevaluationIdleSlot `json:"idle_slots"`
}

// The authenticated public projection contains digests, never retained private
// transport inventory or original terminal bytes. Those stay in the immutable
// operator audit alongside the signed original-run and all-host observations.
type CompetitionDistributedJobReevaluationCycle struct {
	Schema                int       `json:"schema"`
	Reason                string    `json:"reason"`
	OriginalHostId        string    `json:"original_host_id"`
	OriginalSlotId        int       `json:"original_slot_id"`
	OriginalFencingToken  int64     `json:"original_fencing_token"`
	OriginalRunId         string    `json:"original_run_id"`
	OriginalStartedAt     time.Time `json:"original_started_at"`
	OriginalDeadlineAt    time.Time `json:"original_deadline_at"`
	SourceLockSha256      string    `json:"source_lock_sha256"`
	EvaluatorImageDigest  string    `json:"evaluator_image_digest"`
	PolicyApprovalSha256  string    `json:"policy_approval_sha256"`
	ClosureEvidenceSha256 string    `json:"closure_evidence_sha256"`
	NativeProofSha256     string    `json:"native_proof_sha256"`
}

// Version-one bytes and failure classes retain their old meaning. Version two
// requires its explicit distributed provenance and cannot relabel a scored or
// submitter-caused outcome as an infrastructure recovery.
func validCompetitionReevaluationCycleClass(cycle *CompetitionJobReevaluationCycle) bool {
	if cycle.Schema == 1 {
		return cycle.Distributed == nil && (cycle.IncidentClass == "" || cycle.IncidentClass == CompetitionJobReevaluationHeartbeatBudget ||
			cycle.IncidentClass == CompetitionJobReevaluationBackingService || cycle.IncidentClass == CompetitionJobReevaluationOperatorInterruption)
	}
	proof := cycle.Distributed
	return cycle.Schema == 2 && cycle.IncidentClass == CompetitionJobReevaluationDistributedInfrastructure && proof != nil && proof.Schema == 1 &&
		(proof.Reason == CompetitionDistributedReevaluationRetentionBudget || proof.Reason == CompetitionDistributedReevaluationLegacyPrelaunch) &&
		competitionAutomationScope.MatchString(proof.OriginalHostId) && proof.OriginalSlotId > 0 && proof.OriginalFencingToken > 0 &&
		competitionAutomationSha256.MatchString(proof.OriginalRunId) && !proof.OriginalStartedAt.IsZero() &&
		proof.OriginalDeadlineAt.Equal(proof.OriginalStartedAt.Add(3*time.Hour)) &&
		!cycle.AuthorizedAt.Before(proof.OriginalDeadlineAt) &&
		competitionAutomationSha256.MatchString(proof.SourceLockSha256) && validCompetitionReevaluationImage(proof.EvaluatorImageDigest) &&
		competitionAutomationSha256.MatchString(proof.PolicyApprovalSha256) && competitionAutomationSha256.MatchString(proof.ClosureEvidenceSha256) &&
		competitionAutomationSha256.MatchString(proof.NativeProofSha256)
}
