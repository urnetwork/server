// A deliberate operator retry of an unchanged patch is not an infrastructure
// diagnosis. Its evidence, paid job and original failure remain independently pinned.
package model

import "time"

const (
	CompetitionOperatorRequestedPatchRetryAuthorizationKind = "competition-operator-requested-patch-retry-authorization/v1"
	CompetitionOperatorRequestedPatchRetryRuntimeKind       = "competition-operator-requested-patch-retry-runtime-qualification/v1"
	CompetitionOperatorRequestedPatchRetryCapability        = "competition-operator-requested-patch-retry/v1"
	CompetitionJobReevaluationOperatorRequestedPatchRetry   = "operator_requested_patch_retry"
)

// The operator's applicability diagnosis is explicit retained evidence, not a
// subtype inferred from the native evaluator's generic process-exit code.
type CompetitionOperatorRequestedPatchRetry struct {
	Schema                      int    `json:"schema"`
	PatchSha256                 string `json:"patch_sha256"`
	ApplicabilityEvidenceSha256 string `json:"applicability_evidence_sha256"`
}

// This class preserves the original generic non-retriable executor failure.
// It retains the ordinary original run identity and frozen three-hour policy.
func validCompetitionOperatorRequestedPatchRetryCycle(cycle *CompetitionJobReevaluationCycle) bool {
	proof := cycle.Distributed
	if cycle.Schema != 2 || proof == nil || proof.Schema != 1 || proof.Reason != CompetitionJobReevaluationOperatorRequestedPatchRetry || proof.PatchRetry == nil {
		return false
	}
	retry := proof.PatchRetry
	return retry.Schema == 1 && retry.PatchSha256 == cycle.PatchSha256 && competitionAutomationSha256.MatchString(retry.PatchSha256) &&
		competitionAutomationSha256.MatchString(retry.ApplicabilityEvidenceSha256) &&
		competitionAutomationScope.MatchString(proof.OriginalHostId) && proof.OriginalSlotId > 0 && proof.OriginalFencingToken > 0 &&
		competitionAutomationSha256.MatchString(proof.OriginalRunId) && !proof.OriginalStartedAt.IsZero() &&
		proof.OriginalDeadlineAt.Equal(proof.OriginalStartedAt.Add(3*time.Hour)) && !cycle.AuthorizedAt.Before(proof.OriginalStartedAt) &&
		competitionAutomationSha256.MatchString(proof.SourceLockSha256) && validCompetitionReevaluationImage(proof.EvaluatorImageDigest) &&
		competitionAutomationSha256.MatchString(proof.PolicyApprovalSha256) && competitionAutomationSha256.MatchString(proof.ClosureEvidenceSha256) &&
		competitionAutomationSha256.MatchString(proof.NativeProofSha256)
}
