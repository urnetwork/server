// Fleet measurements use the exact URL admission cohort, never all online peers.
package model

import (
	"context"
	"time"

	"github.com/urnetwork/server"
)

type ProviderUrlProbeFleet struct {
	Eligible               int
	Due                    int
	Complete               int
	QuotaComplete          int
	Overdue                int
	SuccessesNeeded        int
	SecurityExceptions     int
	SecurityUnknownTargets int
	Warming                int
	MissingCycles          int
	CohortStartedAtSeconds float64
	OldestDueSeconds       float64
}

// This aggregate belongs on the periodic metrics path, not on every claim.
// Missing quota rows remain in the denominator and show their full deficit.
func GetProviderUrlProbeFleet(ctx context.Context, now time.Time) ProviderUrlProbeFleet {
	fleet := ProviderUrlProbeFleet{}
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `WITH cohort AS MATERIALIZED (
			SELECT cycle.client_id,cycle.cycle_started_at,cycle.next_attempt_at,recent.success_count,
				`+providerHasUrlSecurityExceptionSql("provider.client_id")+` AS security_exception,
				EXISTS(SELECT 1 FROM provider_egress_health AS health
					WHERE health.client_id=provider.client_id AND health.legacy_tls_authentication_failure) AS unknown_security_target
			FROM network_client_location_reliability AS provider
			JOIN network_client AS client USING (client_id)
			LEFT JOIN provider_egress_probe_cycle AS cycle USING (client_id)
			CROSS JOIN LATERAL (`+providerUrlProbeSuccessWindowSql("provider.client_id", "$1")+`) AS recent
			WHERE provider.connected AND provider.valid
			AND (provider.ipv4_proven OR NOT provider.ipv6_proven)
			AND client.active AND client.source_client_id IS NULL
			AND EXISTS (SELECT 1 FROM provide_key AS key WHERE key.client_id=provider.client_id AND key.provide_mode=$2)
			AND `+providerProbeEligibilitySql("provider")+`
			)
			SELECT COUNT(*),
				COUNT(*) FILTER (WHERE next_attempt_at <= $1 OR client_id IS NULL),
				COUNT(*) FILTER (WHERE success_count >= $3 AND NOT security_exception),
				COUNT(*) FILTER (WHERE (success_count < $3 OR security_exception) AND (cycle_started_at <= $4 OR client_id IS NULL)),
				COALESCE(SUM(GREATEST(0,$3-success_count)),0),
				COALESCE(MAX(EXTRACT(EPOCH FROM ($1-next_attempt_at))) FILTER (WHERE next_attempt_at <= $1),0),
				COUNT(*) FILTER (WHERE security_exception),
				COUNT(*) FILTER (WHERE unknown_security_target),
				COUNT(*) FILTER (WHERE success_count >= $3),
				COUNT(*) FILTER (WHERE (success_count < $3 OR security_exception) AND cycle_started_at > $4),
				COUNT(*) FILTER (WHERE client_id IS NULL),
				COALESCE(MIN(EXTRACT(EPOCH FROM cycle_started_at)),0)
			FROM cohort`, now.UTC(), ProvideModePublic, ProviderEgressProbeSuccessTarget,
			now.Add(-ProviderEgressProbeRefreshAge).UTC())
		server.WithPgResult(rows, err, func() {
			if rows.Next() {
				server.Raise(rows.Scan(&fleet.Eligible, &fleet.Due, &fleet.Complete, &fleet.Overdue, &fleet.SuccessesNeeded, &fleet.OldestDueSeconds,
					&fleet.SecurityExceptions, &fleet.SecurityUnknownTargets, &fleet.QuotaComplete,
					&fleet.Warming, &fleet.MissingCycles, &fleet.CohortStartedAtSeconds))
			}
		})
	})
	return fleet
}
