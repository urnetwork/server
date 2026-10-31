// Deterministic operator metric projections use synthetic stored summaries.
// They never query a database, run an evaluator or expose public score data.
package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/server"
)

// Every scope, identity and timestamp is synthetic; numeric fields follow the
// actual shared score contract, not an inferred TTFB/throughput significance.
func competitionSubmissionMetricFixture(t *testing.T) (*Settings, competitionSubmissionMetricSnapshot, *ScoreResult) {
	t.Helper()
	settings := validSettings()
	settings.CompetitionId = "synthetic-score-competition"
	settings.AutomationPolicy.Enabled, settings.AutomationPolicy.Staging = true, false
	settings.AutomationPolicy.Environment, settings.AutomationPolicy.SeasonId = "synthetic-score-env", "synthetic-score-season"
	roundId := server.RequireIdFromBytes(bytes.Repeat([]byte{2}, 16))
	snapshot := competitionSubmissionMetricSnapshot{Scope: competitionSubmissionMetricScope{CompetitionId: settings.CompetitionId,
		SeasonId: settings.AutomationPolicy.SeasonId, Environment: settings.AutomationPolicy.Environment, RoundId: roundId, Epoch: 1, Margin: .12},
		SourceAt: time.Date(2020, time.January, 2, 3, 4, 5, 0, time.UTC)}
	raw, normalized := 80.0, 125.0
	score := &ScoreResult{ScoreSchema: ScoreSchema, RawScore: &raw, NormalizedScore: &normalized, Placeable: true,
		Significance: testScoreSignificance(true), Gates: map[string]Gate{}, Diagnostics: map[string]any{
			"round_id": roundId.String(), "baseline_sha256": strings.Repeat("7", 64), "takeover_margin": .12,
			"baseline": map[string]any{"raw_score": 100.0}, "baseline_takeover_raw_max": 88.0, "baseline_takeover_eligible": true,
		}}
	for _, name := range []string{"G1_success", "G2_volume", "G3_path_integrity", "G4_matchmaking", "G5_stability", "G6_resources"} {
		score.Gates[name] = Gate{Passed: true, Details: map[string]any{}}
	}
	return settings, snapshot, score
}

// These are distinct normative-median and candidate-variance mean boundaries.
func TestCompetitionSubmissionMetricProjectionSeparatesMedianAndWelch(t *testing.T) {
	_, snapshot, score := competitionSubmissionMetricFixture(t)
	jobId := server.RequireIdFromBytes(bytes.Repeat([]byte{3}, 16))
	point, err := competitionSubmissionMetricPointFromScore(snapshot.Scope, jobId, snapshot.SourceAt.Add(-time.Hour), score)
	if err != nil || point.Classification != "eligible" || point.Values != ([13]float64{80, 100, 88, 80, 100, 98, 20, 2, 12, 12, .01, .05, 0}) {
		t.Fatal("stored median/mean/significance fields were conflated or eligibility was recomputed", point, err)
	}
	if score.TakeoverEligible {
		t.Fatal("fixture must prove the native diagnostic, not a synthesized top-level flag, supplies eligibility")
	}
}

// Outcome colors reflect stored gates, significance and diagnostic eligibility.
func TestCompetitionSubmissionMetricProjectionUsesRecordedClassifications(t *testing.T) {
	for _, classification := range []string{"eligible", "significant_only", "not_significant", "gate_failed"} {
		_, snapshot, score := competitionSubmissionMetricFixture(t)
		if classification != "eligible" {
			score.Diagnostics["baseline_takeover_eligible"] = false
		}
		if classification == "not_significant" {
			score.Significance = testScoreSignificance(false)
		}
		if classification == "gate_failed" {
			score.Gates["G5_stability"] = Gate{Passed: false, Details: map[string]any{}}
			score.Placeable = false
		}
		point, err := competitionSubmissionMetricPointFromScore(snapshot.Scope, server.RequireIdFromBytes(bytes.Repeat([]byte{3}, 16)), snapshot.SourceAt, score)
		if err != nil || point.Classification != classification {
			t.Fatal("recorded outcome class changed", classification, point, err)
		}
	}
}

// Missing inferential failure records are counted, not plotted as invented zero.
// A valid failed G5 report remains a real numeric red point.
func TestCompetitionSubmissionMetricProjectionAccountsIncompleteFailedRows(t *testing.T) {
	_, snapshot, score := competitionSubmissionMetricFixture(t)
	limits := defaultCompetitionSubmissionMetricSettings()
	jobId := server.RequireIdFromBytes(bytes.Repeat([]byte{3}, 16))
	for _, incomplete := range []string{`null`, `{"score_schema":1,"gates":{},"significance":null}`} {
		if err := appendCompetitionSubmissionMetricProjection(&snapshot, limits, jobId, snapshot.SourceAt, "failed", []byte(incomplete), false); err != nil {
			t.Fatal(err)
		}
	}
	score.Gates["G5_stability"], score.Placeable = Gate{Passed: false, Details: map[string]any{}}, false
	score.Diagnostics["baseline_takeover_eligible"] = false
	content, err := json.Marshal(score)
	if err != nil {
		t.Fatal(err)
	}
	if err := appendCompetitionSubmissionMetricProjection(&snapshot, limits, jobId, snapshot.SourceAt, "failed", content, true); err != nil {
		t.Fatal(err)
	}
	if snapshot.Rows != 3 || snapshot.UnplottableFailed != 2 || len(snapshot.Points) != 1 || snapshot.Points[0].Classification != "gate_failed" || snapshot.Points[0].Values[0] != 80 {
		t.Fatal("failed row accounting omitted a valid score or fabricated an early-failure point", snapshot)
	}
}

// Overflow, foreign scope and incomplete accounting cannot publish a prefix
// or relabel the last complete same-scope roster as a new complete snapshot.
func TestCompetitionSubmissionMetricSnapshotRejectsPartialAndForeignResults(t *testing.T) {
	for _, failure := range []string{"points", "rows", "accounting", "scope"} {
		settings, snapshot, score := competitionSubmissionMetricFixture(t)
		point, err := competitionSubmissionMetricPointFromScore(snapshot.Scope, server.RequireIdFromBytes(bytes.Repeat([]byte{3}, 16)), snapshot.SourceAt, score)
		if err != nil {
			t.Fatal(err)
		}
		snapshot.Points, snapshot.Rows = []competitionSubmissionMetricPoint{point}, 1
		limits := defaultCompetitionSubmissionMetricSettings()
		limits.MaximumPoints, limits.MaximumRows = 1, 2
		collector := newCompetitionSubmissionMetricCollector(limits, func(context.Context, *Settings, competitionSubmissionMetricSettings) (competitionSubmissionMetricSnapshot, error) {
			return snapshot, nil
		})
		if err := collector.refresh(t.Context(), settings); err != nil {
			t.Fatal(err)
		}
		switch failure {
		case "points":
			snapshot.Points, snapshot.Rows = []competitionSubmissionMetricPoint{point, point}, 2
		case "rows":
			snapshot.Rows = 3
		case "accounting":
			snapshot.Rows = 2
		case "scope":
			snapshot.Scope.CompetitionId = "synthetic-foreign-competition"
		}
		if err := collector.refresh(t.Context(), settings); err == nil || collector.snapshot.Ready || collector.snapshot.Complete || len(collector.snapshot.Points) != 1 || collector.snapshot.Scope.CompetitionId != settings.CompetitionId {
			t.Fatal("partial or foreign result replaced or falsely completed the prior roster", failure, err)
		}
	}
}

// Row, point and byte budgets bound examined summaries, including early failures.
func TestCompetitionSubmissionMetricProjectionRefusesEveryBudgetOverflow(t *testing.T) {
	for _, budget := range []string{"rows", "points", "bytes"} {
		_, snapshot, score := competitionSubmissionMetricFixture(t)
		limits := defaultCompetitionSubmissionMetricSettings()
		limits.MaximumRows, limits.MaximumPoints = 2, 1
		jobId := server.RequireIdFromBytes(bytes.Repeat([]byte{3}, 16))
		content, err := json.Marshal(score)
		if err != nil {
			t.Fatal(err)
		}
		if budget == "bytes" {
			limits.MaximumProjectionBytes = len(content) - 1
		} else {
			if err := appendCompetitionSubmissionMetricProjection(&snapshot, limits, jobId, snapshot.SourceAt, "succeeded", content, true); err != nil {
				t.Fatal(err)
			}
			if budget == "rows" {
				snapshot.Rows = limits.MaximumRows
			}
		}
		if err := appendCompetitionSubmissionMetricProjection(&snapshot, limits, jobId, snapshot.SourceAt, "succeeded", content, true); err == nil {
			t.Fatal("operator metric budget silently truncated", budget)
		}
	}
}

// A failed fetch retains prior evidence but withdraws all coverage assertions.
func TestCompetitionSubmissionMetricSnapshotWithdrawsCoverageOnFetchFailure(t *testing.T) {
	settings, snapshot, score := competitionSubmissionMetricFixture(t)
	point, err := competitionSubmissionMetricPointFromScore(snapshot.Scope, server.RequireIdFromBytes(bytes.Repeat([]byte{3}, 16)), snapshot.SourceAt, score)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Points, snapshot.Rows = []competitionSubmissionMetricPoint{point}, 1
	failed := false
	collector := newCompetitionSubmissionMetricCollector(defaultCompetitionSubmissionMetricSettings(), func(context.Context, *Settings, competitionSubmissionMetricSettings) (competitionSubmissionMetricSnapshot, error) {
		if failed {
			return snapshot, errors.New("synthetic fetch failure")
		}
		return snapshot, nil
	})
	if err := collector.refresh(t.Context(), settings); err != nil {
		t.Fatal(err)
	}
	failed = true
	if err := collector.refresh(t.Context(), settings); err == nil || collector.snapshot.Ready || collector.snapshot.Complete ||
		collector.snapshot.SourceAt != snapshot.SourceAt || !reflect.DeepEqual(collector.snapshot.Points, snapshot.Points) {
		t.Fatal("failed refresh erased retained evidence or falsely advertised complete fresh data", err)
	}
}

// An epoch or configured season change must not keep the previous roster even
// when the new scoped fetch fails before it can provide any valid score.
func TestCompetitionSubmissionMetricSnapshotClearsEpochAndConfiguration(t *testing.T) {
	settings, snapshot, score := competitionSubmissionMetricFixture(t)
	point, err := competitionSubmissionMetricPointFromScore(snapshot.Scope, server.RequireIdFromBytes(bytes.Repeat([]byte{3}, 16)), snapshot.SourceAt, score)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Points, snapshot.Rows = []competitionSubmissionMetricPoint{point}, 1
	failed := false
	collector := newCompetitionSubmissionMetricCollector(defaultCompetitionSubmissionMetricSettings(), func(context.Context, *Settings, competitionSubmissionMetricSettings) (competitionSubmissionMetricSnapshot, error) {
		if failed {
			return snapshot, errors.New("synthetic new-scope failure")
		}
		return snapshot, nil
	})
	if err := collector.refresh(t.Context(), settings); err != nil {
		t.Fatal(err)
	}
	snapshot.Scope.RoundId, snapshot.Scope.Epoch = server.RequireIdFromBytes(bytes.Repeat([]byte{4}, 16)), 2
	failed = true
	if err := collector.refresh(t.Context(), settings); err == nil || len(collector.snapshot.Points) != 0 || !collector.snapshot.SourceAt.IsZero() || collector.snapshot.Scope.Epoch != 2 {
		t.Fatal("new epoch retained an old roster", err)
	}
	settings.AutomationPolicy.SeasonId = "synthetic-other-season"
	if err := collector.refresh(t.Context(), settings); err == nil || collector.snapshot.Scope.SeasonId != settings.AutomationPolicy.SeasonId || len(collector.snapshot.Points) != 0 {
		t.Fatal("new configuration exposed old or foreign-scope evidence", err)
	}
}

// Owner cancellation wins even when an injected fetch would return plausible
// data; no sleeps or service calls are needed to force either cancellation edge.
func TestCompetitionSubmissionMetricSnapshotHonorsCancellation(t *testing.T) {
	for _, beforeFetch := range []bool{false, true} {
		settings, snapshot, _ := competitionSubmissionMetricFixture(t)
		ctx, cancel := context.WithCancel(t.Context())
		called := false
		collector := newCompetitionSubmissionMetricCollector(defaultCompetitionSubmissionMetricSettings(), func(context.Context, *Settings, competitionSubmissionMetricSettings) (competitionSubmissionMetricSnapshot, error) {
			called = true
			cancel()
			return snapshot, nil
		})
		if beforeFetch {
			cancel()
		}
		if err := collector.refresh(ctx, settings); err == nil || collector.snapshot.Ready || collector.snapshot.Complete || called == beforeFetch {
			t.Fatal("canceled owner published score history or entered a canceled fetch", err)
		}
		cancel()
	}
}

// The configured stage bit selects one exact era, never a mixed-stage fallback.
// Failed reads after switching either direction must retire the old roster.
func TestCompetitionSubmissionMetricSnapshotIsolatesStageAndProduction(t *testing.T) {
	settings, snapshot, score := competitionSubmissionMetricFixture(t)
	point, err := competitionSubmissionMetricPointFromScore(snapshot.Scope, server.RequireIdFromBytes(bytes.Repeat([]byte{3}, 16)), snapshot.SourceAt, score)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Points, snapshot.Rows = []competitionSubmissionMetricPoint{point}, 1
	collector := newCompetitionSubmissionMetricCollector(defaultCompetitionSubmissionMetricSettings(), func(context.Context, *Settings, competitionSubmissionMetricSettings) (competitionSubmissionMetricSnapshot, error) {
		return snapshot, nil
	})
	if err := collector.refresh(t.Context(), settings); err != nil {
		t.Fatal(err)
	}
	settings.AutomationPolicy.Staging = true
	if err := collector.refresh(t.Context(), settings); err == nil || !collector.snapshot.Scope.Staging || len(collector.snapshot.Points) != 0 || collector.snapshot.Ready {
		t.Fatal("staging feed reused production data", err)
	}
	snapshot.Scope.Staging, snapshot.Scope.RoundId = true, server.RequireIdFromBytes(bytes.Repeat([]byte{4}, 16))
	score.Diagnostics["round_id"] = snapshot.Scope.RoundId.String()
	point, err = competitionSubmissionMetricPointFromScore(snapshot.Scope, point.JobId, point.SubmittedAt, score)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Points = []competitionSubmissionMetricPoint{point}
	if err := collector.refresh(t.Context(), settings); err != nil || !collector.snapshot.Ready || !collector.snapshot.Complete || !collector.snapshot.Scope.Staging {
		t.Fatal("valid exact-scope staging score was disabled", err)
	}
	settings.AutomationPolicy.Staging = false
	if err := collector.refresh(t.Context(), settings); err == nil || collector.snapshot.Scope.Staging || len(collector.snapshot.Points) != 0 || collector.snapshot.Ready {
		t.Fatal("production feed reused staging data", err)
	}
	if !strings.Contains(competitionSubmissionScoreScopeSql, "staging=$4") || strings.Contains(competitionSubmissionScoreScopeSql, "staging=false") {
		t.Fatal("source lookup does not bind the explicit configured era")
	}
}

// The wire contract is exactly the requested operator-only gauge families,
// bounded fixed statistics and numeric submission-time label, with no raw JSON.
func TestCompetitionSubmissionMetricCollectorExportsExactContract(t *testing.T) {
	settings, snapshot, score := competitionSubmissionMetricFixture(t)
	point, err := competitionSubmissionMetricPointFromScore(snapshot.Scope, server.RequireIdFromBytes(bytes.Repeat([]byte{3}, 16)), snapshot.SourceAt, score)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Points, snapshot.Rows = []competitionSubmissionMetricPoint{point}, 1
	limits := defaultCompetitionSubmissionMetricSettings()
	limits.MaximumAge = 95 * time.Second
	collector := newCompetitionSubmissionMetricCollector(limits, func(context.Context, *Settings, competitionSubmissionMetricSettings) (competitionSubmissionMetricSnapshot, error) {
		return snapshot, nil
	})
	registry := prometheus.NewRegistry()
	registry.MustRegister(collector)
	if err := collector.refresh(t.Context(), settings); err != nil {
		t.Fatal(err)
	}
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	statistics := map[string]bool{}
	wantValues := map[string]float64{"median_raw_ms": 80, "baseline_median_raw_ms": 100, "practical_median_max_ms": 88,
		"mean_raw_ms": 80, "baseline_mean_raw_ms": 100, "significant_mean_max_ms": 98, "mean_improvement_percent": 20,
		"minimum_significant_improvement_percent": 2, "required_improvement_percent": 12, "takeover_margin_percent": 12,
		"one_sided_p_value": .01, "alpha": .05, "zero": 0}
	wantMetadata := map[string]float64{"ready": 1, "complete": 1, "timestamp_seconds": float64(snapshot.SourceAt.Unix()), "max_age_seconds": 95,
		"points": 1, "rows": 1, "unplottable_failed": 0}
	metadataCount := 0
	for _, family := range families {
		if family.GetName() == "urnetwork_competition_submission_score_value" {
			if len(family.Metric) != 13 {
				t.Fatal("score metric has missing/extra statistics")
			}
			for _, metric := range family.Metric {
				labels := map[string]string{}
				for _, label := range metric.Label {
					labels[label.GetName()] = label.GetValue()
				}
				if len(labels) != 8 || labels["submitted_at_ms"] != strconv.FormatInt(point.SubmittedAt.UnixMilli(), 10) || labels["classification"] != "eligible" {
					t.Fatal("score metric changed its bounded identity/time/class labels", labels)
				}
				value, present := wantValues[labels["statistic"]]
				if !present || metric.Gauge.GetValue() != value || labels["competition_id"] != settings.CompetitionId || labels["season_id"] != snapshot.Scope.SeasonId ||
					labels["round_id"] != snapshot.Scope.RoundId.String() || labels["epoch"] != "1" || labels["job_id"] != point.JobId.String() {
					t.Fatal("score metric changed a stored value or scope label", labels)
				}
				statistics[labels["statistic"]] = true
			}
		} else {
			metadataCount++
			if !strings.HasPrefix(family.GetName(), "urnetwork_competition_submission_score_snapshot_") || len(family.Metric) != 1 || len(family.Metric[0].Label) != 4 {
				t.Fatal("unexpected score snapshot family")
			}
			value, present := wantMetadata[strings.TrimPrefix(family.GetName(), "urnetwork_competition_submission_score_snapshot_")]
			if !present || family.Metric[0].Gauge.GetValue() != value {
				t.Fatal("snapshot coverage/time/age was not sourced from the atomic component snapshot")
			}
		}
	}
	if metadataCount != 7 || len(statistics) != 13 || !statistics["significant_mean_max_ms"] || !statistics["practical_median_max_ms"] || !statistics["zero"] {
		t.Fatal("operator metric contract is incomplete")
	}
}

// Same-round baseline provenance and complete supplied statistics are required;
// a missing or malformed successful result cannot quietly become a point.
func TestCompetitionSubmissionMetricProjectionRejectsForgedOrIncompleteScores(t *testing.T) {
	for index, mutate := range []func(*ScoreResult){
		func(score *ScoreResult) { score.RawScore = nil },
		func(score *ScoreResult) { score.Significance = nil },
		func(score *ScoreResult) { *score.RawScore = math.Inf(1) },
		func(score *ScoreResult) { score.Diagnostics["round_id"] = "synthetic-foreign-round" },
		func(score *ScoreResult) { score.Diagnostics["baseline_sha256"] = "synthetic-invalid-digest" },
		func(score *ScoreResult) { score.Diagnostics["baseline"] = nil },
		func(score *ScoreResult) { score.Diagnostics["baseline_takeover_raw_max"] = 99.0 },
		func(score *ScoreResult) { score.Diagnostics["takeover_margin"] = .13 },
		func(score *ScoreResult) { score.Significance.OneSidedPValue = nil },
		func(score *ScoreResult) { delete(score.Gates, "G1_success") },
	} {
		_, snapshot, score := competitionSubmissionMetricFixture(t)
		mutate(score)
		if _, err := competitionSubmissionMetricPointFromScore(snapshot.Scope, server.RequireIdFromBytes(bytes.Repeat([]byte{3}, 16)), snapshot.SourceAt, score); err == nil {
			t.Fatal("invalid stored operator projection accepted", index)
		}
	}
}

// Query shape matches the new partial index and never selects full score,
// policy, error or artifact objects. No aggregate count or hidden value scan.
func TestCompetitionSubmissionMetricQueryUsesBoundedFixedScalarIndexPath(t *testing.T) {
	for _, required := range []string{"j.round_id=$1", "j.state IN ('succeeded','failed') AND j.score_json IS NOT NULL", "ORDER BY j.job_id LIMIT $2", "b.round_id=j.round_id", "jsonb_build_object("} {
		if !strings.Contains(competitionSubmissionScoreRowsSql, required) {
			t.Fatal("operator metric query lost indexed/scalar admission", required)
		}
	}
	for _, forbidden := range []string{"SELECT *", "count(", "jsonb_each", "seed_nonce", "seed_ciphertext", "patch_bytes", "artifact_manifest_json", "j.score_json,", "j.eval_error_json,"} {
		if strings.Contains(competitionSubmissionScoreRowsSql, forbidden) {
			t.Fatal("operator metric query includes unbounded/private payload", forbidden)
		}
	}
}
