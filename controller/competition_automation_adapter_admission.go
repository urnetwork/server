// The submission adapter opts into v2 per competition before collecting a fee.
package controller

import (
	"errors"
	"time"

	"github.com/urnetwork/server/model"
)

// Unknown or mismatched contracts fail before payment; legacy honesty checks remain untouched.
func validateApexAdmissionContract(competitionId string, round RoundResult, enabledCompetitionId string) error {
	if round.DecisionMode == "" && round.RequiredApiCapability == "" {
		return nil
	}
	if round.DecisionMode != model.CompetitionDecisionModeV2 || round.RequiredApiCapability != model.CompetitionAutomationCapability ||
		enabledCompetitionId == "" || competitionId != enabledCompetitionId {
		return errors.New("Apex submission requires explicit support for this competition's v2 decision contract")
	}
	return nil
}

// Polling may publish immutable scores, never an approved winner or payment status.
func recordApexV2Poll(record *ApexAdapterRecord, job ScoreJobResult, now time.Time) error {
	if record.DecisionMode != model.CompetitionDecisionModeV2 || job.DecisionMode != record.DecisionMode ||
		job.RequiredApiCapability != record.RequiredApiCapability || record.JobId != job.JobId ||
		record.RoundId != job.RoundId || record.Staging != job.Staging || record.CanonicalPatchSha256 != job.PatchSha256 {
		return errors.New("v2 poll changed an immutable admission identity")
	}
	if err := validateScoreJobPoll(job); err != nil {
		return err
	}
	if record.EvaluationStatus == "failed" || record.EvaluationStatus == "canceled" {
		if job.EvaluationStatus != record.EvaluationStatus ||
			(record.EvaluationFailure == nil) != (job.EvaluationFailure == nil) ||
			record.EvaluationFailure != nil && *record.EvaluationFailure != *job.EvaluationFailure {
			return errors.New("v2 poll changed a terminal evaluation outcome")
		}
	}
	if record.Score != nil && job.Score != nil {
		previous, _ := model.CompetitionAutomationDigest(record.Score)
		current, _ := model.CompetitionAutomationDigest(job.Score)
		if previous != current {
			return errors.New("v2 poll changed a published score")
		}
	}
	if job.Score != nil {
		if err := validateScore(job.Score); err != nil {
			return err
		}
		record.Score, record.Published = job.Score, true
	}
	if job.EvalError != nil {
		if job.EvalError.Retriable || validateEvaluationError(job.EvalError) != nil {
			return errors.New("v2 poll returned an invalid terminal error")
		}
		record.EvalError, record.Published = job.EvalError, true
	}
	if record.Published && job.Score == nil && job.EvalError == nil {
		return errors.New("v2 poll retracted a published outcome")
	}
	record.State, record.EvaluationStatus, record.EvaluationFailure = job.State, job.EvaluationStatus, job.EvaluationFailure
	record.UpdatedAt = now.UTC()
	return nil
}
