// Durable URL cycles admit bounded work before any provider tunnel opens.
package model

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
)

// A durable admission token and trailing-four-hour measured-run deficit travel with
// the existing provider place. OutcomeCount is a monotonic receipt ordinal.
type ProviderUrlProbeDue struct {
	ClientId       server.Id `json:"client_id"`
	CountryCode    string    `json:"country_code,omitempty"`
	Region         string    `json:"region,omitempty"`
	CycleStartedAt time.Time `json:"cycle_started_at"`
	RunsNeeded     int       `json:"runs_needed"`
	// Deprecated wire alias for rolling upgrades; capability v2 defines both
	// fields as the measured-run deficit, including accepted failures.
	SuccessesNeeded      int                        `json:"successes_needed"`
	OutcomeCount         int                        `json:"outcome_count"`
	ClaimOrdinal         int64                      `json:"claim_ordinal"`
	ClaimedAt            time.Time                  `json:"claimed_at"`
	CompletedRunCount    *int64                     `json:"completed_run_count,omitempty"`
	SecurityDestinations []egresshealth.Destination `json:"security_destinations,omitempty"`
}

// A stable provider slot belongs to one logical shard, independently of the
// taskworker host. Power-of-two geometries preserve the old hash mapping.
const ProviderUrlProbeSlotCount = 1024

// Compatibility readers share the exact slot-to-shard mapping with indexed
// admission. hashtext is signed, so normalize the stable slot before modulo.
func providerUrlProbeShardSql(clientExpression, shardCountExpression string) string {
	return fmt.Sprintf(`(((hashtext(%s::text) %% %d)+%d) %% %d) %% %s`,
		clientExpression, ProviderUrlProbeSlotCount, ProviderUrlProbeSlotCount, ProviderUrlProbeSlotCount, shardCountExpression)
}

// Stable per-provider jitter spreads recurring work without retaining a timer
// or tunnel. Quota-full scheduling is separately tied to the oldest counted run.
func providerUrlProbePacedAttemptSql(cycleAlias, measuredAtParam, acceptedSuccessesParam string) string {
	rules := GetProviderEgressRules()
	defaults := DefaultProviderEgressRules()
	successSeconds, failureSeconds := rules.UrlSuccessIntervalSeconds, rules.UrlFailureIntervalSeconds
	if successSeconds == 0 {
		successSeconds = defaults.UrlSuccessIntervalSeconds
	}
	if failureSeconds == 0 {
		failureSeconds = defaults.UrlFailureIntervalSeconds
	}
	return fmt.Sprintf(`%[2]s::timestamp + ((CASE WHEN %[3]s > 0 THEN %[4]d ELSE %[5]d END)
		* (0.9 + (((hashtext(%[1]s.client_id::text) %% 2001) + 2001) %% 2001)::double precision / 10000)
		* interval '1 second')`, cycleAlias, measuredAtParam, acceptedSuccessesParam,
		successSeconds, failureSeconds)
}

// This success-only projection remains diagnostic. It cannot satisfy the
// measured-run quota by itself or change the separate eight-hour quality ratio.
func providerUrlProbeSuccessWindowSql(clientIdExpression, nowExpression string) string {
	return fmt.Sprintf(`SELECT COUNT(*)::integer AS success_count, MIN(measured_at) AS oldest_success_at
		FROM (SELECT measured_at FROM provider_egress_health_history
			WHERE client_id=%s AND url_probe AND url_probe_policy_version=%d AND ok_count=1
			AND measured_at > %s::timestamp - interval '%d seconds'
			AND measured_at <= %s::timestamp
			ORDER BY measured_at DESC LIMIT %d) AS recent_successes`, clientIdExpression, SelectedProviderUrlProbePolicyVersion(), nowExpression,
		int(ProviderEgressProbeRefreshAge/time.Second), nowExpression, ProviderUrlProbeRunTarget)
}

// One accepted, admitted, selected-policy measurement is either success or
// failure: E=total_count-ok_count. Setup/attempt-only and security-only reports
// have no measured row or total_count=0; grouped/legacy versions do not count.
// The partial index bounds reads to the latest ten runs, with strict expiry
// and no future credit. Immutable run_id identity deduplicates publication.
func providerUrlProbeRunWindowSql(clientIdExpression, nowExpression string) string {
	return fmt.Sprintf(`SELECT COUNT(*)::integer AS run_count, MIN(measured_at) AS oldest_run_at
		FROM (SELECT measured_at FROM provider_egress_health_history
			WHERE client_id=%s AND url_probe AND url_probe_policy_version=%d
			AND total_count=1 AND (ok_count=0 OR ok_count=1)
			AND measured_at > %s::timestamp - interval '%d seconds'
			AND measured_at <= %s::timestamp
			ORDER BY measured_at DESC LIMIT %d) AS recent_runs`, clientIdExpression, SelectedProviderUrlProbePolicyVersion(), nowExpression,
		int(ProviderEgressProbeRefreshAge/time.Second), nowExpression, ProviderUrlProbeRunTarget)
}

// The rollup seeds never-probed providers once. Its ordered next-attempt index
// supplies a bounded candidate head; row locks prevent duplicate admission and
// advance the retry deadline atomically. Rejected stale hints leave the ordered
// head without changing progress. Accepted measured failures count; local
// setup and attempt-only reports do not. Runs expire individually; the
// admission token never resets.
func ClaimProviderUrlProbeDue(ctx context.Context, now time.Time, limit, shardIndex, shardCount int) []ProviderUrlProbeDue {
	return ClaimProviderUrlProbeDueWithStatus(ctx, now, limit, shardIndex, shardCount).Providers
}

// Readiness metadata prevents a bounded maintenance page from looking like an
// empty due cohort. The accepted writer epoch is operator-attested, not inferred.
type ProviderUrlProbeDueResult struct {
	Providers                  []ProviderUrlProbeDue `json:"providers"`
	PriorityMaintenancePending bool                  `json:"priority_maintenance_pending,omitempty"`
	CompletedRunPriorityReady  bool                  `json:"completed_run_priority_ready"`
	CompletedRunPrioritySince  *time.Time            `json:"completed_run_priority_since,omitempty"`
}

// Maintains owned counts before priority selection. Both legacy and priority
// order issue durable run identities, so receipt ingestion precedes activation.
func ClaimProviderUrlProbeDueWithStatus(ctx context.Context, now time.Time, limit, shardIndex, shardCount int) ProviderUrlProbeDueResult {
	result := ProviderUrlProbeDueResult{Providers: []ProviderUrlProbeDue{}}
	if limit <= 0 || shardCount < 1 || ProviderUrlProbeSlotCount < shardCount || shardIndex < 0 || shardIndex >= shardCount {
		return result
	}
	since := GetProviderEgressRules().UrlCompletedRunPrioritySince
	if !since.IsZero() {
		result.CompletedRunPrioritySince = &since
	}
	result.CompletedRunPriorityReady = providerUrlProbeCompletedPriorityReady(now)
	server.Tx(ctx, func(tx server.PgTx) {
		// A serialization/deadlock retry must discard the rolled-back result.
		result.Providers = []ProviderUrlProbeDue{}
		result.PriorityMaintenancePending = false
		maintained := maintainProviderUrlProbeCompletedPriority(ctx, tx, now, shardIndex, shardCount, result.CompletedRunPriorityReady)
		if result.CompletedRunPriorityReady && !maintained {
			result.PriorityMaintenancePending = true
			return
		}
		rows, err := tx.Query(ctx, providerUrlProbeDueSql(shardIndex, shardCount, result.CompletedRunPriorityReady),
			now.UTC(), ProvideModePublic, limit, shardCount, shardIndex,
			ProviderUrlProbeRunTarget, now.Add(ProviderEgressProbeAttemptBackoff).UTC())
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var provider ProviderUrlProbeDue
				var securityDestinations []byte
				server.Raise(rows.Scan(&provider.ClientId, &provider.CycleStartedAt, &provider.RunsNeeded, &provider.OutcomeCount,
					&provider.CountryCode, &provider.Region, &securityDestinations,
					&provider.ClaimOrdinal, &provider.ClaimedAt, &provider.CompletedRunCount))
				server.Raise(json.Unmarshal(securityDestinations, &provider.SecurityDestinations))
				provider.SuccessesNeeded = provider.RunsNeeded
				result.Providers = append(result.Providers, provider)
			}
		})
	})
	// Retire at least as many old receipt slots as this admission can issue.
	// Cleanup remains bounded even when a mostly empty shard is polled.
	RemoveExpiredProviderUrlProbeRuns(ctx, now, limit)
	return result
}

// One shared statement is exercised by admission and its query-plan regression.
func providerUrlProbeDueSql(shardIndex, shardCount int, priorities ...bool) string {
	priority := len(priorities) > 0 && priorities[0]
	predicate := ""
	cycleOrder := "cycle.next_attempt_at,cycle.client_id"
	mergeOrder := "next_attempt_at,client_id"
	slotOrder := "slot_candidates.next_attempt_at,slot_candidates.client_id"
	resultOrder := "candidates.next_attempt_at,claimed.client_id"
	countProjection := "NULL::bigint"
	if priority {
		predicate = " AND cycle.completed_priority_ready"
		cycleOrder = "cycle.completed_run_count," + cycleOrder
		mergeOrder = "completed_run_count," + mergeOrder
		slotOrder = "slot_candidates.completed_run_count," + slotOrder
		resultOrder = "candidates.completed_run_count," + resultOrder
		countProjection = "candidates.completed_run_count"
	}
	head := fmt.Sprintf(`WITH head AS MATERIALIZED (
				SELECT cycle.client_id, cycle.next_attempt_at, cycle.completed_run_count
				FROM provider_egress_probe_cycle AS cycle
				WHERE cycle.eligible AND cycle.next_attempt_at <= $1%s
				AND $4=1 AND $5=0
				ORDER BY %s
				LIMIT $3
				FOR UPDATE OF cycle SKIP LOCKED
			)`, predicate, cycleOrder)
	if shardCount > 1 {
		// Ordered UNION branches permit Merge Append to advance only the slot
		// holding the next oldest row. A lateral join followed by Sort would
		// eagerly read owned_slots*limit rows even for one small admission.
		branches := []string{}
		for slot := shardIndex; slot < ProviderUrlProbeSlotCount; slot += shardCount {
			branches = append(branches, fmt.Sprintf(`(SELECT cycle.client_id,cycle.next_attempt_at,cycle.completed_run_count
				FROM provider_egress_probe_cycle AS cycle
				WHERE cycle.eligible AND cycle.slot_id=%d AND cycle.next_attempt_at<=$1%s
				ORDER BY %s LIMIT $3)`, slot, predicate, cycleOrder))
		}
		head = fmt.Sprintf(`WITH slot_candidates AS MATERIALIZED (
				%s ORDER BY %s LIMIT $3
			), head AS MATERIALIZED (
				SELECT cycle.client_id,cycle.next_attempt_at,cycle.completed_run_count
				FROM slot_candidates CROSS JOIN LATERAL (
					SELECT cycle.client_id,cycle.next_attempt_at,cycle.completed_run_count
					FROM provider_egress_probe_cycle AS cycle
					WHERE cycle.client_id=slot_candidates.client_id
					AND cycle.eligible AND cycle.next_attempt_at<=$1%s AND $4=%d AND $5=%d
					OFFSET 0 FOR UPDATE OF cycle SKIP LOCKED
				) AS cycle
				ORDER BY %s LIMIT $3
			)`, strings.Join(branches, " UNION ALL "), mergeOrder, predicate, shardCount, shardIndex, slotOrder)
	}
	// Generic plans cannot estimate a parameterized LIMIT's actual head size.
	// Keep admission lookups parameterized by each head row, and give UPDATE
	// targets explicit bounded key arrays as well as their exact identity joins.
	return fmt.Sprintf(head+`, candidates AS MATERIALIZED (
				SELECT head.*, provider.country_location_id, provider.region_location_id
				FROM head
				CROSS JOIN LATERAL (
					SELECT provider.country_location_id, provider.region_location_id
					FROM network_client_location_reliability AS provider
					JOIN network_client AS client USING (client_id)
					WHERE provider.client_id=head.client_id
					AND provider.connected AND provider.valid
					AND (provider.ipv4_proven OR NOT provider.ipv6_proven)
					AND client.active AND client.source_client_id IS NULL
					AND EXISTS (SELECT 1 FROM provide_key AS key
						WHERE key.client_id=head.client_id AND key.provide_mode=$2)
					AND %s
					OFFSET 0
				) AS provider
			), ineligible_head AS (
				UPDATE provider_egress_probe_cycle AS cycle SET eligible = false
				FROM head WHERE cycle.client_id = head.client_id
				AND cycle.client_id=ANY(ARRAY(SELECT client_id FROM head))
				AND NOT EXISTS (SELECT 1 FROM candidates WHERE candidates.client_id = head.client_id)
				RETURNING cycle.client_id
			), measured AS MATERIALIZED (
				SELECT candidates.*, recent.run_count, recent.oldest_run_at, successes.success_count,
					security.security_exception
				FROM candidates CROSS JOIN LATERAL (%s) AS recent
				CROSS JOIN LATERAL (%s) AS successes
				CROSS JOIN LATERAL (SELECT (%s) AS security_exception OFFSET 0) AS security
			), claimed AS (
				UPDATE provider_egress_probe_cycle AS cycle SET
					success_count = measured.success_count,
					completed_priority_ready = false,
					claim_ordinal = cycle.claim_ordinal + CASE WHEN measured.run_count < $6 OR measured.security_exception THEN 1 ELSE 0 END,
					next_attempt_at = CASE WHEN measured.run_count >= $6 AND NOT measured.security_exception
						THEN measured.oldest_run_at + interval '%d seconds' ELSE $7 END
				FROM measured WHERE cycle.client_id=measured.client_id
				AND cycle.client_id=ANY(ARRAY(SELECT client_id FROM measured))
				RETURNING cycle.client_id, cycle.cycle_started_at, cycle.success_count, cycle.outcome_count, cycle.claim_ordinal,
					measured.security_exception, measured.run_count
			), issued AS (
				INSERT INTO provider_url_probe_run(client_id,claim_ordinal,claimed_at)
				SELECT client_id,claim_ordinal,$1 FROM claimed
				WHERE claimed.run_count<$6 OR claimed.security_exception
				RETURNING client_id,claim_ordinal,claimed_at
			)
			SELECT claimed.client_id, claimed.cycle_started_at, GREATEST(0, $6-claimed.run_count),
				claimed.outcome_count,
				COALESCE(country.country_code, ''), COALESCE(region.location_name, ''),
				COALESCE((SELECT jsonb_agg(security.destination ORDER BY security.url_key)
					FROM provider_egress_url_security AS security
					WHERE security.client_id=claimed.client_id AND security.tls_failure), '[]'::jsonb),
				claimed.claim_ordinal, issued.claimed_at, %s
			FROM claimed JOIN issued USING(client_id,claim_ordinal)
			JOIN candidates USING (client_id)
			LEFT JOIN LATERAL (SELECT country_code FROM location
				WHERE location_id=candidates.country_location_id OFFSET 0) AS country ON true
			LEFT JOIN LATERAL (SELECT location_name FROM location
				WHERE location_id=candidates.region_location_id OFFSET 0) AS region ON true
			WHERE claimed.run_count < $6 OR claimed.security_exception
			ORDER BY %s
		`, providerProbeEligibilitySql("provider"), providerUrlProbeRunWindowSql("candidates.client_id", "$1"),
		providerUrlProbeSuccessWindowSql("candidates.client_id", "$1"),
		providerHasUrlSecurityExceptionSql("candidates.client_id"), int(ProviderEgressProbeRefreshAge/time.Second), countProjection, resultOrder)
}
