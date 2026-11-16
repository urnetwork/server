// A separately approved second recovery binds the immutable first receipt;
// it never changes the default one-cycle authorization contract.
package model

import "github.com/urnetwork/server"

// Authorization omits PreviousRecovery; only the audited store supplies it in
// a public receipt. A single predecessor keeps the history finite and exact.
type CompetitionOperatorSecondRecovery struct {
	Schema                 int                              `json:"schema"`
	JobId                  server.Id                        `json:"job_id"`
	PreviousReceiptSha256  string                           `json:"previous_receipt_sha256"`
	ApprovalEvidenceSha256 string                           `json:"approval_evidence_sha256"`
	PreviousRecovery       *CompetitionJobReevaluationCycle `json:"previous_recovery,omitempty"`
}

// Scope is structural here; the authenticated controller and independent SQL
// predicate separately restrict grants to the one approved production job.
func validCompetitionOperatorSecondRecoveryCycle(cycle *CompetitionJobReevaluationCycle) bool {
	second := cycle.SecondRecovery
	if cycle.Schema != 3 || cycle.Cycle != 2 || cycle.IncidentClass != CompetitionJobReevaluationOperatorRequestedPatchRetry ||
		second == nil || second.Schema != 1 || second.JobId != cycle.JobId ||
		!competitionAutomationSha256.MatchString(second.PreviousReceiptSha256) || !competitionAutomationSha256.MatchString(second.ApprovalEvidenceSha256) {
		return false
	}
	previous := second.PreviousRecovery
	if previous == nil || previous.Schema != 2 || previous.Cycle != 1 || previous.SecondRecovery != nil ||
		previous.IncidentClass != CompetitionJobReevaluationOperatorRequestedPatchRetry ||
		ValidateCompetitionJobReevaluationCycle(previous) != nil || previous.ReceiptSha256 != second.PreviousReceiptSha256 ||
		previous.Environment != cycle.Environment || previous.CompetitionId != cycle.CompetitionId || previous.SeasonId != cycle.SeasonId ||
		previous.JobId != cycle.JobId || previous.RoundId != cycle.RoundId || previous.PatchSha256 != cycle.PatchSha256 ||
		previous.RoundPolicySha256 != cycle.RoundPolicySha256 || previous.BaselineSha256 != cycle.BaselineSha256 ||
		previous.StartedAt == nil || cycle.AuthorizedAt.Before(*previous.StartedAt) || previous.Distributed == nil || cycle.Distributed == nil ||
		!cycle.Distributed.OriginalStartedAt.Equal(*previous.StartedAt) ||
		previous.Distributed.SourceLockSha256 != cycle.Distributed.SourceLockSha256 ||
		previous.Distributed.PolicyApprovalSha256 != cycle.Distributed.PolicyApprovalSha256 ||
		previous.Distributed.EvaluatorImageDigest != cycle.Distributed.EvaluatorImageDigest {
		return false
	}
	copy := *cycle
	copy.Schema, copy.SecondRecovery = 2, nil
	return validCompetitionOperatorRequestedPatchRetryCycle(&copy)
}
