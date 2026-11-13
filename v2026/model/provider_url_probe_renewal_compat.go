package model

import (
	"context"
	"fmt"
	"time"

	"github.com/urnetwork/server/v2026"
)

// This is a conservative compatibility repair, not a new due-time predicate.
// Only an empty ordinary admission examines one bounded future head. Exact old
// quota-expiry geometry and retained completed ownership are necessary; every
// configured retry interval must also have elapsed since all retained activity.
// A busy, ambiguous or recent row is left for its original deadline. Such rows
// can occupy the capped head, so this is not an immediate full-cohort backfill.
func repairProviderUrlProbeLegacyDeadlines(ctx context.Context, tx server.PgTx, now time.Time, limit, shardIndex, shardCount int) int {
	rules, defaults := GetProviderEgressRules(), DefaultProviderEgressRules()
	success, failure := rules.UrlSuccessIntervalSeconds, rules.UrlFailureIntervalSeconds
	if success == 0 {
		success = defaults.UrlSuccessIntervalSeconds
	}
	if failure == 0 {
		failure = defaults.UrlFailureIntervalSeconds
	}
	seconds := max(success, failure)
	// Longer custom policies are intentionally not accelerated by this repair.
	// This also avoids overflowing duration arithmetic for an unusual setting.
	if seconds < 0 || seconds > int(ProviderEgressProbeRefreshAge/time.Second) {
		return 0
	}
	quiet := max(ProviderEgressProbeAttemptBackoff, time.Duration(seconds)*time.Second*11/10)
	var changed int
	server.Raise(tx.QueryRow(ctx, providerUrlProbeLegacyDeadlineRepairSql(shardIndex, shardCount), now.UTC(), min(limit, providerUrlProbeExpiryClients), now.Add(-quiet).UTC()).Scan(&changed))
	return changed
}

func providerUrlProbeLegacyDeadlineRepairSql(shardIndex, shardCount int) string {
	return providerUrlProbeMaintenancePrefix(fmt.Sprintf("cycle.eligible AND cycle.next_attempt_at>$1 AND cycle.next_attempt_at<=$1::timestamp+interval '%d seconds'", int(ProviderUrlProbeRenewalHeadroom/time.Second)), "next_attempt_at", shardIndex, shardCount) + fmt.Sprintf(`,
 eligible_repair AS MATERIALIZED (
  SELECT head.client_id FROM head
  CROSS JOIN LATERAL (
   SELECT cycle.claim_ordinal,cycle.latest_result_at FROM provider_egress_probe_cycle cycle
   WHERE cycle.client_id=head.client_id AND cycle.claim_ordinal>0
    AND cycle.latest_result_at IS NOT NULL AND cycle.latest_result_at<=$3 OFFSET 0
  )cycle
  CROSS JOIN LATERAL (
   SELECT run.claimed_at FROM provider_url_probe_run run
   WHERE run.client_id=head.client_id AND run.claim_ordinal=cycle.claim_ordinal
    AND run.completed_at IS NOT NULL AND run.received_at IS NOT NULL
    AND run.claimed_at<=$3 AND run.completed_at<=$3 AND run.received_at<=$3
    AND cycle.latest_result_at>=run.claimed_at OFFSET 0
  )owned
  LEFT JOIN LATERAL (
   SELECT attempt_at FROM provider_egress_probe_attempt attempt WHERE attempt.client_id=head.client_id OFFSET 0
  )attempt ON true
  CROSS JOIN LATERAL (%s)recent
  WHERE (attempt.attempt_at IS NULL OR attempt.attempt_at<=$3)
   AND recent.run_count=%d
   AND head.next_attempt_at=recent.oldest_run_at+interval '%d seconds'
 ), repaired AS (
  UPDATE provider_egress_probe_cycle cycle SET next_attempt_at=$1,completed_priority_ready=false
  FROM eligible_repair WHERE cycle.client_id=eligible_repair.client_id
   AND cycle.client_id=ANY(ARRAY(SELECT client_id FROM eligible_repair))
  RETURNING cycle.client_id
 ) SELECT COUNT(*) FROM repaired`, providerUrlProbeRunWindowSql("head.client_id", "$1"), ProviderUrlProbeRunTarget, int(ProviderEgressProbeRefreshAge/time.Second))
}
