package competition

import (
	"context"
	"errors"
	"math"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/server"
)

const runnerHeartbeatInterval = 15 * time.Second

var (
	competitionConfigured = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "competition", Name: "configured",
		Help: "1 when the process loaded a valid enabled competition configuration.",
	})
	competitionJobs = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "competition", Name: "jobs",
		Help: "Durable competition jobs by state.",
	}, []string{"state"})
	competitionOldestJobAge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "competition", Name: "oldest_job_age_seconds",
		Help: "Age in seconds of the oldest competition job in each state.",
	}, []string{"state"})
	competitionWorkerHeartbeatAge = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "competition", Name: "worker_heartbeat_age_seconds",
		Help: "Age of the singleton worker-slot heartbeat, or -1 when absent.",
	})
	competitionRunnerHeartbeatTimestamp = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "competition", Name: "runner_heartbeat_timestamp_seconds",
		Help: "Unix timestamp of the latest heartbeat emitted by the sim-latency runner process.",
	})
	competitionSubmissionQueueSize = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "competition", Name: "submission_queue_size",
		Help: "Number of submissions waiting in the durable FIFO queue.",
	})
	competitionCurrentEvaluationInfo = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "competition", Name: "current_evaluation_info",
		Help: "Identity of the submission currently being evaluated; the value is always one.",
	}, []string{"job_id", "round_id", "patch_sha256", "attempt"})
	competitionCurrentEvaluationElapsed = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "competition", Name: "current_evaluation_elapsed_seconds",
		Help: "Elapsed wall time of the submission currently being evaluated, or zero when idle.",
	})
	competitionSignificantSubmissionFound = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "competition", Name: "significant_submission_found",
		Help: "1 when any completed submission in the latest epoch is statistically significant.",
	})
	competitionEvaluationDurationEstimate = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "competition", Name: "evaluation_duration_estimate_seconds",
		Help: "Estimated duration of one submission from the recent p75, falling back to the evaluation time limit.",
	})
	competitionSubmissionBacklogEstimate = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "competition", Name: "submission_backlog_estimated_seconds",
		Help: "Estimated wall time until the running submission and durable FIFO queue are drained.",
	})
	competitionCurrentEpoch = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "competition", Name: "current_epoch",
		Help: "Latest durable epoch number, or zero before the first epoch.",
	})
	competitionRoundPhase = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "competition", Name: "round_phase",
		Help: "One-hot phase of the latest competition epoch.",
	}, []string{"phase"})
	competitionArtifactArchiveReady = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "competition", Name: "artifact_archive_ready",
		Help: "1 when the retained blob backend proves versioning and object-lock readiness.",
	})
	competitionMetricRefreshErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "urnetwork", Subsystem: "competition", Name: "metric_refresh_errors_total",
		Help: "Operational metric refresh failures.",
	})
	competitionSubmissions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "urnetwork", Subsystem: "competition", Name: "submissions_total",
		Help: "Score submission requests by bounded outcome.",
	}, []string{"outcome"})
	competitionEvaluations = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "urnetwork", Subsystem: "competition", Name: "evaluations_total",
		Help: "Evaluator attempts by terminal or retry outcome.",
	}, []string{"outcome"})
	competitionEvaluationSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "urnetwork", Subsystem: "competition", Name: "evaluation_seconds",
		Help:    "Wall time of one evaluator attempt.",
		Buckets: []float64{60, 300, 900, 3600, 7200, 14400, 28800, 57600},
	})
	competitionRoundEvents = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "urnetwork", Subsystem: "competition", Name: "round_events_total",
		Help: "Epoch lifecycle events.",
	}, []string{"event"})
	competitionArtifactObjects = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "urnetwork", Subsystem: "competition", Name: "artifact_objects_total",
		Help: "Authenticated retained competition objects, excluding the enclosing manifest.",
	})
	competitionArtifactBytes = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "urnetwork", Subsystem: "competition", Name: "artifact_bytes_total",
		Help: "Authenticated retained competition artifact bytes, excluding the enclosing manifest.",
	})
	competitionArtifactFailures = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "urnetwork", Subsystem: "competition", Name: "artifact_failures_total",
		Help: "Retained artifact upload or post-upload authentication failures.",
	})
)

func init() {
	prometheus.MustRegister(
		competitionConfigured,
		competitionJobs,
		competitionOldestJobAge,
		competitionWorkerHeartbeatAge,
		competitionRunnerHeartbeatTimestamp,
		competitionSubmissionQueueSize,
		competitionCurrentEvaluationInfo,
		competitionCurrentEvaluationElapsed,
		competitionSignificantSubmissionFound,
		competitionEvaluationDurationEstimate,
		competitionSubmissionBacklogEstimate,
		competitionCurrentEpoch,
		competitionRoundPhase,
		competitionArtifactArchiveReady,
		competitionMetricRefreshErrors,
		competitionSubmissions,
		competitionEvaluations,
		competitionEvaluationSeconds,
		competitionRoundEvents,
		competitionArtifactObjects,
		competitionArtifactBytes,
		competitionArtifactFailures,
	)
}

// StartRunnerHeartbeat emits the runner-process liveness signal immediately
// and every 15 seconds until ctx is canceled. Start this only in the dedicated
// competition worker; API processes deliberately leave the signal untouched.
func StartRunnerHeartbeat(ctx context.Context) {
	recordRunnerHeartbeat(server.NowUtc())
	go func() {
		ticker := time.NewTicker(runnerHeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				recordRunnerHeartbeat(now.UTC())
			}
		}
	}()
}

func recordRunnerHeartbeat(now time.Time) {
	competitionRunnerHeartbeatTimestamp.Set(float64(now.UnixNano()) / float64(time.Second))
}

// StartMetrics refreshes database-derived gauges for the existing main Grafana
// pipeline. Counters are updated synchronously at their durable boundaries.
// An unconfigured competition leaves a truthful configured=0 and no goroutine.
func StartMetrics(ctx context.Context) {
	settings, err := DefaultService().Settings()
	if err != nil {
		competitionConfigured.Set(0)
		return
	}
	competitionConfigured.Set(1)
	refresh := func() {
		if err := refreshOperationalMetrics(ctx, settings); err != nil {
			competitionMetricRefreshErrors.Inc()
			glog.Infof("[competition]metric refresh failed: %s\n", err)
		}
	}
	refresh()
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				refresh()
			}
		}
	}()
}

func refreshOperationalMetrics(ctx context.Context, settings *Settings) error {
	now := server.NowUtc()
	queuedCount := 0
	reviewPending := false
	currentEvaluationElapsed := 0.0
	evaluationDurationEstimate := float64(settings.EvaluationPolicy.ScoreTimeoutSeconds)
	for _, state := range []string{"queued", "running", "succeeded", "failed", "canceled"} {
		competitionJobs.WithLabelValues(state).Set(0)
		competitionOldestJobAge.WithLabelValues(state).Set(0)
	}
	competitionWorkerHeartbeatAge.Set(-1)
	competitionSubmissionQueueSize.Set(0)
	competitionCurrentEvaluationInfo.Reset()
	competitionCurrentEvaluationElapsed.Set(0)
	competitionSignificantSubmissionFound.Set(0)
	competitionEvaluationDurationEstimate.Set(evaluationDurationEstimate)
	competitionSubmissionBacklogEstimate.Set(0)
	err := captureDatabaseError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			rows, queryErr := conn.Query(ctx, `
				SELECT job.state, count(*),
				       COALESCE(extract(epoch FROM ($2::timestamp - min(job.submitted_at))), 0)
				FROM competition_job AS job
				JOIN competition_round AS round ON round.round_id = job.round_id
				WHERE round.competition_id = $1
				GROUP BY job.state
			`, settings.CompetitionId, now)
			server.WithPgResult(rows, queryErr, func() {
				for rows.Next() {
					var state string
					var count int
					var age float64
					server.Raise(rows.Scan(&state, &count, &age))
					competitionJobs.WithLabelValues(state).Set(float64(count))
					competitionOldestJobAge.WithLabelValues(state).Set(age)
					if state == "queued" {
						queuedCount = count
					}
				}
			})
			var heartbeat *time.Time
			server.Raise(conn.QueryRow(ctx, `
				SELECT heartbeat_at FROM competition_worker_slot WHERE slot_id = 1
			`).Scan(&heartbeat))
			if heartbeat != nil {
				competitionWorkerHeartbeatAge.Set(math.Max(0, now.Sub(*heartbeat).Seconds()))
			}
			server.Raise(conn.QueryRow(ctx, `
				SELECT EXISTS (
					SELECT 1 FROM competition_round AS round
					WHERE round.competition_id = $1 AND round.canceled = false
					  AND round.epoch_number = (
					      SELECT max(latest.epoch_number)
					      FROM competition_round AS latest
					      WHERE latest.competition_id = $1 AND latest.canceled = false
					  )
					  AND round.closes_at <= $2 AND round.finalized_at IS NULL
					  AND NOT EXISTS (
					      SELECT 1 FROM competition_job AS active
					      WHERE active.round_id = round.round_id
					        AND active.state IN ('queued', 'running')
					  )
				)
			`, settings.CompetitionId, now).Scan(&reviewPending))

			var jobId string
			var roundId string
			var patchSha256 string
			var attempt int
			var startedAt time.Time
			scanErr := conn.QueryRow(ctx, `
				SELECT job.job_id::text, job.round_id::text, job.patch_sha256,
				       job.attempt_count, job.started_at
				FROM competition_job AS job
				JOIN competition_round AS round ON round.round_id = job.round_id
				WHERE round.competition_id = $1
				  AND job.state = 'running'
				  AND job.started_at IS NOT NULL
				ORDER BY job.started_at, job.job_id
				LIMIT 1
			`, settings.CompetitionId).Scan(
				&jobId, &roundId, &patchSha256, &attempt, &startedAt,
			)
			if scanErr == nil {
				currentEvaluationElapsed = math.Max(0, now.Sub(startedAt).Seconds())
				competitionCurrentEvaluationInfo.WithLabelValues(
					jobId, roundId, patchSha256, strconv.Itoa(attempt),
				).Set(1)
			} else if !errors.Is(scanErr, pgx.ErrNoRows) {
				server.Raise(scanErr)
			}

			var significant bool
			server.Raise(conn.QueryRow(ctx, `
				SELECT EXISTS (
					SELECT 1
					FROM competition_job AS job
					JOIN competition_round AS round ON round.round_id = job.round_id
					WHERE round.competition_id = $1
					  AND round.epoch_number = (
					      SELECT max(latest.epoch_number)
					      FROM competition_round AS latest
					      WHERE latest.competition_id = $1
					        AND latest.canceled = false
					  )
					  AND job.state = 'succeeded'
					  AND job.score_json @> '{"significance":{"statistically_significant":true}}'::jsonb
				)
			`, settings.CompetitionId).Scan(&significant))
			if significant {
				competitionSignificantSubmissionFound.Set(1)
			}

			server.Raise(conn.QueryRow(ctx, `
				SELECT COALESCE(
					percentile_cont(0.75) WITHIN GROUP (ORDER BY recent.duration_seconds),
					$2::double precision
				)
				FROM (
					SELECT extract(epoch FROM (job.completed_at - job.started_at))::double precision AS duration_seconds
					FROM competition_job AS job
					JOIN competition_round AS round ON round.round_id = job.round_id
					WHERE round.competition_id = $1
					  AND job.state = 'succeeded'
					  AND job.started_at IS NOT NULL
					  AND job.completed_at >= job.started_at
					ORDER BY job.completed_at DESC
					LIMIT 20
				) AS recent
			`, settings.CompetitionId, evaluationDurationEstimate).Scan(&evaluationDurationEstimate))
		})
	})
	if err != nil {
		return err
	}
	competitionSubmissionQueueSize.Set(float64(queuedCount))
	competitionCurrentEvaluationElapsed.Set(currentEvaluationElapsed)
	competitionEvaluationDurationEstimate.Set(evaluationDurationEstimate)
	backlogEstimate := float64(queuedCount) * evaluationDurationEstimate
	if 0 < currentEvaluationElapsed {
		backlogEstimate += math.Max(0, evaluationDurationEstimate-currentEvaluationElapsed)
	}
	competitionSubmissionBacklogEstimate.Set(backlogEstimate)
	for _, phase := range []string{"none", "scheduled", "open", "grading", "review", "finalized", "canceled"} {
		competitionRoundPhase.WithLabelValues(phase).Set(0)
	}
	round, err := (PostgresStore{}).CurrentRound(ctx, settings)
	if err != nil {
		return err
	}
	if round == nil {
		competitionCurrentEpoch.Set(0)
		competitionRoundPhase.WithLabelValues("none").Set(1)
	} else {
		competitionCurrentEpoch.Set(float64(round.Epoch))
		phase := round.Status
		if phase == "grading" && reviewPending {
			phase = "review"
		}
		competitionRoundPhase.WithLabelValues(phase).Set(1)
	}
	archiveReady := settings.artifactArchive != nil && settings.artifactArchive.Check(ctx) == nil
	if archiveReady {
		competitionArtifactArchiveReady.Set(1)
	} else {
		competitionArtifactArchiveReady.Set(0)
	}
	return nil
}
