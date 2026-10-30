// Private completed-score feedback is credential-owner scoped, not public
// publication. Only fixed numeric and boolean diagnostics cross this boundary.
package controller

import (
	"errors"
	"math"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// Direct service/store callers cannot bypass the transports' role checks.
// Legacy operators retain inspection authority; v2 never upgrades that role.
func competitionScorePrincipalAllowed(principal *Principal, v2 bool) bool {
	return principal != nil && (principal.Role == "submitter" && principal.Id != "" ||
		!v2 && principal.Role == "operator")
}

// Keeps publication state and full errors sealed even when the authorized
// submitter can already inspect a completed production score. Holds use this
// same projection instead of returning a previously published full document.
func unpublishedScoreJobView(job *queuedJob, principal *Principal, result ScoreJobResult) ScoreJobResult {
	stagingDiscarded := isStagingDiscardedJob(result)
	result.State = scoreJobStateView(result.State, false, principal)
	result.Score = nil
	if !stagingDiscarded {
		result.EvalError = nil
	}
	if principal != nil && principal.Role == "submitter" && principal.Id != "" &&
		!job.Staging && !job.Round.Staging && !job.Round.Canceled &&
		job.State == "succeeded" && job.EvalError == nil {
		result.Score = privateCompletedScore(job.Score)
	}
	return result
}

// Copies only reviewed score fields. Unknown gate identifiers, free-form
// strings, evaluation identities, artifacts and findings stay in stored evidence.
// Reapplying the projection is idempotent, including after durable JSON decoding.
func privateCompletedScore(score *ScoreResult) *ScoreResult {
	if validateScore(score) != nil {
		return nil
	}
	result := ScoreResult{
		ScoreSchema: score.ScoreSchema, RawScore: score.RawScore, NormalizedScore: score.NormalizedScore,
		Placeable: score.Placeable, TakeoverEligible: score.TakeoverEligible,
		Gates: map[string]Gate{}, Significance: score.Significance,
	}
	for _, gateFields := range []struct {
		name         string
		numberFields []string
		boolFields   []string
	}{
		{name: "G1_success", numberFields: []string{"candidate", "baseline", "absolute_min_inclusive", "baseline_minus_max"}},
		{name: "G2_volume", numberFields: []string{"candidate_requests", "request_min", "request_max", "candidate_bytes", "bytes_min", "bytes_max"}},
		{name: "G3_path_integrity", numberFields: []string{"minimum_replicate_coverage", "minimum_inclusive"}},
		{name: "G4_matchmaking", numberFields: []string{
			"minimum_replicate_sample_span_fraction", "baseline_minimum_replicate_sample_span_fraction", "sample_span_minimum_inclusive",
			"candidate_load_p95_ms", "baseline_load_p95_ms", "maximum_inclusive_ms",
			"candidate_pool_p05", "baseline_pool_p05", "pool_minimum_inclusive",
		}, boolFields: []string{"all_replicates_have_samples", "all_pools_nonempty", "all_replicates_span_at_least_minimum"}},
		{name: "G5_stability", boolFields: []string{"all_replicates_clean"}},
		{name: "G6_resources", boolFields: []string{"all_replicates_within_limits"}},
	} {
		if gate, ok := score.Gates[gateFields.name]; ok {
			result.Gates[gateFields.name] = Gate{
				Passed:  gate.Passed,
				Details: privateScoreDiagnosticFields(gate.Details, gateFields.numberFields, gateFields.boolFields),
			}
		}
	}
	result.Diagnostics = privateScoreDiagnosticFields(score.Diagnostics,
		[]string{"replicate_count", "request_timeout_ms", "takeover_margin", "baseline_takeover_raw_max"},
		[]string{"baseline_takeover_eligible"})
	for _, name := range []string{"baseline", "candidate"} {
		if summary, ok := score.Diagnostics[name].(map[string]any); ok {
			result.Diagnostics[name] = privateScoreDiagnosticFields(summary, []string{
				"raw_score", "success_rate", "request_count", "received_bytes", "accounting_coverage",
				"findproviders_load_p95_ms", "findproviders_pool_p05", "findproviders_sample_span_fraction",
			}, nil)
		}
	}
	if replicates, ok := score.Diagnostics["replicates"].([]any); ok {
		// The validated statistical contract bounds diagnostics to measured runs.
		replicates = replicates[:min(len(replicates), score.Significance.ReplicateCount)]
		privateReplicates := make([]any, 0, len(replicates))
		for _, replicate := range replicates {
			if summary, ok := replicate.(map[string]any); ok {
				privateReplicates = append(privateReplicates, privateScoreDiagnosticFields(summary, []string{
					"raw_score", "success_rate", "request_count", "received_bytes", "provider_egress_bytes", "accounting_coverage",
					"findproviders_samples", "findproviders_empty_pools", "findproviders_load_p95_ms", "findproviders_pool_p05", "findproviders_sample_span_fraction",
				}, nil))
			}
		}
		result.Diagnostics["replicates"] = privateReplicates
	}
	if len(result.Diagnostics) == 0 {
		result.Diagnostics = nil
	}
	return &result
}

// A known key alone is not enough: numeric/bool schema fields never forward
// strings, nested objects or non-finite values placed under that key.
func privateScoreDiagnosticFields(values map[string]any, numberFields []string, boolFields []string) map[string]any {
	result := map[string]any{}
	for _, field := range numberFields {
		switch value := values[field].(type) {
		case float64:
			if !math.IsNaN(value) && !math.IsInf(value, 0) {
				result[field] = value
			}
		case float32:
			if !math.IsNaN(float64(value)) && !math.IsInf(float64(value), 0) {
				result[field] = value
			}
		case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
			result[field] = value
		}
	}
	for _, field := range boolFields {
		if value, ok := values[field].(bool); ok {
			result[field] = value
		}
	}
	return result
}

// The reference adapter accepts private scores only in this exact reviewed
// shape; a new server field cannot silently become private-to-public leakage.
func privateScoreProjectionMatches(score *ScoreResult) bool {
	projected := privateCompletedScore(score)
	if projected == nil {
		return false
	}
	expected, err := model.CompetitionAutomationDigest(projected)
	if err != nil {
		return false
	}
	actual, err := model.CompetitionAutomationDigest(score)
	return err == nil && actual == expected
}

// A private score remains attached to its immutable admission and cannot later
// become a failed job or a different measurement. Legacy polls without feedback
// during a rolling deployment retain the stronger previously observed record.
func recordApexPrivateScore(record *ApexAdapterRecord, job ScoreJobResult) error {
	if !record.Staging && record.State == "completed" && record.EvaluationStatus == "completed" && job.EvaluationStatus != "completed" {
		return errors.New("competition poll changed completed production scoring")
	}
	if record.PrivateScore != nil {
		if job.EvaluationStatus != "completed" {
			return errors.New("competition poll changed a completed private score")
		}
		if job.Score != nil {
			previous, previousErr := model.CompetitionAutomationDigest(record.PrivateScore)
			current, currentErr := model.CompetitionAutomationDigest(privateCompletedScore(job.Score))
			if previousErr != nil || currentErr != nil || previous != current {
				return errors.New("competition poll changed a completed private score")
			}
		}
	}
	if job.Score != nil && job.State == "completed" {
		if record.Published || record.Staging || !privateScoreProjectionMatches(job.Score) {
			return errors.New("competition poll returned invalid private completed-score feedback")
		}
		record.PrivateScore = job.Score
		record.PrivateScoreRefreshAt = nil
	}
	if job.EvaluationStatus == "failed" || job.EvaluationStatus == "canceled" {
		record.PrivateScoreRefreshAt = nil
	}
	return nil
}

// Each adapter owns its HTTP client and retry defaults. Private feedback is
// enabled by default; the interval bounds old-replica and transport retries.
func DefaultApexAdapterOptions() ApexAdapterOptions {
	return ApexAdapterOptions{
		HttpClient: server.NewHttpClient(30 * time.Second), MaxAttempts: 5,
		PrivateScoreRefreshEvery: time.Minute,
	}
}

// Ordinary work keeps the historical FIFO. A separate lane probes completed
// production records without making ambiguous or failed evaluations successful.
func apexAdapterRecordPending(record ApexAdapterRecord) bool {
	return record.JobId != (server.Id{}) && !record.Published &&
		(record.Staging || record.State != "completed") && record.State != "failed" &&
		record.State != "canceled" && record.State != "invalid"
}

// Known scoring success and ambiguous cached admissions both need an owner
// poll. An absent status remains unknown until the API supplies it; known failed
// and canceled evaluations never enter this lane.
func apexPrivateScoreRefreshEligible(record ApexAdapterRecord) bool {
	return record.JobId != (server.Id{}) && !record.Staging && !record.Published &&
		record.State == "completed" && (record.EvaluationStatus == "" || record.EvaluationStatus == "completed") &&
		record.EvaluationFailure == nil && record.EvalError == nil && record.PrivateScore == nil
}

// Reserves a due refresh before releasing the file lock, including when HTTP
// later fails. The alternating lane preference cannot starve ordinary FIFO or
// older completed jobs; no network work runs inside this state transaction.
func (self *ApexAdapterFileStore) nextPrivateScoreAwarePoll(now time.Time, refreshEvery time.Duration) (*ApexAdapterRecord, error) {
	if refreshEvery <= 0 {
		return nil, errors.New("private score refresh interval must be positive")
	}
	var result *ApexAdapterRecord
	noWrite := errors.New("poll selection leaves durable state unchanged")
	err := self.update(func(state *apexAdapterState) error {
		var active, refresh *ApexAdapterRecord
		for i := range state.Records {
			record := &state.Records[i]
			if apexAdapterRecordPending(*record) && (active == nil || record.Sequence < active.Sequence) {
				active = record
			}
			if !apexPrivateScoreRefreshEligible(*record) || record.PrivateScoreRefreshAt != nil && now.Before(*record.PrivateScoreRefreshAt) {
				continue
			}
			if refresh == nil || record.PrivateScoreRefreshAt == nil && refresh.PrivateScoreRefreshAt != nil ||
				record.PrivateScoreRefreshAt != nil && refresh.PrivateScoreRefreshAt != nil && record.PrivateScoreRefreshAt.Before(*refresh.PrivateScoreRefreshAt) ||
				(record.PrivateScoreRefreshAt == nil && refresh.PrivateScoreRefreshAt == nil ||
					record.PrivateScoreRefreshAt != nil && refresh.PrivateScoreRefreshAt != nil && record.PrivateScoreRefreshAt.Equal(*refresh.PrivateScoreRefreshAt)) && record.Sequence < refresh.Sequence {
				refresh = record
			}
		}
		selected := active
		changed := false
		if refresh != nil && (active == nil || state.PrivateScoreRefreshNext) {
			selected = refresh
			nextRefreshAt := now.Add(refreshEvery).UTC()
			for i := range state.Records {
				record := &state.Records[i]
				if record.JobId == selected.JobId && apexPrivateScoreRefreshEligible(*record) {
					if !apexAdapterJobIdentityMatches(*selected, *record) {
						return errors.New("Apex cached refresh changed an immutable admission association")
					}
					record.PrivateScoreRefreshAt = &nextRefreshAt
				}
			}
			state.PrivateScoreRefreshNext = false
			changed = true
		} else if active != nil && refresh != nil {
			state.PrivateScoreRefreshNext = true
			changed = true
		}
		if selected == nil {
			return noWrite
		}
		copy := apexAdapterRecordView(*selected)
		result = &copy
		if !changed {
			// Ordinary FIFO reads and idle polls do not need an extra fsync.
			return noWrite
		}
		return nil
	})
	if errors.Is(err, noWrite) {
		return result, nil
	}
	return result, err
}
