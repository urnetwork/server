// The second recovery is a separate exact-job branch of the existing local
// operator control flow; no setting exposes a general multi-cycle policy.
package controller

import (
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// A schema-three request cannot borrow an old authorization or provide its own
// predecessor. The store loads that receipt from the immutable first audit.
func competitionSecondRecoveryShape(a model.CompetitionJobReevaluationAuthorization) bool {
	second := a.SecondRecovery
	return a.Schema == 3 && a.Kind == model.CompetitionOperatorRequestedPatchRetryAuthorizationKind &&
		a.IncidentClass == model.CompetitionJobReevaluationOperatorRequestedPatchRetry && a.ExpectedCycle == 1 &&
		second != nil && second.Schema == 1 && second.JobId == a.JobId && second.PreviousRecovery == nil &&
		sha256Pattern.MatchString(second.PreviousReceiptSha256) && sha256Pattern.MatchString(second.ApprovalEvidenceSha256)
}

// Only an unexported isolated-fixture seam replaces this immutable production
// approval. The normal zero-value settings always select the one exact job.
func competitionSecondRecoveryJobAllowed(settings *Settings, jobId server.Id) bool {
	approved := server.CompetitionOperatorSecondRecoveryJobId()
	if settings != nil && settings.secondRecoveryJobIdForTest != (server.Id{}) {
		approved = settings.secondRecoveryJobIdForTest
	}
	return jobId == approved
}

// These are fixed trusted table names, never request-supplied SQL identifiers.
func competitionRecoveryReceiptQuery(a model.CompetitionJobReevaluationAuthorization) string {
	if a.Schema == 3 {
		return `SELECT idempotency_key,request_sha256,receipt_json FROM competition_job_second_reevaluation WHERE job_id=$1`
	}
	return `SELECT idempotency_key,request_sha256,receipt_json FROM competition_job_reevaluation WHERE job_id=$1`
}
