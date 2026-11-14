// Explicit operator patch retry authority shares the distributed closure
// barrier, never the infrastructure diagnosis or runtime qualification.
package controller

import (
	"encoding/json"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The same compiled program must be separately qualified for this capability;
// an infrastructure-only receipt does not authorize retrying a submitted patch.
func bindCompetitionOperatorRequestedPatchRetryRuntime(settings *Settings, proof model.CompetitionJobReevaluationRuntime, digest string) error {
	if settings == nil || proof.Schema != 2 || proof.Kind != model.CompetitionOperatorRequestedPatchRetryRuntimeKind ||
		proof.Capability != model.CompetitionOperatorRequestedPatchRetryCapability || !proof.Accepted || !proof.ApiCompatible || !proof.WorkerCompatible ||
		len(proof.IncidentClasses) != 1 || proof.IncidentClasses[0] != model.CompetitionJobReevaluationOperatorRequestedPatchRetry ||
		!gitShaPattern.MatchString(proof.SourceCommit) || !imageDigestPattern.MatchString(proof.ApiImageDigest) ||
		!imageDigestPattern.MatchString(proof.WorkerImageDigest) || !sha256Pattern.MatchString(proof.WorkerSha256) ||
		!sha256Pattern.MatchString(proof.EvidenceSha256) || !sha256Pattern.MatchString(digest) {
		return ErrConflict
	}
	actual, err := model.CompetitionAutomationDigest(proof)
	if err != nil || actual != digest {
		return ErrConflict
	}
	copy := proof
	copy.IncidentClasses = append([]string(nil), proof.IncidentClasses...)
	settings.reevaluationRuntime, settings.reevaluationRuntimeSha256 = &copy, digest
	return nil
}

// Exact typed authority prevents a patch request from borrowing either a
// historical singleton incident or the distributed infrastructure capability.
func competitionDistributedRecoveryRequestClassAllowed(a model.CompetitionJobReevaluationAuthorization) bool {
	d := a.Distributed
	if d == nil {
		return false
	}
	if a.Kind == model.CompetitionDistributedJobReevaluationAuthorizationKind && a.IncidentClass == model.CompetitionJobReevaluationDistributedInfrastructure {
		return d.PatchRetry == nil && (d.Reason == model.CompetitionDistributedReevaluationRetentionBudget || d.Reason == model.CompetitionDistributedReevaluationLegacyPrelaunch)
	}
	if a.Kind != model.CompetitionOperatorRequestedPatchRetryAuthorizationKind || a.IncidentClass != model.CompetitionJobReevaluationOperatorRequestedPatchRetry ||
		d.Reason != model.CompetitionJobReevaluationOperatorRequestedPatchRetry || d.PatchRetry == nil || d.PatchRetry.Schema != 1 ||
		!sha256Pattern.MatchString(d.PatchRetry.PatchSha256) || !sha256Pattern.MatchString(d.PatchRetry.ApplicabilityEvidenceSha256) {
		return false
	}
	var terminal struct {
		JobId           server.Id                          `json:"job_id"`
		RoundId         server.Id                          `json:"round_id"`
		PatchSha256     string                             `json:"patch_sha256"`
		State           string                             `json:"state"`
		EvaluationCycle int                                `json:"evaluation_cycle"`
		AttemptCount    int                                `json:"attempt_count"`
		Failure         model.CompetitionEvaluationFailure `json:"eval_error_json"`
	}
	return json.Unmarshal(a.ExpectedTerminalSnapshot, &terminal) == nil && terminal.JobId == a.JobId && terminal.RoundId == a.RoundId &&
		terminal.PatchSha256 == d.PatchRetry.PatchSha256 && terminal.State == "failed" && terminal.EvaluationCycle == 0 &&
		terminal.AttemptCount == a.ExpectedAttemptCount && model.CompetitionJobReevaluationFailureAllowed(a.IncidentClass, terminal.Failure)
}
