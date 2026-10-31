// Internal operator-only score plots read immutable stored summaries, never
// evaluator artifacts or public feedback routes. A bounded snapshot is atomic
// across points and coverage; incomplete refreshes cannot look like all jobs.
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/server"
)

// Budgets belong to this projection, not grading or the frozen score policy.
type competitionSubmissionMetricSettings struct {
	MaximumPoints          int           // Positive complete-epoch point cap; overflow refuses the whole new snapshot.
	MaximumRows            int           // Positive indexed scored-row cap, including incomplete failed reports.
	MaximumProjectionBytes int           // Positive bound on one fixed-scalar projection, never a full stored score.
	QueryTimeout           time.Duration // Positive total read-only snapshot deadline.
	RefreshInterval        time.Duration // Positive operator metric refresh cadence.
	MaximumAge             time.Duration // Positive query freshness gate, exported rather than copied into dashboards.
}

// The new feed shares the existing fifteen-second metrics owner cadence.
func defaultCompetitionSubmissionMetricSettings() competitionSubmissionMetricSettings {
	return competitionSubmissionMetricSettings{MaximumPoints: 2048, MaximumRows: 4096, MaximumProjectionBytes: 8 * 1024,
		QueryTimeout: 5 * time.Second, RefreshInterval: 15 * time.Second, MaximumAge: time.Minute}
}

// Environment is an internal cache boundary; existing scrape labels carry it.
type competitionSubmissionMetricScope struct {
	CompetitionId string
	SeasonId      string
	Environment   string
	Staging       bool
	RoundId       server.Id
	Epoch         int
	Margin        float64
}

// Only fixed numeric values and already recorded outcome classes reach metrics.
type competitionSubmissionMetricPoint struct {
	JobId          server.Id
	SubmittedAt    time.Time
	Classification string
	Values         [13]float64
}

// SourceAt is the actual database snapshot time, not the last score's age.
type competitionSubmissionMetricSnapshot struct {
	Scope             competitionSubmissionMetricScope
	SourceAt          time.Time
	Points            []competitionSubmissionMetricPoint
	Rows              int
	UnplottableFailed int
	Ready             bool
	Complete          bool
}

// One metrics owner refreshes serially; concurrent scrapes consume one atomic
// point/coverage generation. Each instance owns its own projection budget.
type competitionSubmissionMetricCollector struct {
	settings  competitionSubmissionMetricSettings
	fetch     func(context.Context, *Settings, competitionSubmissionMetricSettings) (competitionSubmissionMetricSnapshot, error)
	stateLock sync.RWMutex
	snapshot  competitionSubmissionMetricSnapshot
	value     *prometheus.Desc
	metadata  map[string]*prometheus.Desc
}

// Registration and scheduling remain with StartMetrics; tests use a private registry.
func newCompetitionSubmissionMetricCollector(settings competitionSubmissionMetricSettings,
	fetch func(context.Context, *Settings, competitionSubmissionMetricSettings) (competitionSubmissionMetricSnapshot, error)) *competitionSubmissionMetricCollector {
	labels := []string{"competition_id", "season_id", "round_id", "epoch"}
	metadata := map[string]*prometheus.Desc{
		"ready": prometheus.NewDesc("urnetwork_competition_submission_score_snapshot_ready",
			"1 only when the last scoped fixed-summary fetch succeeded; query together with complete and source age.", labels, nil),
		"complete": prometheus.NewDesc("urnetwork_competition_submission_score_snapshot_complete",
			"1 only when every scored completed job in the selected epoch fits the bounded snapshot; never latest N.", labels, nil),
		"timestamp_seconds": prometheus.NewDesc("urnetwork_competition_submission_score_snapshot_timestamp_seconds",
			"Actual database snapshot Unix time of the retained point roster; unchanged on failed refresh.", labels, nil),
		"max_age_seconds": prometheus.NewDesc("urnetwork_competition_submission_score_snapshot_max_age_seconds",
			"Settings-owned maximum acceptable snapshot age; gate per producer before deduplicating hosts.", labels, nil),
		"points": prometheus.NewDesc("urnetwork_competition_submission_score_snapshot_points",
			"Valid plotted numeric records in the retained roster; coverage is unavailable when ready or complete is zero.", labels, nil),
		"rows": prometheus.NewDesc("urnetwork_competition_submission_score_snapshot_rows",
			"All indexed terminal records with a non-SQL-NULL stored score in the complete roster, including incomplete failed reports.", labels, nil),
		"unplottable_failed": prometheus.NewDesc("urnetwork_competition_submission_score_snapshot_unplottable_failed",
			"Failed stored reports without complete positive numeric inferential statistics; never emitted as fabricated zero points.", labels, nil),
	}
	return &competitionSubmissionMetricCollector{settings: settings, fetch: fetch, metadata: metadata,
		value: prometheus.NewDesc("urnetwork_competition_submission_score_value",
			"Operator-only immutable stored score summary against its own epoch baseline; not public publication or new scoring.",
			[]string{"competition_id", "season_id", "round_id", "epoch", "job_id", "submitted_at_ms", "statistic", "classification"}, nil)}
}

// A static descriptor set permits ordinary authenticated metrics registration.
func (self *competitionSubmissionMetricCollector) Describe(descriptors chan<- *prometheus.Desc) {
	descriptors <- self.value
	for _, descriptor := range self.metadata {
		descriptors <- descriptor
	}
}

// Point and metadata generation cannot be split by an overlapping refresh.
func (self *competitionSubmissionMetricCollector) Collect(metrics chan<- prometheus.Metric) {
	self.stateLock.RLock()
	defer self.stateLock.RUnlock()
	snapshot := &self.snapshot
	labels := []string{snapshot.Scope.CompetitionId, snapshot.Scope.SeasonId, snapshot.Scope.RoundId.String(), strconv.Itoa(snapshot.Scope.Epoch)}
	ready, complete, timestamp := 0.0, 0.0, 0.0
	if snapshot.Ready {
		ready = 1
	}
	if snapshot.Complete {
		complete = 1
	}
	if !snapshot.SourceAt.IsZero() {
		timestamp = float64(snapshot.SourceAt.UnixNano()) / float64(time.Second)
	}
	for name, value := range map[string]float64{"ready": ready, "complete": complete, "timestamp_seconds": timestamp,
		"max_age_seconds": self.settings.MaximumAge.Seconds(), "points": float64(len(snapshot.Points)),
		"rows": float64(snapshot.Rows), "unplottable_failed": float64(snapshot.UnplottableFailed)} {
		metrics <- prometheus.MustNewConstMetric(self.metadata[name], prometheus.GaugeValue, value, labels...)
	}
	for _, point := range snapshot.Points {
		for index, statistic := range []string{"median_raw_ms", "baseline_median_raw_ms", "practical_median_max_ms", "mean_raw_ms", "baseline_mean_raw_ms",
			"significant_mean_max_ms", "mean_improvement_percent", "minimum_significant_improvement_percent", "required_improvement_percent",
			"takeover_margin_percent", "one_sided_p_value", "alpha", "zero"} {
			pointLabels := append(append([]string{}, labels...), point.JobId.String(), strconv.FormatInt(point.SubmittedAt.UnixMilli(), 10), statistic, point.Classification)
			metrics <- prometheus.MustNewConstMetric(self.value, prometheus.GaugeValue, point.Values[index], pointLabels...)
		}
	}
}

// Failed same-scope reads retain evidence but withdraw readiness/completeness.
// Epoch/config changes first retire the old roster, including when the new read fails.
func (self *competitionSubmissionMetricCollector) refresh(ctx context.Context, settings *Settings) error {
	base := competitionSubmissionMetricScope{}
	if settings != nil {
		base.CompetitionId, base.SeasonId, base.Environment, base.Staging = settings.CompetitionId, settings.AutomationPolicy.SeasonId, settings.AutomationPolicy.Environment, settings.AutomationPolicy.Staging
	}
	self.stateLock.Lock()
	if self.snapshot.Scope.CompetitionId != base.CompetitionId || self.snapshot.Scope.SeasonId != base.SeasonId || self.snapshot.Scope.Environment != base.Environment || self.snapshot.Scope.Staging != base.Staging {
		self.snapshot = competitionSubmissionMetricSnapshot{Scope: base}
	}
	self.stateLock.Unlock()
	if settings == nil || !settings.Enabled || !settings.AutomationPolicy.Enabled || self.fetch == nil ||
		self.settings.MaximumPoints <= 0 || self.settings.MaximumRows < self.settings.MaximumPoints || self.settings.MaximumRows == int(^uint(0)>>1) || self.settings.MaximumProjectionBytes <= 0 ||
		self.settings.QueryTimeout <= 0 || self.settings.RefreshInterval <= 0 || self.settings.MaximumAge < self.settings.RefreshInterval {
		self.stateLock.Lock()
		self.snapshot = competitionSubmissionMetricSnapshot{Scope: base}
		self.stateLock.Unlock()
		return errors.New("operator score snapshot is not configured for this bounded automation scope")
	}
	queryCtx, cancel := context.WithTimeout(ctx, self.settings.QueryTimeout)
	defer cancel()
	next := competitionSubmissionMetricSnapshot{Scope: base}
	err := queryCtx.Err()
	if err == nil {
		next, err = self.fetch(queryCtx, settings, self.settings)
	}
	if err == nil {
		err = queryCtx.Err()
	}
	if err == nil && (next.Scope.CompetitionId != base.CompetitionId || next.Scope.SeasonId != base.SeasonId || next.Scope.Environment != base.Environment || next.Scope.Staging != base.Staging ||
		next.Scope.RoundId == (server.Id{}) || next.Scope.Epoch < 1 || next.SourceAt.IsZero() || len(next.Points) > self.settings.MaximumPoints ||
		next.Rows > self.settings.MaximumRows || next.Rows != len(next.Points)+next.UnplottableFailed) {
		err = errors.New("operator score snapshot is incomplete or outside its configured scope")
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if next.Scope.CompetitionId == base.CompetitionId && next.Scope.SeasonId == base.SeasonId && next.Scope.Environment == base.Environment && next.Scope.Staging == base.Staging &&
		next.Scope.RoundId != (server.Id{}) && next.Scope.Epoch > 0 && next.Scope != self.snapshot.Scope {
		self.snapshot = competitionSubmissionMetricSnapshot{Scope: next.Scope}
	}
	if err != nil {
		self.snapshot.Ready, self.snapshot.Complete = false, false
		return errors.New("operator score snapshot unavailable or incomplete")
	}
	next.Ready, next.Complete = true, true
	self.snapshot = next
	return nil
}

// Only fixed scalar fields are decoded. Free-form diagnostics and evidence
// never enter this process's score-history projection or metric labels.
func competitionSubmissionMetricPointFromScore(scope competitionSubmissionMetricScope, jobId server.Id, submittedAt time.Time, score *ScoreResult) (competitionSubmissionMetricPoint, error) {
	point := competitionSubmissionMetricPoint{JobId: jobId, SubmittedAt: submittedAt}
	if jobId == (server.Id{}) || submittedAt.IsZero() || validateScore(score) != nil {
		return point, errors.New("operator score projection has no valid stored numeric contract")
	}
	baseline, ok := score.Diagnostics["baseline"].(map[string]any)
	if !ok {
		return point, errors.New("operator score projection is missing its same-epoch baseline")
	}
	baselineRaw, baselineOk := baseline["raw_score"].(float64)
	margin, marginOk := score.Diagnostics["takeover_margin"].(float64)
	practicalMax, practicalOk := score.Diagnostics["baseline_takeover_raw_max"].(float64)
	takeover, takeoverOk := score.Diagnostics["baseline_takeover_eligible"].(bool)
	roundId, roundOk := score.Diagnostics["round_id"].(string)
	baselineDigest, digestOk := score.Diagnostics["baseline_sha256"].(string)
	significance := score.Significance
	if !baselineOk || !marginOk || !practicalOk || !takeoverOk || !roundOk || roundId != scope.RoundId.String() || !digestOk || !sha256Pattern.MatchString(baselineDigest) ||
		!competitionSubmissionMetricFinite(baselineRaw) || baselineRaw <= 0 || !competitionSubmissionMetricFinite(margin) || !competitionSubmissionMetricFinite(scope.Margin) || math.Abs(margin-scope.Margin) > 1e-9 ||
		math.Abs(significance.TakeoverMarginPercent-margin*100) > 1e-9 || !competitionSubmissionMetricFinite(practicalMax) ||
		math.Abs(practicalMax-baselineRaw*(1-margin)) > math.Max(1, baselineRaw)*1e-9 || significance.MinimumSignificantImprovementPercent == nil ||
		significance.RequiredImprovementPercent == nil || significance.OneSidedPValue == nil {
		return point, errors.New("operator score projection lost its exact epoch baseline or recorded thresholds")
	}
	allGates := true
	for _, name := range []string{"G1_success", "G2_volume", "G3_path_integrity", "G4_matchmaking", "G5_stability", "G6_resources"} {
		gate, present := score.Gates[name]
		if !present {
			return point, errors.New("operator score projection is missing a recorded gate")
		}
		allGates = allGates && gate.Passed
	}
	point.Classification = "not_significant"
	if !score.Placeable || !allGates {
		point.Classification = "gate_failed"
	} else if takeover {
		if !significance.StatisticallySignificant || !significance.RecommendedNextEpochTakeoverMarginSupported {
			return point, errors.New("operator score projection contains contradictory recorded eligibility")
		}
		point.Classification = "eligible"
	} else if significance.StatisticallySignificant {
		point.Classification = "significant_only"
	}
	point.Values = [13]float64{*score.RawScore, baselineRaw, practicalMax, significance.CandidateMeanRawScore, significance.BaselineMeanRawScore,
		significance.BaselineMeanRawScore * (1 - *significance.MinimumSignificantImprovementPercent/100), significance.ObservedImprovementPercent,
		*significance.MinimumSignificantImprovementPercent, *significance.RequiredImprovementPercent, significance.TakeoverMarginPercent,
		*significance.OneSidedPValue, significance.Alpha, 0}
	for _, value := range point.Values {
		if !competitionSubmissionMetricFinite(value) {
			return point, errors.New("operator score projection has a non-finite display value")
		}
	}
	return point, nil
}

// Non-finite values cannot become Prometheus points or threshold fringes.
func competitionSubmissionMetricFinite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

// Failed early scorer reports legitimately lack statistics. Account for them
// explicitly while refusing invalid complete records, budget overflow or drift.
func appendCompetitionSubmissionMetricProjection(snapshot *competitionSubmissionMetricSnapshot, limits competitionSubmissionMetricSettings,
	jobId server.Id, submittedAt time.Time, state string, content []byte, valid bool) error {
	if snapshot.Rows >= limits.MaximumRows || jobId == (server.Id{}) || submittedAt.IsZero() || (state != "succeeded" && state != "failed") || len(content) == 0 || len(content) > limits.MaximumProjectionBytes {
		return errors.New("operator score indexed projection exceeded its bounded shape")
	}
	snapshot.Rows++
	var score ScoreResult
	if json.Unmarshal(content, &score) != nil {
		return errors.New("operator score fixed projection is malformed")
	}
	if state == "failed" && (score.RawScore == nil || !competitionSubmissionMetricFinite(*score.RawScore) || *score.RawScore <= 0 || score.Significance == nil ||
		score.Significance.ReplicateCount < 2 || score.Significance.MinimumSignificantImprovementPercent == nil || score.Significance.RequiredImprovementPercent == nil || score.Significance.OneSidedPValue == nil) {
		snapshot.UnplottableFailed++
		return nil
	}
	if !valid {
		return errors.New("operator score projection lost recorded gates, scope or source policy")
	}
	point, err := competitionSubmissionMetricPointFromScore(snapshot.Scope, jobId, submittedAt, &score)
	if err != nil {
		return err
	}
	if len(snapshot.Points) >= limits.MaximumPoints {
		return errors.New("operator score point budget exceeded; no partial snapshot is publishable")
	}
	if len(snapshot.Points) > 0 && (point.Values[1] != snapshot.Points[0].Values[1] || point.Values[4] != snapshot.Points[0].Values[4]) {
		return errors.New("operator score snapshot contains more than one same-epoch baseline")
	}
	snapshot.Points = append(snapshot.Points, point)
	return nil
}

// One repeatable-read transaction owns the selected round and every summary.
// The partial index admits only scored terminal rows; max+1 detects overflow.
func fetchCompetitionSubmissionMetricSnapshot(ctx context.Context, settings *Settings, limits competitionSubmissionMetricSettings) (snapshot competitionSubmissionMetricSnapshot, resultErr error) {
	snapshot.Scope = competitionSubmissionMetricScope{CompetitionId: settings.CompetitionId, SeasonId: settings.AutomationPolicy.SeasonId, Environment: settings.AutomationPolicy.Environment, Staging: settings.AutomationPolicy.Staging}
	err := captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `SELECT set_config('statement_timeout',$1,true)`, fmt.Sprintf("%dms", max(1, limits.QueryTimeout.Milliseconds()))))
			server.Raise(tx.QueryRow(ctx, competitionSubmissionScoreScopeSql, settings.CompetitionId, settings.AutomationPolicy.Environment, settings.AutomationPolicy.SeasonId, settings.AutomationPolicy.Staging).
				Scan(&snapshot.Scope.RoundId, &snapshot.Scope.Epoch, &snapshot.SourceAt, &snapshot.Scope.Margin))
			rows, err := tx.Query(ctx, competitionSubmissionScoreRowsSql, snapshot.Scope.RoundId, limits.MaximumRows+1)
			server.Raise(err)
			defer rows.Close()
			for rows.Next() {
				var jobId server.Id
				var submittedAt time.Time
				var state string
				var content []byte
				var valid bool
				server.Raise(rows.Scan(&jobId, &submittedAt, &state, &content, &valid))
				server.Raise(appendCompetitionSubmissionMetricProjection(&snapshot, limits, jobId, submittedAt, state, content, valid))
			}
			server.Raise(rows.Err())
		}, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, server.OptNoRetry())
	})
	if err != nil {
		return snapshot, errors.New("operator score read-only snapshot failed")
	}
	return snapshot, nil
}

// Stage and production are explicit disjoint indexed epoch identities.
const competitionSubmissionScoreScopeSql = `SELECT round_id,epoch_number,transaction_timestamp() AT TIME ZONE 'UTC',
 (policy_json->'evaluation_policy'->>'takeover_margin')::double precision
 FROM competition_round WHERE competition_id=$1 AND staging=$4 AND canceled=false
 AND policy_json->'automation_policy'->>'environment'=$2 AND policy_json->'automation_policy'->>'season_id'=$3
 ORDER BY epoch_number DESC LIMIT 1`

// Casts yield only bounded scalar values, never arbitrary native JSON objects,
// strings, errors, seed/policy payloads, patch bytes or artifact paths.
const competitionSubmissionScoreRowsSql = `SELECT j.job_id,j.submitted_at,j.state,jsonb_build_object(
 'score_schema',(j.score_json->>'score_schema')::integer,
 'raw_score',(j.score_json->>'raw_score')::double precision,
 'normalized_score',(j.score_json->>'normalized_score')::double precision,
 'placeable',(j.score_json->>'placeable')::boolean,
 'takeover_eligible',COALESCE((j.score_json->>'takeover_eligible')::boolean,false),
 'gates',jsonb_build_object(
  'G1_success',jsonb_build_object('passed',(j.score_json#>>'{gates,G1_success,passed}')::boolean,'details','{}'::jsonb),
  'G2_volume',jsonb_build_object('passed',(j.score_json#>>'{gates,G2_volume,passed}')::boolean,'details','{}'::jsonb),
  'G3_path_integrity',jsonb_build_object('passed',(j.score_json#>>'{gates,G3_path_integrity,passed}')::boolean,'details','{}'::jsonb),
  'G4_matchmaking',jsonb_build_object('passed',(j.score_json#>>'{gates,G4_matchmaking,passed}')::boolean,'details','{}'::jsonb),
  'G5_stability',jsonb_build_object('passed',(j.score_json#>>'{gates,G5_stability,passed}')::boolean,'details','{}'::jsonb),
  'G6_resources',jsonb_build_object('passed',(j.score_json#>>'{gates,G6_resources,passed}')::boolean,'details','{}'::jsonb)),
 'significance',jsonb_build_object(
  'method',CASE WHEN j.score_json#>>'{significance,method}'='one-sided-welch-t' THEN 'one-sided-welch-t' ELSE '' END,
  'alpha',(j.score_json#>>'{significance,alpha}')::double precision,
  'replicate_count',(j.score_json#>>'{significance,replicate_count}')::integer,
  'baseline_mean_raw_score',(j.score_json#>>'{significance,baseline_mean_raw_score}')::double precision,
  'candidate_mean_raw_score',(j.score_json#>>'{significance,candidate_mean_raw_score}')::double precision,
  'baseline_sample_variance',(j.score_json#>>'{significance,baseline_sample_variance}')::double precision,
  'candidate_sample_variance',(j.score_json#>>'{significance,candidate_sample_variance}')::double precision,
  'observed_improvement_percent',(j.score_json#>>'{significance,observed_improvement_percent}')::double precision,
  'takeover_margin_percent',(j.score_json#>>'{significance,takeover_margin_percent}')::double precision,
  'minimum_significant_improvement_percent',(j.score_json#>>'{significance,minimum_significant_improvement_percent}')::double precision,
  'required_improvement_percent',(j.score_json#>>'{significance,required_improvement_percent}')::double precision,
  'one_sided_p_value',(j.score_json#>>'{significance,one_sided_p_value}')::double precision,
  'welch_t',(j.score_json#>>'{significance,welch_t}')::double precision,
  'welch_degrees_of_freedom',(j.score_json#>>'{significance,welch_degrees_of_freedom}')::double precision,
  'statistically_significant',(j.score_json#>>'{significance,statistically_significant}')::boolean,
  'next_epoch_minimum_improvement_percent',(j.score_json#>>'{significance,next_epoch_minimum_improvement_percent}')::double precision,
  'recommended_next_epoch_takeover_margin_percent',(j.score_json#>>'{significance,recommended_next_epoch_takeover_margin_percent}')::double precision,
  'recommended_next_epoch_takeover_margin_supported',(j.score_json#>>'{significance,recommended_next_epoch_takeover_margin_supported}')::boolean),
 'diagnostics',jsonb_build_object(
  'round_id',CASE WHEN j.score_json#>>'{diagnostics,round_id}'=j.round_id::text THEN j.round_id::text ELSE '' END,
  'baseline_sha256',CASE WHEN j.score_json#>>'{diagnostics,baseline_sha256}'=b.baseline_sha256 THEN b.baseline_sha256 ELSE '' END,
  'baseline',jsonb_build_object('raw_score',(j.score_json#>>'{diagnostics,baseline,raw_score}')::double precision),
  'takeover_margin',(j.score_json#>>'{diagnostics,takeover_margin}')::double precision,
  'baseline_takeover_raw_max',(j.score_json#>>'{diagnostics,baseline_takeover_raw_max}')::double precision,
  'baseline_takeover_eligible',(j.score_json#>>'{diagnostics,baseline_takeover_eligible}')::boolean))::text,
 COALESCE(jsonb_typeof(j.score_json->'gates')='object'
  AND jsonb_typeof(j.score_json#>'{gates,G1_success,passed}')='boolean'
  AND jsonb_typeof(j.score_json#>'{gates,G2_volume,passed}')='boolean'
  AND jsonb_typeof(j.score_json#>'{gates,G3_path_integrity,passed}')='boolean'
  AND jsonb_typeof(j.score_json#>'{gates,G4_matchmaking,passed}')='boolean'
  AND jsonb_typeof(j.score_json#>'{gates,G5_stability,passed}')='boolean'
  AND jsonb_typeof(j.score_json#>'{gates,G6_resources,passed}')='boolean'
  AND jsonb_typeof(j.score_json#>'{diagnostics,baseline_takeover_eligible}')='boolean'
  AND (j.state='failed' OR NOT COALESCE(jsonb_typeof(j.eval_error_json)<>'null',false))
  AND COALESCE(j.score_json#>>'{diagnostics,g2_volume_policy}','')=COALESCE(r.policy_json#>>'{evaluation_policy,g2_volume_policy}','')
  AND ((NOT (j.score_json->'diagnostics' ? 'g2_volume_policy') AND NOT (j.score_json->'diagnostics' ? 'scorer_version'))
   OR (j.score_json#>>'{diagnostics,g2_volume_policy}'='minimum-work/1' AND j.score_json#>>'{diagnostics,scorer_version}'='sim-latency-score/2')),false)
 FROM competition_job j JOIN competition_round r ON r.round_id=j.round_id
 LEFT JOIN competition_round_baseline b ON b.round_id=j.round_id
 WHERE j.round_id=$1 AND j.state IN ('succeeded','failed') AND j.score_json IS NOT NULL
 ORDER BY j.job_id LIMIT $2`
