// Fleet measurements use the exact URL admission cohort, never all online peers.
package model

import (
	"context"
	"time"

	"github.com/urnetwork/server"
)

type ProviderUrlProbeFleet struct {
	Eligible      int
	Due           int
	Complete      int
	QuotaComplete int
	Overdue       int
	RunsNeeded    int
	// Deprecated compatibility alias; use RunsNeeded for the v2 quota.
	SuccessesNeeded        int
	SecurityExceptions     int
	SecurityUnknownTargets int
	Warming                int
	MissingCycles          int
	CohortStartedAtSeconds float64
	OldestDueSeconds       float64
	// First admission age never resets on re-entry. These partitions retain
	// every current provider without granting older providers another grace period.
	MatureEligible           int
	MatureQuotaComplete      int
	MatureRunsNeeded         int
	WarmingEligible          int
	WarmingQuotaComplete     int
	WarmingRunsNeeded        int
	EligibilityAgeUnknown    int
	AgeUnknownQuotaComplete  int
	AgeUnknownRunsNeeded     int
	MatureDeficitDiagnostics ProviderUrlProbeMatureDeficitDiagnostics
}

// This aggregate belongs on the periodic metrics path, not on every claim.
// Missing quota rows remain in the denominator and show their full deficit.
func GetProviderUrlProbeFleet(ctx context.Context, now time.Time) ProviderUrlProbeFleet {
	fleet := ProviderUrlProbeFleet{}
	policy := SelectedProviderUrlProbePolicyVersion()
	server.Db(ctx, func(conn server.PgConn) {
		var deficitJson []byte
		rows, err := conn.Query(ctx, providerUrlProbeFleetSql(policy), now.UTC(), ProvideModePublic, ProviderUrlProbeRunTarget,
			now.Add(-ProviderEgressProbeRefreshAge).UTC())
		server.WithPgResult(rows, err, func() {
			if rows.Next() {
				server.Raise(rows.Scan(&fleet.Eligible, &fleet.Due, &fleet.Complete, &fleet.Overdue, &fleet.RunsNeeded, &fleet.OldestDueSeconds,
					&fleet.SecurityExceptions, &fleet.SecurityUnknownTargets, &fleet.QuotaComplete,
					&fleet.Warming, &fleet.MissingCycles, &fleet.CohortStartedAtSeconds,
					&fleet.MatureEligible, &fleet.MatureQuotaComplete, &fleet.MatureRunsNeeded,
					&fleet.WarmingEligible, &fleet.WarmingQuotaComplete, &fleet.WarmingRunsNeeded,
					&fleet.EligibilityAgeUnknown, &fleet.AgeUnknownQuotaComplete, &fleet.AgeUnknownRunsNeeded, &deficitJson))
				fleet.MatureDeficitDiagnostics = providerUrlProbeMatureDeficitDiagnostics(deficitJson, policy, fleet.MatureEligible-fleet.MatureQuotaComplete)
			}
		})
	})
	fleet.SuccessesNeeded = fleet.RunsNeeded
	return fleet
}

// The census and its bounded deficient head share one statement snapshot and
// the same selected policy. This CTE already computes every eligible count.
func providerUrlProbeFleetSql(policy int) string {
	return `WITH cohort AS MATERIALIZED (
			SELECT cycle.client_id,cycle.cycle_started_at,cycle.next_attempt_at,recent.run_count,
				` + providerHasUrlSecurityExceptionSql("provider.client_id") + ` AS security_exception,
				EXISTS(SELECT 1 FROM provider_egress_health AS health
					WHERE health.client_id=provider.client_id AND health.legacy_tls_authentication_failure) AS unknown_security_target
			FROM network_client_location_reliability AS provider
			JOIN network_client AS client USING (client_id)
			LEFT JOIN provider_egress_probe_cycle AS cycle USING (client_id)
			CROSS JOIN LATERAL (` + providerUrlProbeRunWindowSql("provider.client_id", "$1", policy) + `) AS recent
			WHERE provider.connected AND provider.valid
			AND (provider.ipv4_proven OR NOT provider.ipv6_proven)
			AND client.active AND client.source_client_id IS NULL
			AND EXISTS (SELECT 1 FROM provide_key AS key WHERE key.client_id=provider.client_id AND key.provide_mode=$2)
			AND ` + providerUrlProbeAdmissionSql("provider") + `
			)` + providerUrlProbeMatureDeficitCtesSql(policy) + `
			SELECT COUNT(*),
				COUNT(*) FILTER (WHERE next_attempt_at <= $1 OR client_id IS NULL),
				COUNT(*) FILTER (WHERE run_count >= $3 AND NOT security_exception),
				COUNT(*) FILTER (WHERE (run_count < $3 OR security_exception) AND (cycle_started_at <= $4 OR client_id IS NULL)),
				COALESCE(SUM(GREATEST(0,$3-run_count)),0),
				COALESCE(MAX(EXTRACT(EPOCH FROM ($1-next_attempt_at))) FILTER (WHERE next_attempt_at <= $1),0),
				COUNT(*) FILTER (WHERE security_exception),
				COUNT(*) FILTER (WHERE unknown_security_target),
				COUNT(*) FILTER (WHERE run_count >= $3),
				COUNT(*) FILTER (WHERE (run_count < $3 OR security_exception) AND cycle_started_at > $4),
				COUNT(*) FILTER (WHERE client_id IS NULL),
				COALESCE(MIN(EXTRACT(EPOCH FROM cycle_started_at)),0),
				COUNT(*) FILTER (WHERE cycle_started_at <= $4),
				COUNT(*) FILTER (WHERE cycle_started_at <= $4 AND run_count >= $3),
				COALESCE(SUM(GREATEST(0,$3-run_count)) FILTER (WHERE cycle_started_at <= $4),0),
				COUNT(*) FILTER (WHERE cycle_started_at > $4 AND cycle_started_at <= $1),
				COUNT(*) FILTER (WHERE cycle_started_at > $4 AND cycle_started_at <= $1 AND run_count >= $3),
				COALESCE(SUM(GREATEST(0,$3-run_count)) FILTER (WHERE cycle_started_at > $4 AND cycle_started_at <= $1),0),
				COUNT(*) FILTER (WHERE cycle_started_at IS NULL OR cycle_started_at > $1),
				COUNT(*) FILTER (WHERE (cycle_started_at IS NULL OR cycle_started_at > $1) AND run_count >= $3),
				COALESCE(SUM(GREATEST(0,$3-run_count)) FILTER (WHERE cycle_started_at IS NULL OR cycle_started_at > $1),0)
				,(SELECT COALESCE(jsonb_agg(diagnostic ORDER BY client_id),'[]'::jsonb) FROM mature_deficit_details)
			FROM cohort`
}
