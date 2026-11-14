// Per-host slot health exposes bounded executor identities, never private
// candidate ids, scores, patch identities or the hidden round seed.
package controller

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/server/v2026"
)

var (
	competitionExecutorPhysicalAvailable = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "competition", Name: "executor_physical_available",
		Help: "Last authenticated physical admission observation: one available, zero occupied, minus one unknown; inspect its observation time too.",
	}, []string{"executor_host"})
	competitionExecutorPhysicalObservedAt = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "competition", Name: "executor_physical_observed_at_seconds",
		Help: "UTC Unix time of the physical admission check, not a historical qualification or database slot heartbeat.",
	}, []string{"executor_host"})
	competitionExecutorRemoteState = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "competition", Name: "executor_remote_state",
		Help: "Last authenticated run state: zero unknown, one accepted, two running, three succeeded, four failed.",
	}, []string{"executor_host"})
	competitionExecutorTransportFailures = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "urnetwork", Subsystem: "competition", Name: "executor_transport_failures_total",
		Help: "Bounded executor transport failures requiring reconciliation of the same run.",
	}, []string{"executor_host"})
	competitionExecutorBusy = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "competition", Name: "executor_slot_busy",
		Help: "One while a frozen executor slot retains a running or uncertain execution.",
	}, []string{"executor_host"})
	competitionExecutorReconciliationRequired = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "competition", Name: "executor_reconciliation_required",
		Help: "One when retained executor custody expired; the same run must be reconciled before reuse.",
	}, []string{"executor_host"})
	competitionExecutorHeartbeatAge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "competition", Name: "executor_heartbeat_age_seconds",
		Help: "Age of the selected executor's latest host heartbeat, or minus one when absent.",
	}, []string{"executor_host"})
	competitionExecutorDeadlineRemaining = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "competition", Name: "executor_deadline_remaining_seconds",
		Help: "Remaining absolute job budget on each frozen executor; zero when idle or expired.",
	}, []string{"executor_host"})
	competitionExecutorQualified = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "competition", Name: "executor_noop_qualified",
		Help: "One only for an immutable passing no-op receipt against the current shared baseline.",
	}, []string{"executor_host"})
)

// All label values come from a validated immutable policy with at most sixteen
// hosts. The database refresh uses one bounded same-round snapshot.
func init() {
	prometheus.MustRegister(competitionExecutorBusy, competitionExecutorReconciliationRequired,
		competitionExecutorHeartbeatAge, competitionExecutorDeadlineRemaining, competitionExecutorQualified,
		competitionExecutorRemoteState, competitionExecutorTransportFailures, competitionExecutorPhysicalAvailable, competitionExecutorPhysicalObservedAt)
}

// Keeps the prior complete snapshot on a database error; successful omission of
// a distributed round removes obsolete host series without inventing readiness.
func refreshCompetitionDistributedMetrics(ctx context.Context, settings *Settings, now time.Time) error {
	type hostMetric struct {
		hostId                          string
		busy, reconciliation, qualified bool
		heartbeat                       *time.Time
		deadline                        *time.Time
	}
	hostMetrics := []hostMetric{}
	err := captureDatabaseError(func() {
		if settings == nil || !settings.AutomationPolicy.Enabled {
			return
		}
		server.Db(ctx, func(conn server.PgConn) {
			var installed bool
			server.Raise(conn.QueryRow(ctx, `SELECT to_regclass('competition_execution_slot') IS NOT NULL AND to_regclass('competition_executor_equivalence') IS NOT NULL`).Scan(&installed))
			if !installed {
				return
			}
			var policyJson []byte
			var roundId server.Id
			var opensAt time.Time
			var finalizedAt, sealedAt *time.Time
			loadErr := conn.QueryRow(ctx, `SELECT round_id,policy_json,opens_at,finalized_at,automation_sealed_at FROM competition_round
			 WHERE competition_id=$1 AND staging=$2 AND NOT canceled
			 AND COALESCE((policy_json->'automation_policy'->>'enabled')::boolean,false)
			 AND COALESCE((policy_json->'automation_policy'->>'staging')::boolean,false)=$2
			 AND policy_json->'automation_policy'->>'environment'=$3 AND policy_json->'automation_policy'->>'season_id'=$4
			 ORDER BY epoch_number DESC,round_id DESC LIMIT 1`, settings.CompetitionId, settings.AutomationPolicy.Staging,
				settings.AutomationPolicy.Environment, settings.AutomationPolicy.SeasonId).Scan(&roundId, &policyJson, &opensAt, &finalizedAt, &sealedAt)
			if errors.Is(loadErr, pgx.ErrNoRows) {
				return
			}
			server.Raise(loadErr)
			// Choose the current round before testing execution mode/lifecycle:
			// historical distributed policy cannot replace a newer singleton.
			if now.Before(opensAt) || finalizedAt != nil || sealedAt != nil {
				return
			}
			policy, policyErr := decodeRoundPolicySnapshot(policyJson)
			server.Raise(policyErr)
			if policy.DistributedEvaluationPolicy == nil {
				return
			}
			for _, host := range policy.DistributedEvaluationPolicy.Hosts {
				metric := hostMetric{hostId: host.HostId}
				server.Raise(conn.QueryRow(ctx, `SELECT s.job_id IS NOT NULL,COALESCE(s.job_id IS NOT NULL AND s.lease_expires_at<=$4,false),h.heartbeat_at,j.execution_deadline_at,
				 EXISTS(SELECT 1 FROM competition_executor_equivalence e JOIN competition_round_baseline b USING(round_id)
				 WHERE e.round_id=$1 AND e.host_id=$2 AND e.baseline_sha256=b.baseline_sha256
				 AND e.receipt_json->>'policy_approval_sha256'=$5 AND COALESCE((e.receipt_json->>'passed')::boolean,false))
				 FROM (SELECT 1) anchor LEFT JOIN competition_execution_slot s ON s.slot_id=$3 AND s.host_id=$2 AND s.round_id=$1
				 LEFT JOIN competition_job j ON j.job_id=s.job_id LEFT JOIN competition_evaluator_host h ON h.host_id=$2`,
					roundId, host.HostId, host.SlotId, now, policy.DistributedEvaluationPolicy.ApprovalSha256).
					Scan(&metric.busy, &metric.reconciliation, &metric.heartbeat, &metric.deadline, &metric.qualified))
				hostMetrics = append(hostMetrics, metric)
			}
		})
	})
	if err != nil {
		return err
	}
	competitionExecutorBusy.Reset()
	competitionExecutorReconciliationRequired.Reset()
	competitionExecutorHeartbeatAge.Reset()
	competitionExecutorDeadlineRemaining.Reset()
	competitionExecutorQualified.Reset()
	if len(hostMetrics) == 0 {
		competitionExecutorRemoteState.Reset()
		competitionExecutorPhysicalAvailable.Reset()
		competitionExecutorPhysicalObservedAt.Reset()
	}
	oldestHeartbeatAge := -1.0
	allHeartbeatsPresent := true
	for _, metric := range hostMetrics {
		busy, reconciliation, qualified := 0.0, 0.0, 0.0
		if metric.busy {
			busy = 1
		}
		if metric.reconciliation {
			reconciliation = 1
		}
		if metric.qualified {
			qualified = 1
		}
		heartbeatAge, remaining := -1.0, 0.0
		if metric.heartbeat != nil {
			heartbeatAge = max(0, now.Sub(*metric.heartbeat).Seconds())
			oldestHeartbeatAge = max(oldestHeartbeatAge, heartbeatAge)
		} else {
			allHeartbeatsPresent = false
		}
		if metric.deadline != nil {
			remaining = max(0, metric.deadline.Sub(now).Seconds())
		}
		competitionExecutorBusy.WithLabelValues(metric.hostId).Set(busy)
		competitionExecutorReconciliationRequired.WithLabelValues(metric.hostId).Set(reconciliation)
		competitionExecutorHeartbeatAge.WithLabelValues(metric.hostId).Set(heartbeatAge)
		competitionExecutorDeadlineRemaining.WithLabelValues(metric.hostId).Set(remaining)
		competitionExecutorQualified.WithLabelValues(metric.hostId).Set(qualified)
	}
	if len(hostMetrics) != 0 {
		if !allHeartbeatsPresent {
			oldestHeartbeatAge = -1
		}
		competitionWorkerHeartbeatAge.Set(oldestHeartbeatAge)
	}
	return nil
}
