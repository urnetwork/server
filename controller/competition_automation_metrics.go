// Reports aggregate automation health without exposing candidate identities,
// rankings, scores, or private review findings before publication.
package controller

import (
	"context"
	"math"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/server"
)

var (
	competitionAutomationUnpublishedSeals = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "competition", Name: "automation_unpublished_seals",
		Help: "Sealed evaluation inventories awaiting their frozen publication time.",
	})
	competitionAutomationPendingEvents = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "competition", Name: "automation_pending_reward_events",
		Help: "Published reward events without an integration receipt; receipt is not proof of chain settlement.",
	})
	competitionAutomationOldestEventAge = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "competition", Name: "automation_oldest_reward_event_age_seconds",
		Help: "Seconds since the oldest unacknowledged reward event became public, or zero when none.",
	})
	competitionAutomationPendingSources = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "competition", Name: "automation_pending_source_activations",
		Help: "Durable source activation intents not yet activated; reward election does not authorize source.",
	})
	competitionAutomationSupervisorHeartbeatAge = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "competition", Name: "automation_supervisor_heartbeat_age_seconds",
		Help: "Age of the latest durable supervisor heartbeat, or -1 when no supervisor is registered.",
	})
	competitionAutomationSupervisorExpected = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "competition", Name: "automation_supervisor_expected",
		Help: "1 while enabled season progression still requires a supervisor; zero after the sixth seal or an explicit disable.",
	})
	competitionAutomationHoldActive = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "competition", Name: "automation_hold_active",
		Help: "Latest independently scoped pause state by bounded capability, never a candidate identifier.",
	}, []string{"kind"})
	competitionAutomationIncidentCount = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "competition", Name: "automation_integrity_incidents",
		Help: "Retained integrity incidents in the configured automation scope; not a count of dishonest miners.",
	})
)

// Registers bounded aggregate metrics with the existing main metrics pipeline.
func init() {
	prometheus.MustRegister(
		competitionAutomationUnpublishedSeals,
		competitionAutomationPendingEvents,
		competitionAutomationOldestEventAge,
		competitionAutomationPendingSources,
		competitionAutomationSupervisorHeartbeatAge,
		competitionAutomationSupervisorExpected,
		competitionAutomationHoldActive,
		competitionAutomationIncidentCount,
	)
}

// Keeps one successful database snapshot together. A failed refresh leaves the
// prior snapshot intact and is surfaced by the existing refresh-error counter.
type competitionAutomationMetricSnapshot struct {
	unpublishedSeals      int
	pendingEvents         int
	oldestPublicationAt   *time.Time
	pendingSources        int
	supervisorHeartbeatAt *time.Time
	supervisorExpected    bool
	activeHoldKinds       []string
	incidentCount         int
}

// Allows a back-compatible API binary to start before the additive v2 migration.
// Queries share one connection and never acquire a nested database resource.
func refreshAutomationOperationalMetrics(ctx context.Context, settings *Settings, now time.Time) error {
	snapshot := competitionAutomationMetricSnapshot{}
	err := captureDatabaseError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			var installed bool
			server.Raise(conn.QueryRow(ctx, `
				SELECT to_regclass('competition_evaluation_seal') IS NOT NULL
			`).Scan(&installed))
			if !installed {
				return
			}
			server.Raise(conn.QueryRow(ctx, `
				SELECT count(*) FROM competition_evaluation_seal AS seal
				JOIN competition_round AS round ON round.round_id = seal.round_id
				WHERE round.competition_id = $1 AND seal.publication_at > $2
			`, settings.CompetitionId, now).Scan(&snapshot.unpublishedSeals))
			server.Raise(conn.QueryRow(ctx, `
				SELECT count(*), min(event.publication_at)
				FROM competition_reward_outbox AS event
				JOIN competition_round AS round ON round.round_id = event.round_id
				WHERE round.competition_id = $1 AND event.publication_at <= $2
				  AND NOT EXISTS (
				      SELECT 1 FROM competition_settlement_receipt AS receipt
				      WHERE receipt.event_id = event.event_id
				  )
			`, settings.CompetitionId, now).Scan(&snapshot.pendingEvents, &snapshot.oldestPublicationAt))
			var abandonmentInstalled bool
			server.Raise(conn.QueryRow(ctx, `SELECT EXISTS (
				SELECT 1 FROM pg_attribute
				WHERE attrelid=to_regclass('competition_source_activation')
				  AND attname='abandoned_at' AND NOT attisdropped
			)`).Scan(&abandonmentInstalled))
			pendingSourcesQuery := `
				SELECT count(*) FROM competition_source_activation
				WHERE competition_id = $1 AND activated_at IS NULL
			`
			if abandonmentInstalled {
				pendingSourcesQuery += ` AND abandoned_at IS NULL`
			}
			server.Raise(conn.QueryRow(ctx, pendingSourcesQuery, settings.CompetitionId).Scan(&snapshot.pendingSources))
			server.Raise(conn.QueryRow(ctx, `
				SELECT max(heartbeat_at) FROM competition_supervisor_lease
				WHERE competition_id=$1 AND environment=$2 AND season_id=$3
			`, settings.CompetitionId, settings.AutomationPolicy.Environment, settings.AutomationPolicy.SeasonId).Scan(&snapshot.supervisorHeartbeatAt))
			if settings.AutomationPolicy.Enabled {
				server.Raise(conn.QueryRow(ctx, `
					SELECT COALESCE(max(CASE WHEN automation_sealed_at IS NOT NULL THEN epoch_number ELSE epoch_number-1 END),0) < 6
					FROM competition_round WHERE competition_id=$1 AND staging=false
					  AND policy_json->'automation_policy'->>'environment'=$2
					  AND policy_json->'automation_policy'->>'season_id'=$3
				`, settings.CompetitionId, settings.AutomationPolicy.Environment, settings.AutomationPolicy.SeasonId).Scan(&snapshot.supervisorExpected))
			}
			var incidentSchemaInstalled bool
			server.Raise(conn.QueryRow(ctx, `SELECT to_regclass('competition_automation_hold') IS NOT NULL`).Scan(&incidentSchemaInstalled))
			if incidentSchemaInstalled {
				rows, queryErr := conn.Query(ctx, `SELECT kind FROM (
					SELECT DISTINCT ON (kind) kind,active FROM competition_automation_hold
					WHERE environment=$1 AND competition_id=$2 AND season_id=$3 ORDER BY kind,revision DESC
				) AS latest WHERE active`, settings.AutomationPolicy.Environment, settings.CompetitionId, settings.AutomationPolicy.SeasonId)
				server.WithPgResult(rows, queryErr, func() {
					for rows.Next() {
						var kind string
						server.Raise(rows.Scan(&kind))
						snapshot.activeHoldKinds = append(snapshot.activeHoldKinds, kind)
					}
				})
				server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM competition_automation_incident
					WHERE environment=$1 AND competition_id=$2 AND season_id=$3`,
					settings.AutomationPolicy.Environment, settings.CompetitionId, settings.AutomationPolicy.SeasonId).Scan(&snapshot.incidentCount))
			}
		}, server.OptReadWrite())
	})
	if err != nil {
		return err
	}
	snapshot.publish(now)
	return nil
}

// Clamps clock skew and uses explicit absent sentinels rather than stale ages.
func (self competitionAutomationMetricSnapshot) publish(now time.Time) {
	for _, kind := range []string{"admission", "evaluation", "source_activation", "reward_delivery"} {
		competitionAutomationHoldActive.WithLabelValues(kind).Set(0)
		for _, activeKind := range self.activeHoldKinds {
			if activeKind == kind {
				competitionAutomationHoldActive.WithLabelValues(kind).Set(1)
			}
		}
	}
	competitionAutomationIncidentCount.Set(float64(self.incidentCount))
	competitionAutomationUnpublishedSeals.Set(float64(self.unpublishedSeals))
	competitionAutomationPendingEvents.Set(float64(self.pendingEvents))
	competitionAutomationPendingSources.Set(float64(self.pendingSources))
	competitionAutomationOldestEventAge.Set(0)
	if self.oldestPublicationAt != nil {
		competitionAutomationOldestEventAge.Set(math.Max(0, now.Sub(*self.oldestPublicationAt).Seconds()))
	}
	competitionAutomationSupervisorHeartbeatAge.Set(-1)
	competitionAutomationSupervisorExpected.Set(0)
	if self.supervisorExpected {
		competitionAutomationSupervisorExpected.Set(1)
	}
	if self.supervisorHeartbeatAt != nil {
		competitionAutomationSupervisorHeartbeatAge.Set(math.Max(0, now.Sub(*self.supervisorHeartbeatAt).Seconds()))
	}
}
