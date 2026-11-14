// One authenticated recovery cycle can supersede a cached evaluator failure
// failure without erasing its history or acquiring publication authority.
package controller

import (
	"errors"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The original message-free failure remains append-only in the private cache.
// A single-use recovery permits exactly one prior, original-cycle observation.
type ApexReevaluationObservation struct {
	Cycle                  int               `json:"cycle"`
	State                  string            `json:"state"`
	EvaluationStatus       string            `json:"evaluation_status"`
	EvaluationFailure      EvaluationFailure `json:"evaluation_failure"`
	PreviousTerminalSha256 string            `json:"previous_terminal_sha256"`
	ObservedAt             time.Time         `json:"observed_at"`
}

// Per-store cursor work is bounded independently of the existing owner lane.
type apexReevaluationRefreshSettings struct {
	// Maximum cache records examined by one recovery probe; non-positive refuses.
	ScanLimit int
}

// A small pass advances durable progress even when no candidate is due.
func defaultApexReevaluationRefreshSettings() apexReevaluationRefreshSettings {
	return apexReevaluationRefreshSettings{ScanLimit: 64}
}

// Copies the mutable first-claim timestamp as well as the receipt value.
func cloneApexReevaluationCycle(cycle *model.CompetitionJobReevaluationCycle) *model.CompetitionJobReevaluationCycle {
	if cycle == nil {
		return nil
	}
	copy := *cycle
	if cycle.Distributed != nil {
		distributed := *cycle.Distributed
		if distributed.PatchRetry != nil {
			retry := *distributed.PatchRetry
			distributed.PatchRetry = &retry
		}
		copy.Distributed = &distributed
	}
	if cycle.StartedAt != nil {
		startedAt := *cycle.StartedAt
		copy.StartedAt = &startedAt
	}
	return &copy
}

// The authenticated API supplies authorization provenance. Its independently
// checked digest binds every immutable scope, previous failure and fixed runtime.
func validateApexReevaluationScope(record ApexAdapterRecord, cycle *model.CompetitionJobReevaluationCycle) error {
	if err := model.ValidateCompetitionJobReevaluationCycle(cycle); err != nil {
		return err
	}
	if record.Staging || record.JobId != cycle.JobId || record.RoundId != cycle.RoundId ||
		record.CanonicalPatchSha256 != cycle.PatchSha256 ||
		record.CompetitionId != "" && record.CompetitionId != cycle.CompetitionId {
		return errors.New("reevaluation changed an immutable Apex admission scope")
	}
	return nil
}

// A saved marker needs its original failure; metadata cannot silently turn an
// existing successful or published evaluation into another sampling opportunity.
func validateApexReevaluationRecord(record ApexAdapterRecord) error {
	if record.Reevaluation == nil {
		if len(record.ReevaluationHistory) != 0 {
			return errors.New("Apex reevaluation history has no authenticated cycle")
		}
		return nil
	}
	if err := validateApexReevaluationScope(record, record.Reevaluation); err != nil {
		return err
	}
	if len(record.ReevaluationHistory) != 1 {
		return errors.New("Apex reevaluation must retain one original failure")
	}
	previous := record.ReevaluationHistory[0]
	if previous.Cycle != 0 || previous.EvaluationStatus != "failed" ||
		(previous.State != "completed" && previous.State != "failed") || previous.ObservedAt.IsZero() ||
		previous.EvaluationFailure != record.Reevaluation.PreviousFailure ||
		previous.PreviousTerminalSha256 != record.Reevaluation.PreviousTerminalSha256 {
		return errors.New("Apex reevaluation changed its original failure history")
	}
	return nil
}

// First observation may follow an offline interval. A receipt can supply the
// missed prior failure, but never contradict already known completed scoring.
func prepareApexReevaluationRecord(record *ApexAdapterRecord, cycle *model.CompetitionJobReevaluationCycle, now time.Time) error {
	if err := validateApexReevaluationScope(*record, cycle); err != nil {
		return err
	}
	if record.Reevaluation != nil {
		if err := validateApexReevaluationRecord(*record); err != nil {
			return err
		}
		if record.Reevaluation.ReceiptSha256 != cycle.ReceiptSha256 {
			return errors.New("Apex reevaluation changed its pinned authorization receipt")
		}
		if previous := record.Reevaluation.StartedAt; previous != nil &&
			(cycle.StartedAt == nil || !previous.Equal(*cycle.StartedAt)) {
			return errors.New("Apex reevaluation changed its first-claim timestamp")
		}
		record.Reevaluation = cloneApexReevaluationCycle(cycle)
		return nil
	}
	if len(record.ReevaluationHistory) != 0 || record.Published || record.Winner || record.HonestyReview != "" ||
		record.PrivateScore != nil || record.Score != nil || record.EvalError != nil ||
		record.EvaluationStatus == "completed" || record.EvaluationStatus == "canceled" {
		return errors.New("Apex reevaluation cannot replace a known scored or published outcome")
	}
	previous := ApexReevaluationObservation{
		Cycle: 0, State: "completed", EvaluationStatus: "failed", EvaluationFailure: cycle.PreviousFailure,
		PreviousTerminalSha256: cycle.PreviousTerminalSha256, ObservedAt: now.UTC(),
	}
	if record.EvaluationStatus == "failed" {
		missingPatchFailure := record.EvaluationFailure == nil && cycle.IncidentClass == model.CompetitionJobReevaluationOperatorRequestedPatchRetry
		if !missingPatchFailure && (record.EvaluationFailure == nil || *record.EvaluationFailure != cycle.PreviousFailure) {
			return errors.New("Apex reevaluation contradicts its cached original failure")
		}
		previous.State, previous.ObservedAt = record.State, record.UpdatedAt
	} else if record.EvaluationFailure != nil ||
		(record.EvaluationStatus != "" && record.EvaluationStatus != "queued" && record.EvaluationStatus != "running") {
		return errors.New("Apex reevaluation has an incompatible prior observation")
	}
	record.Reevaluation = cloneApexReevaluationCycle(cycle)
	record.ReevaluationHistory = []ApexReevaluationObservation{previous}
	record.State, record.EvaluationStatus, record.EvaluationFailure = "queued", "queued", nil
	record.PrivateScoreRefreshAt = nil
	return nil
}

// Cycle admission precedes the unchanged per-cycle legacy/v2 lifecycle gates.
// Failure leaves the caller's record unchanged, including its prior history.
func recordApexReevaluationAwarePoll(record *ApexAdapterRecord, job ScoreJobResult, now time.Time) error {
	copy := *record
	copy.Reevaluation = cloneApexReevaluationCycle(record.Reevaluation)
	copy.ReevaluationHistory = append([]ApexReevaluationObservation(nil), record.ReevaluationHistory...)
	if job.Reevaluation == nil {
		if copy.Reevaluation != nil {
			return errors.New("Apex poll replayed an earlier evaluation cycle")
		}
	} else {
		if job.JobId != job.Reevaluation.JobId || job.RoundId != job.Reevaluation.RoundId ||
			job.PatchSha256 != job.Reevaluation.PatchSha256 || job.Staging || job.EvaluationStatus == "" ||
			(job.EvaluationStatus == "running" || job.EvaluationStatus == "completed" || job.EvaluationStatus == "failed") && job.Reevaluation.StartedAt == nil {
			return errors.New("Apex poll returned inconsistent reevaluation scope or lifecycle")
		}
		if err := prepareApexReevaluationRecord(&copy, job.Reevaluation, now); err != nil {
			return err
		}
	}
	var err error
	if copy.DecisionMode != "" {
		err = recordApexV2Poll(&copy, job, now)
	} else {
		err = recordApexLegacyPoll(&copy, job, now)
	}
	if err != nil {
		return err
	}
	if err := validateApexReevaluationRecord(copy); err != nil {
		return err
	}
	*record = copy
	return nil
}

// Temporary publication comparisons lack a job identity and keep their existing
// score checks. Real cache associations must agree on cycle and receipt.
func validateApexReevaluationObservation(first ApexAdapterRecord, second ApexAdapterRecord) error {
	if first.JobId == (server.Id{}) || second.JobId == (server.Id{}) {
		return nil
	}
	if (first.Reevaluation == nil) != (second.Reevaluation == nil) {
		return errors.New("Apex cached associations disagree on evaluation cycle")
	}
	if first.Reevaluation != nil {
		if first.Reevaluation.ReceiptSha256 != second.Reevaluation.ReceiptSha256 ||
			(first.Reevaluation.StartedAt == nil) != (second.Reevaluation.StartedAt == nil) ||
			first.Reevaluation.StartedAt != nil && !first.Reevaluation.StartedAt.Equal(*second.Reevaluation.StartedAt) {
			return errors.New("Apex cached associations disagree on reevaluation authority")
		}
	}
	return nil
}

// A late cache alias inherits the same authenticated cycle, not a new job or
// fee. Only current-cycle evidence participates in the existing strength order.
func reconcileApexReevaluationAssociations(records []*ApexAdapterRecord, now time.Time) (bool, error) {
	if len(records) == 0 {
		return false, nil
	}
	hasCycle := false
	for _, record := range records {
		if record == nil {
			return false, errors.New("Apex reevaluation association is missing")
		}
		hasCycle = hasCycle || record.Reevaluation != nil
	}
	if !hasCycle {
		return false, nil
	}
	originalRecords := records
	records = make([]*ApexAdapterRecord, len(originalRecords))
	for i, record := range originalRecords {
		copy := *record
		copy.Reevaluation = cloneApexReevaluationCycle(record.Reevaluation)
		copy.ReevaluationHistory = append([]ApexReevaluationObservation(nil), record.ReevaluationHistory...)
		records[i] = &copy
	}
	var observation *ApexAdapterRecord
	for _, record := range records {
		if !apexAdapterJobIdentityMatches(*records[0], *record) {
			return false, errors.New("Apex reevaluation changed a cached admission association")
		}
		if record.Reevaluation == nil {
			continue
		}
		if err := validateApexReevaluationRecord(*record); err != nil {
			return true, err
		}
		if observation != nil {
			if record.Reevaluation.ReceiptSha256 != observation.Reevaluation.ReceiptSha256 {
				return true, errors.New("Apex cached reevaluation replayed another receipt")
			}
			if err := validateApexJobObservation(*observation, *record); err != nil {
				return true, err
			}
		}
		if observation == nil || apexJobObservationRank(*record) > apexJobObservationRank(*observation) {
			observation = record
		}
	}
	if observation == nil {
		return false, nil
	}
	current := *observation
	for _, record := range records {
		if err := prepareApexReevaluationRecord(record, current.Reevaluation, now); err != nil {
			return true, err
		}
		record.State, record.EvaluationStatus, record.EvaluationFailure = current.State, current.EvaluationStatus, current.EvaluationFailure
		record.Published, record.Winner, record.HonestyReview = current.Published, current.Winner, current.HonestyReview
		record.PrivateScore, record.Score, record.EvalError = current.PrivateScore, current.Score, current.EvalError
		record.PrivateScoreRefreshAt = current.PrivateScoreRefreshAt
		record.UpdatedAt = now.UTC()
	}
	for i, record := range records {
		*originalRecords[i] = *record
	}
	return true, nil
}

// A read-only owner probe is not reevaluation permission. An unstable run may
// discover a typed operator receipt. Older APIs omitted some reviewed generic
// failures; their bounded read-only lookup grants no execution authority.
func apexReevaluationRefreshEligible(record ApexAdapterRecord) bool {
	return record.JobId != (server.Id{}) && !record.Staging && !record.Published && record.Reevaluation == nil &&
		record.EvaluationStatus == "failed" &&
		(record.EvaluationFailure == nil || !record.EvaluationFailure.Retriable &&
			((record.EvaluationFailure.Kind == "infrastructure" && (record.EvaluationFailure.Code == "artifact_archive_failed" || record.EvaluationFailure.Code == "evaluation_time_budget_exhausted" || record.EvaluationFailure.Code == "executor_process_failed")) ||
				(record.EvaluationFailure.Kind == "submission" && record.EvaluationFailure.Code == "run_unstable"))) &&
		record.PrivateScore == nil && record.Score == nil && record.EvalError == nil
}

// Reservation keys carry no authority and cannot grow beyond the admitted cache.
func validateApexReevaluationRefreshState(state *apexAdapterState) error {
	if state.ReevaluationRefreshCursor < 0 ||
		(len(state.Records) == 0 && state.ReevaluationRefreshCursor != 0) ||
		(len(state.Records) != 0 && state.ReevaluationRefreshCursor >= len(state.Records)) ||
		len(state.ReevaluationRefreshAt) > len(state.Records) {
		return errors.New("Apex reevaluation refresh cursor or reservations exceed the cache")
	}
	for value, at := range state.ReevaluationRefreshAt {
		jobId, err := server.ParseId(value)
		if err != nil || jobId == (server.Id{}) || jobId.String() != value || at.IsZero() {
			return errors.New("Apex reevaluation refresh reservation is invalid")
		}
	}
	return nil
}

// Each pass examines at most the owning store's scan budget. A job-keyed durable
// reservation bounds aliases and transport failures before any HTTP work starts.
func (self *ApexAdapterFileStore) reserveApexReevaluationRefresh(now time.Time, refreshEvery time.Duration) (*ApexAdapterRecord, bool, error) {
	if refreshEvery <= 0 || self.reevaluationSettings.ScanLimit <= 0 {
		return nil, false, errors.New("Apex reevaluation refresh interval and scan limit must be positive")
	}
	var result *ApexAdapterRecord
	tried := false
	noWrite := errors.New("empty recovery cache needs no durable reservation")
	err := self.update(func(state *apexAdapterState) error {
		if len(state.Records) == 0 {
			tried = true
			return noWrite
		}
		if !state.ReevaluationRefreshNext {
			state.ReevaluationRefreshNext = true
			return nil
		}
		state.ReevaluationRefreshNext = false
		tried = true
		for examined := 0; examined < min(len(state.Records), self.reevaluationSettings.ScanLimit); examined++ {
			index := state.ReevaluationRefreshCursor
			state.ReevaluationRefreshCursor = (index + 1) % len(state.Records)
			record := &state.Records[index]
			if !apexReevaluationRefreshEligible(*record) {
				continue
			}
			jobKey := record.JobId.String()
			if at, ok := state.ReevaluationRefreshAt[jobKey]; ok && now.Before(at) {
				continue
			}
			if state.ReevaluationRefreshAt == nil {
				state.ReevaluationRefreshAt = map[string]time.Time{}
			}
			state.ReevaluationRefreshAt[jobKey] = now.Add(refreshEvery).UTC()
			copy := apexAdapterRecordView(*record)
			result = &copy
			return nil
		}
		return nil
	})
	if errors.Is(err, noWrite) {
		err = nil
	}
	return result, tried, err
}

// Alternate the bounded recovery lane with the existing FIFO/private lane. An
// idle normal lane can use its turn immediately without a second recovery scan.
func (self *ApexAdapterFileStore) nextApexReevaluationAwarePoll(now time.Time, refreshEvery time.Duration) (*ApexAdapterRecord, error) {
	recovery, tried, err := self.reserveApexReevaluationRefresh(now, refreshEvery)
	if err != nil || recovery != nil {
		return recovery, err
	}
	ordinary, err := self.nextPrivateScoreAwarePoll(now, refreshEvery)
	if err != nil || ordinary != nil || tried {
		return ordinary, err
	}
	recovery, _, err = self.reserveApexReevaluationRefresh(now, refreshEvery)
	return recovery, err
}
