package competition

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/server"
)

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
	for _, state := range []string{"queued", "running", "succeeded", "failed", "canceled"} {
		competitionJobs.WithLabelValues(state).Set(0)
		competitionOldestJobAge.WithLabelValues(state).Set(0)
	}
	competitionWorkerHeartbeatAge.Set(-1)
	err := captureDatabaseError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			rows, queryErr := conn.Query(ctx, `
				SELECT job.state, count(*),
				       COALESCE(extract(epoch FROM ($2::timestamp - min(job.submitted_at))), 0)
				FROM competition_job AS job
				JOIN competition_round AS round ON round.round_id = job.round_id
				WHERE round.competition_id = $1
				GROUP BY job.state
			`, settings.CompetitionId, server.NowUtc())
			server.WithPgResult(rows, queryErr, func() {
				for rows.Next() {
					var state string
					var count int
					var age float64
					server.Raise(rows.Scan(&state, &count, &age))
					competitionJobs.WithLabelValues(state).Set(float64(count))
					competitionOldestJobAge.WithLabelValues(state).Set(age)
				}
			})
			var heartbeat *time.Time
			server.Raise(conn.QueryRow(ctx, `
				SELECT heartbeat_at FROM competition_worker_slot WHERE slot_id = 1
			`).Scan(&heartbeat))
			if heartbeat != nil {
				competitionWorkerHeartbeatAge.Set(server.NowUtc().Sub(*heartbeat).Seconds())
			}
		})
	})
	if err != nil {
		return err
	}
	for _, phase := range []string{"none", "scheduled", "open", "grading", "finalized", "canceled"} {
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
		competitionRoundPhase.WithLabelValues(round.Status).Set(1)
	}
	archiveReady := settings.artifactArchive != nil && settings.artifactArchive.Check(ctx) == nil
	if archiveReady {
		competitionArtifactArchiveReady.Set(1)
	} else {
		competitionArtifactArchiveReady.Set(0)
	}
	return nil
}
