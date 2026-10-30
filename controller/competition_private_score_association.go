// Canonical cache identity belongs to a job, not to one Apex submission. Only
// existing, exactly matching admissions can share its lifecycle and feedback.
package controller

import (
	"errors"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Input bytes, fee receipts and submission/miner IDs remain admission-local;
// different patch encodings may canonicalize to the same immutable API job.
func apexAdapterJobIdentityMatches(first ApexAdapterRecord, second ApexAdapterRecord) bool {
	return first.JobId != (server.Id{}) && first.JobId == second.JobId &&
		first.RoundId == second.RoundId && first.CanonicalPatchSha256 == second.CanonicalPatchSha256 &&
		first.StatusUrl == second.StatusUrl && first.Staging == second.Staging &&
		first.DecisionMode == second.DecisionMode && first.RequiredApiCapability == second.RequiredApiCapability &&
		first.CompetitionId == second.CompetitionId
}

// Matching identities cannot hold contradictory terminal measurements. Missing
// legacy fields are weaker evidence, not permission to erase a known outcome.
func validateApexJobObservation(first ApexAdapterRecord, second ApexAdapterRecord) error {
	first, second = apexAdapterRecordView(first), apexAdapterRecordView(second)
	terminal := func(status string) bool {
		return status == "completed" || status == "failed" || status == "canceled"
	}
	if terminal(first.EvaluationStatus) && terminal(second.EvaluationStatus) && first.EvaluationStatus != second.EvaluationStatus {
		return errors.New("Apex cached job has contradictory terminal admission observations")
	}
	firstScore, secondScore := first.PrivateScore, second.PrivateScore
	if first.Score != nil {
		firstScore = privateCompletedScore(first.Score)
	}
	if second.Score != nil {
		secondScore = privateCompletedScore(second.Score)
	}
	if firstScore != nil && secondScore != nil {
		firstDigest, firstErr := model.CompetitionAutomationDigest(firstScore)
		secondDigest, secondErr := model.CompetitionAutomationDigest(secondScore)
		if firstErr != nil || secondErr != nil || firstDigest != secondDigest {
			return errors.New("Apex cached job changed its completed admission score")
		}
	}
	if first.Score != nil && second.Score != nil {
		firstDigest, firstErr := model.CompetitionAutomationDigest(first.Score)
		secondDigest, secondErr := model.CompetitionAutomationDigest(second.Score)
		if firstErr != nil || secondErr != nil || firstDigest != secondDigest {
			return errors.New("Apex cached job changed its published admission score")
		}
	}
	if first.EvalError != nil && second.EvalError != nil {
		firstDigest, firstErr := model.CompetitionAutomationDigest(first.EvalError)
		secondDigest, secondErr := model.CompetitionAutomationDigest(second.EvalError)
		if firstErr != nil || secondErr != nil || firstDigest != secondDigest {
			return errors.New("Apex cached job changed its published admission error")
		}
	}
	if first.EvaluationFailure != nil && second.EvaluationFailure != nil && *first.EvaluationFailure != *second.EvaluationFailure {
		return errors.New("Apex cached job changed its terminal admission failure summary")
	}
	if first.Published && second.Published && (first.Winner != second.Winner || first.HonestyReview != second.HonestyReview) {
		return errors.New("Apex cached job has inconsistent published admission decisions")
	}
	return nil
}

// Evidence strength is protocol state, not scheduling policy: public evidence
// outranks private measurements, then reviewed failures and typed completion.
func apexJobObservationRank(record ApexAdapterRecord) int {
	record = apexAdapterRecordView(record)
	switch {
	case record.Published:
		return 4
	case record.PrivateScore != nil:
		return 3
	case record.EvaluationFailure != nil:
		return 2
	case record.EvaluationStatus == "completed" || record.EvaluationStatus == "failed" || record.EvaluationStatus == "canceled":
		return 1
	default:
		return 0
	}
}

// A new legitimate cache association inherits only already observed job facts.
// It retains its own submission identity, input digest, fee and timestamps. A
// legacy completed marker alone never supplies scoring success or publication.
func reconcileApexJobAssociations(state *apexAdapterState, jobId server.Id, now time.Time) error {
	var records []*ApexAdapterRecord
	for i := range state.Records {
		record := &state.Records[i]
		if record.JobId == jobId && jobId != (server.Id{}) {
			records = append(records, record)
		}
	}
	if len(records) < 2 {
		return nil
	}
	var observation *ApexAdapterRecord
	var refreshAt *time.Time
	for _, record := range records {
		if !apexAdapterJobIdentityMatches(*records[0], *record) {
			return errors.New("Apex cached job changed an immutable admission association")
		}
		if observation != nil {
			if err := validateApexJobObservation(*observation, *record); err != nil {
				return err
			}
		}
		view := apexAdapterRecordView(*record)
		rank := apexJobObservationRank(view)
		if rank > 0 && (observation == nil || rank > apexJobObservationRank(*observation)) {
			copy := view
			observation = &copy
		}
		if record.PrivateScoreRefreshAt != nil && (refreshAt == nil || refreshAt.Before(*record.PrivateScoreRefreshAt)) {
			copy := *record.PrivateScoreRefreshAt
			refreshAt = &copy
		}
	}
	for _, record := range records {
		if observation != nil {
			record.State = observation.State
			record.EvaluationStatus = observation.EvaluationStatus
			record.EvaluationFailure = observation.EvaluationFailure
			record.Published = observation.Published
			record.Winner, record.HonestyReview = observation.Winner, observation.HonestyReview
			record.Score, record.EvalError = observation.Score, observation.EvalError
			record.PrivateScore = observation.PrivateScore
		}
		if apexPrivateScoreRefreshEligible(*record) {
			record.PrivateScoreRefreshAt = refreshAt
		} else {
			record.PrivateScoreRefreshAt = nil
		}
		record.UpdatedAt = now.UTC()
	}
	return nil
}

// Each association interprets its own copy of the original legacy response.
// Normalizing one sibling must not invent a typed success for another sibling.
func recordApexLegacyPoll(record *ApexAdapterRecord, job ScoreJobResult, now time.Time) error {
	if job.DecisionMode != "" || job.RequiredApiCapability != "" {
		return errors.New("legacy polling cannot acquire v2 semantics")
	}
	if record.JobId != job.JobId || record.RoundId != job.RoundId || record.Staging != job.Staging || record.CanonicalPatchSha256 != job.PatchSha256 {
		return errors.New("competition poll changed an immutable job identity")
	}
	stagingDiscard := isStagingDiscardedJob(job)
	privateScore := job.Score != nil && job.State == "completed" && privateScoreProjectionMatches(job.Score)
	if !record.Staging && (job.Score != nil && !privateScore || job.EvalError != nil && !stagingDiscard) {
		return errors.New("competition poll disclosed an embargoed outcome")
	}
	legacyProjection := job.EvaluationStatus == ""
	if err := normalizeLegacyScoreJobPoll(&job); err != nil {
		return err
	}
	if err := validateScoreJobPoll(job); err != nil {
		return err
	}
	if err := recordApexPrivateScore(record, job); err != nil {
		return err
	}
	legacyCompleted := legacyProjection && job.State == "completed" &&
		job.EvaluationFailure == nil && job.Score == nil && job.EvalError == nil
	if record.EvaluationStatus == "failed" {
		if legacyCompleted {
			record.UpdatedAt = now.UTC()
			return nil
		}
		if stagingDiscard && job.EvaluationStatus == "canceled" {
			record.State = job.State
			record.EvaluationStatus = job.EvaluationStatus
			record.EvaluationFailure = nil
			record.UpdatedAt = now.UTC()
			return nil
		}
		if job.EvaluationStatus != "failed" ||
			(record.EvaluationFailure == nil) != (job.EvaluationFailure == nil) ||
			record.EvaluationFailure != nil &&
				(record.EvaluationFailure.Kind != job.EvaluationFailure.Kind ||
					record.EvaluationFailure.Code != job.EvaluationFailure.Code) {
			return errors.New("competition poll changed a terminal evaluation failure")
		}
	}
	if record.EvaluationStatus == "canceled" && job.EvaluationStatus != "canceled" {
		return errors.New("competition poll changed a canceled evaluation")
	}
	if legacyCompleted {
		if record.EvaluationStatus != "completed" && record.EvaluationStatus != "canceled" {
			record.State = "completed"
			record.EvaluationStatus = ""
			record.EvaluationFailure = nil
		}
		record.UpdatedAt = now.UTC()
		return nil
	}
	if record.Staging && !stagingDiscard && (job.Score != nil || job.EvalError != nil) {
		if job.Score != nil {
			if job.State != "succeeded" || job.EvalError != nil || validateScore(job.Score) != nil {
				return errors.New("competition poll returned an invalid published staging score")
			}
		} else if job.State != "failed" || job.EvalError.Retriable || validateEvaluationError(job.EvalError) != nil {
			return errors.New("competition poll returned an invalid published staging error")
		}
		record.Score = job.Score
		record.EvalError = job.EvalError
		record.Published = true
	} else if record.Staging && !stagingDiscard && job.State != "queued" && job.State != "running" && job.State != "completed" {
		return errors.New("competition poll returned an invalid unpublished staging state")
	}
	record.State = job.State
	record.EvaluationStatus = job.EvaluationStatus
	record.EvaluationFailure = job.EvaluationFailure
	record.UpdatedAt = now.UTC()
	return nil
}
