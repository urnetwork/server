// Durable URL cycles admit bounded work before any provider tunnel opens.
package model

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/qualityprobe/egresshealth"
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

// Replace measurements before they leave the four-hour window. This covers
// the default 220-second URL turn plus 90 seconds for control/publication and
// 50 seconds of scheduling margin. It is not extra quota credit or a promise
// that a stalled worker finishes. Admission still owns one 15-minute claim.
const ProviderUrlProbeRenewalHeadroom = 6 * time.Minute

// The latest ten accepted measurements suffice to decide whether at least ten
// will remain at the replacement horizon; any older measurements expire first.
// Full cohorts outside that horizon do not receive another measured turn.
func providerUrlProbeMeasuredWorkDueSql(recentAlias, nowExpression, targetExpression string) string {
	return fmt.Sprintf("(%s.run_count < %s OR %s.oldest_run_at <= %s::timestamp - interval '%d seconds')",
		recentAlias, targetExpression, recentAlias, nowExpression, int((ProviderEgressProbeRefreshAge-ProviderUrlProbeRenewalHeadroom)/time.Second))
}

// Compatibility readers share the exact slot-to-shard mapping with indexed
// admission. hashtext is signed, so normalize the stable slot before modulo.
func providerUrlProbeShardSql(clientExpression, shardCountExpression string) string {
	return fmt.Sprintf(`(((hashtext(%s::text) %% %d)+%d) %% %d) %% %s`,
		clientExpression, ProviderUrlProbeSlotCount, ProviderUrlProbeSlotCount, ProviderUrlProbeSlotCount, shardCountExpression)
}

// Stable per-provider jitter spreads recurring work without retaining a timer
// or tunnel. Quota-full scheduling is separately tied to the oldest counted run.
func providerUrlProbePacedAttemptSql(cycleAlias, measuredAtParam, acceptedSuccessesParam string, measuredDeficit ...string) string {
	rules := GetProviderEgressRules()
	defaults := DefaultProviderEgressRules()
	successSeconds, failureSeconds := rules.UrlSuccessIntervalSeconds, rules.UrlFailureIntervalSeconds
	if successSeconds == 0 {
		successSeconds = defaults.UrlSuccessIntervalSeconds
	}
	if failureSeconds == 0 {
		failureSeconds = defaults.UrlFailureIntervalSeconds
	}
	// A mature rolling deficit needs replenishment even when its latest
	// measurement succeeded. Waiting the warmup success interval can lose
	// additional retained measurements before that provider becomes due again.
	// Only the accepted-history writer supplies this measured deficit; setup
	// completion keeps its existing independent pacing contract.
	recovery := "false"
	if len(measuredDeficit) != 0 {
		recovery = fmt.Sprintf("(%s) AND %s.cycle_started_at <= %s::timestamp - interval '%d seconds'",
			measuredDeficit[0], cycleAlias, measuredAtParam, int(ProviderEgressProbeRefreshAge/time.Second))
	}
	paced := fmt.Sprintf(`%[2]s::timestamp + ((CASE WHEN %[3]s > 0 AND NOT (%[6]s) THEN %[4]d ELSE %[5]d END)
		* (0.9 + (((hashtext(%[1]s.client_id::text) %% 2001) + 2001) %% 2001)::double precision / 10000)
		* interval '1 second')`, cycleAlias, measuredAtParam, acceptedSuccessesParam,
		successSeconds, failureSeconds, recovery)
	if len(measuredDeficit) == 0 {
		return paced
	}
	// Only an accepted warm success can reserve beyond its cycle's maturity.
	// Cap that deficit's pace; active claims and setup completion stay unchanged.
	maturity := fmt.Sprintf("%s.cycle_started_at + interval '%d seconds'", cycleAlias, int(ProviderEgressProbeRefreshAge/time.Second))
	return fmt.Sprintf(`CASE WHEN (%s) AND %s > 0 AND %s::timestamp < (%s)
		THEN LEAST((%s), (%s)) ELSE (%s) END`, measuredDeficit[0], acceptedSuccessesParam,
		measuredAtParam, maturity, paced, maturity, paced)
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
func providerUrlProbeRunWindowSql(clientIdExpression, nowExpression string, policies ...int) string {
	policy := 0
	if len(policies) == 0 {
		policy = SelectedProviderUrlProbePolicyVersion()
	} else {
		policy = policies[0]
	}
	return fmt.Sprintf(`SELECT COUNT(*)::integer AS run_count, MIN(measured_at) AS oldest_run_at
		FROM (SELECT measured_at FROM provider_egress_health_history
			WHERE client_id=%s AND url_probe AND url_probe_policy_version=%d
			AND total_count=1 AND (ok_count=0 OR ok_count=1)
			AND measured_at > %s::timestamp - interval '%d seconds'
			AND measured_at <= %s::timestamp
			ORDER BY measured_at DESC LIMIT %d) AS recent_runs`, clientIdExpression, policy, nowExpression,
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
	return ClaimProviderUrlProbeDueWithObservation(ctx, now, limit, shardIndex, shardCount, nil)
}

// Observation is optional and request-local; it never changes the wire result
// or the transaction, statement, claim, and cleanup ordering.
func ClaimProviderUrlProbeDueWithObservation(ctx context.Context, now time.Time, limit, shardIndex, shardCount int, observation *ProviderUrlProbeDueObservation) (result ProviderUrlProbeDueResult) {
	observation.measure(ProviderUrlProbeDueModel, func() {
		result = claimProviderUrlProbeDue(ctx, now, limit, shardIndex, shardCount, observation)
	})
	return
}

func claimProviderUrlProbeDue(ctx context.Context, now time.Time, limit, shardIndex, shardCount int, observation *ProviderUrlProbeDueObservation) ProviderUrlProbeDueResult {
	result := ProviderUrlProbeDueResult{Providers: []ProviderUrlProbeDue{}}
	if limit <= 0 || shardCount < 1 || ProviderUrlProbeSlotCount < shardCount || shardIndex < 0 || shardIndex >= shardCount {
		return result
	}
	since := GetProviderEgressRules().UrlCompletedRunPrioritySince
	if !since.IsZero() {
		result.CompletedRunPrioritySince = &since
	}
	result.CompletedRunPriorityReady = providerUrlProbeCompletedPriorityReady(now)
	observation.measure(ProviderUrlProbeDueClaimTransaction, func() {
		server.Tx(ctx, func(tx server.PgTx) {
			observation.measure(ProviderUrlProbeDueClaimBody, func() {
				// A serialization/deadlock retry must discard the rolled-back result.
				result.Providers = []ProviderUrlProbeDue{}
				result.PriorityMaintenancePending = false
				// Retry admission once after a bounded repair of an old, idle,
				// completed quota deadline. Never widen the ordinary due cutoff.
				for pass := 0; pass < 2; pass++ {
					maintained := maintainProviderUrlProbeCompletedPriority(ctx, tx, now, shardIndex, shardCount, result.CompletedRunPriorityReady, observation)
					if result.CompletedRunPriorityReady && !maintained {
						result.PriorityMaintenancePending = true
						return
					}
					observation.measure(ProviderUrlProbeDueClaimQueryRows, func() {
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
					if pass != 0 || len(result.Providers) != 0 || repairProviderUrlProbeLegacyDeadlines(ctx, tx, now, limit, shardIndex, shardCount) == 0 {
						return
					}
				}
			})
		}, observation.database(false))
	})
	// Seven-day receipt storage cleanup has a durable Taskworker owner. Current
	// completion expiry above and measured-history clock predicates retain all
	// admission authority even when that independent cleanup is delayed.
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
					`+providerUrlProbeMeasuredWorkDueSql("recent", "$1", "$6")+` AS measured_work_due,
					security.security_exception
				FROM candidates CROSS JOIN LATERAL (%s) AS recent
				CROSS JOIN LATERAL (%s) AS successes
				CROSS JOIN LATERAL (SELECT (%s) AS security_exception OFFSET 0) AS security
			), claimed AS (
				UPDATE provider_egress_probe_cycle AS cycle SET
					success_count = measured.success_count,
					completed_priority_ready = false,
					claim_ordinal = cycle.claim_ordinal + CASE WHEN measured.measured_work_due OR measured.security_exception THEN 1 ELSE 0 END,
					next_attempt_at = CASE WHEN NOT measured.measured_work_due AND NOT measured.security_exception
						THEN measured.oldest_run_at + interval '%d seconds' ELSE $7 END
				FROM measured WHERE cycle.client_id=measured.client_id
				AND cycle.client_id=ANY(ARRAY(SELECT client_id FROM measured))
				RETURNING cycle.client_id, cycle.cycle_started_at, cycle.success_count, cycle.outcome_count, cycle.claim_ordinal,
					measured.security_exception, measured.run_count, measured.measured_work_due
			), issued AS (
				INSERT INTO provider_url_probe_run(client_id,claim_ordinal,claimed_at)
				SELECT client_id,claim_ordinal,$1 FROM claimed
				WHERE claimed.measured_work_due OR claimed.security_exception
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
			WHERE claimed.measured_work_due OR claimed.security_exception
			ORDER BY %s
		`, providerProbeEligibilitySql("provider"), providerUrlProbeRunWindowSql("candidates.client_id", "$1"),
		providerUrlProbeSuccessWindowSql("candidates.client_id", "$1"),
		providerHasUrlSecurityExceptionSql("candidates.client_id"), int((ProviderEgressProbeRefreshAge-ProviderUrlProbeRenewalHeadroom)/time.Second), countProjection, resultOrder)
}
