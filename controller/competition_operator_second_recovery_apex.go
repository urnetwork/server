// A second authenticated receipt appends the first recovered failure; it does
// not erase original observations or reopen scored/published outcomes.
package controller

import (
	"errors"
	"time"

	"github.com/urnetwork/server/model"
)

// An offline alias may learn both failures from the authenticated receipt chain.
// Known first-cycle authority and observed failure must agree before advancing.
func prepareApexSecondRecoveryRecord(record *ApexAdapterRecord, cycle *model.CompetitionJobReevaluationCycle, now time.Time) error {
	if err := validateApexReevaluationScope(*record, cycle); err != nil {
		return err
	}
	if cycle.Schema != 3 || cycle.SecondRecovery == nil || cycle.SecondRecovery.PreviousRecovery == nil {
		return errors.New("second recovery lacks its first receipt")
	}
	if record.Reevaluation != nil && record.Reevaluation.Cycle == 2 {
		if validateApexReevaluationRecord(*record) != nil || record.Reevaluation.ReceiptSha256 != cycle.ReceiptSha256 ||
			record.Reevaluation.StartedAt != nil && (cycle.StartedAt == nil || !record.Reevaluation.StartedAt.Equal(*cycle.StartedAt)) {
			return errors.New("second recovery changed its pinned receipt or first claim")
		}
		record.Reevaluation = cloneApexReevaluationCycle(cycle)
		return nil
	}
	previous := cycle.SecondRecovery.PreviousRecovery
	if record.Reevaluation == nil {
		if err := prepareApexReevaluationRecord(record, previous, now); err != nil {
			return err
		}
	} else if validateApexReevaluationRecord(*record) != nil || record.Reevaluation.Cycle != 1 ||
		record.Reevaluation.ReceiptSha256 != previous.ReceiptSha256 ||
		record.Reevaluation.StartedAt != nil && !record.Reevaluation.StartedAt.Equal(*previous.StartedAt) {
		return errors.New("second recovery contradicts the cached first receipt")
	}
	if record.Published || record.Winner || record.HonestyReview != "" || record.PrivateScore != nil || record.Score != nil || record.EvalError != nil ||
		record.EvaluationStatus == "completed" || record.EvaluationStatus == "canceled" {
		return errors.New("second recovery cannot replace a scored or published outcome")
	}
	observation := ApexReevaluationObservation{Cycle: 1, State: "completed", EvaluationStatus: "failed",
		EvaluationFailure: cycle.PreviousFailure, PreviousTerminalSha256: cycle.PreviousTerminalSha256, ObservedAt: now.UTC()}
	if record.EvaluationStatus == "failed" {
		if record.EvaluationFailure != nil && *record.EvaluationFailure != cycle.PreviousFailure {
			return errors.New("second recovery contradicts a known first-cycle failure")
		}
		observation.State, observation.ObservedAt = record.State, record.UpdatedAt
	} else if record.EvaluationFailure != nil || (record.EvaluationStatus != "queued" && record.EvaluationStatus != "running" && record.EvaluationStatus != "") {
		return errors.New("second recovery has an incompatible prior lifecycle")
	}
	record.ReevaluationHistory = append(record.ReevaluationHistory, observation)
	record.Reevaluation = cloneApexReevaluationCycle(cycle)
	record.State, record.EvaluationStatus, record.EvaluationFailure = "queued", "queued", nil
	record.PrivateScoreRefreshAt = nil
	return nil
}
