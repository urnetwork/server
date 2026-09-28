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

// A durable admission token and trailing-four-hour success deficit travel with
// the existing provider place. OutcomeCount is a monotonic receipt ordinal.
type ProviderUrlProbeDue struct {
	ClientId             server.Id                  `json:"client_id"`
	CountryCode          string                     `json:"country_code,omitempty"`
	Region               string                     `json:"region,omitempty"`
	CycleStartedAt       time.Time                  `json:"cycle_started_at"`
	SuccessesNeeded      int                        `json:"successes_needed"`
	OutcomeCount         int                        `json:"outcome_count"`
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
// or tunnel. Quota-full scheduling is separately tied to the oldest success.
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

// The partial index excludes errors and grouped legacy reports before the
// bounded descending lookup. Future timestamps never satisfy a present quota.
func providerUrlProbeSuccessWindowSql(clientIdExpression, nowExpression string) string {
	return fmt.Sprintf(`SELECT COUNT(*)::integer AS success_count, MIN(measured_at) AS oldest_success_at
		FROM (SELECT measured_at FROM provider_egress_health_history
			WHERE client_id=%s AND url_probe AND url_probe_policy_version=%d AND ok_count=1
			AND measured_at > %s::timestamp - interval '%d seconds'
			AND measured_at <= %s::timestamp
			ORDER BY measured_at DESC LIMIT %d) AS recent_successes`, clientIdExpression, SelectedProviderUrlProbePolicyVersion(), nowExpression,
		int(ProviderEgressProbeRefreshAge/time.Second), nowExpression, ProviderEgressProbeSuccessTarget)
}

// The rollup seeds never-probed providers once. Its ordered next-attempt index
// supplies a bounded candidate head; row locks prevent duplicate admission and
// advance the retry deadline atomically. Failed/local attempts leave the quota
// intact. Successes expire individually; the admission token never resets.
func ClaimProviderUrlProbeDue(ctx context.Context, now time.Time, limit, shardIndex, shardCount int) []ProviderUrlProbeDue {
	providers := []ProviderUrlProbeDue{}
	if limit <= 0 || shardCount < 1 || ProviderUrlProbeSlotCount < shardCount || shardIndex < 0 || shardIndex >= shardCount {
		return providers
	}
	server.Tx(ctx, func(tx server.PgTx) {
		result, err := tx.Query(ctx, providerUrlProbeDueSql(shardIndex, shardCount), now.UTC(), ProvideModePublic, limit, shardCount, shardIndex,
			ProviderEgressProbeSuccessTarget, now.Add(ProviderEgressProbeAttemptBackoff).UTC())
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var provider ProviderUrlProbeDue
				var securityDestinations []byte
				server.Raise(result.Scan(&provider.ClientId, &provider.CycleStartedAt, &provider.SuccessesNeeded, &provider.OutcomeCount, &provider.CountryCode, &provider.Region, &securityDestinations))
				server.Raise(json.Unmarshal(securityDestinations, &provider.SecurityDestinations))
				providers = append(providers, provider)
			}
		})
	})
	return providers
}

// One shared statement is exercised by admission and its query-plan regression.
func providerUrlProbeDueSql(shardIndex, shardCount int) string {
	head := `WITH head AS MATERIALIZED (
				SELECT cycle.client_id, cycle.next_attempt_at
				FROM provider_egress_probe_cycle AS cycle
				WHERE cycle.eligible AND cycle.next_attempt_at <= $1
				AND $4=1 AND $5=0
				ORDER BY cycle.next_attempt_at, cycle.client_id
				LIMIT $3
				FOR UPDATE OF cycle SKIP LOCKED
			)`
	if shardCount > 1 {
		// Ordered UNION branches permit Merge Append to advance only the slot
		// holding the next oldest row. A lateral join followed by Sort would
		// eagerly read owned_slots*limit rows even for one small admission.
		branches := []string{}
		for slot := shardIndex; slot < ProviderUrlProbeSlotCount; slot += shardCount {
			branches = append(branches, fmt.Sprintf(`(SELECT cycle.client_id,cycle.next_attempt_at
				FROM provider_egress_probe_cycle AS cycle
				WHERE cycle.eligible AND cycle.slot_id=%d AND cycle.next_attempt_at<=$1
				ORDER BY cycle.next_attempt_at,cycle.client_id LIMIT $3)`, slot))
		}
		head = fmt.Sprintf(`WITH slot_candidates AS MATERIALIZED (
				%s ORDER BY next_attempt_at,client_id LIMIT $3
			), head AS MATERIALIZED (
				SELECT cycle.client_id,cycle.next_attempt_at
				FROM slot_candidates JOIN provider_egress_probe_cycle AS cycle USING (client_id)
				WHERE cycle.eligible AND cycle.next_attempt_at<=$1 AND $4=%d AND $5=%d
				ORDER BY slot_candidates.next_attempt_at,slot_candidates.client_id LIMIT $3
				FOR UPDATE OF cycle SKIP LOCKED
			)`, strings.Join(branches, " UNION ALL "), shardCount, shardIndex)
	}
	return fmt.Sprintf(head+`, candidates AS MATERIALIZED (
				SELECT head.client_id, head.next_attempt_at
				FROM head
				JOIN network_client_location_reliability AS provider USING (client_id)
				JOIN network_client AS client USING (client_id)
				WHERE true
				AND provider.connected AND provider.valid
				AND (provider.ipv4_proven OR NOT provider.ipv6_proven)
				AND client.active AND client.source_client_id IS NULL
				AND EXISTS (SELECT 1 FROM provide_key AS key
					WHERE key.client_id=head.client_id AND key.provide_mode=$2)
				AND %s
			), measured AS MATERIALIZED (
				SELECT candidates.*, recent.success_count, recent.oldest_success_at,
					%s AS security_exception
				FROM candidates CROSS JOIN LATERAL (%s) AS recent
			), claimed AS (
				UPDATE provider_egress_probe_cycle AS cycle SET
					success_count = measured.success_count,
					next_attempt_at = CASE WHEN measured.success_count >= $6 AND NOT measured.security_exception
						THEN measured.oldest_success_at + interval '%d seconds' ELSE $7 END
				FROM measured WHERE cycle.client_id=measured.client_id
				RETURNING cycle.client_id, cycle.cycle_started_at, cycle.success_count, cycle.outcome_count,
					measured.security_exception
			)
			SELECT claimed.client_id, claimed.cycle_started_at, GREATEST(0, $6-claimed.success_count),
				claimed.outcome_count,
				COALESCE(country.country_code, ''), COALESCE(region.location_name, ''),
				COALESCE((SELECT jsonb_agg(security.destination ORDER BY security.url_key)
					FROM provider_egress_url_security AS security
					WHERE security.client_id=claimed.client_id AND security.tls_failure), '[]'::jsonb)
			FROM claimed
			JOIN candidates USING (client_id)
			JOIN network_client_location_reliability AS provider USING (client_id)
			LEFT JOIN location AS country ON country.location_id=provider.country_location_id
			LEFT JOIN location AS region ON region.location_id=provider.region_location_id
			WHERE claimed.success_count < $6 OR claimed.security_exception
			ORDER BY candidates.next_attempt_at, claimed.client_id
		`, providerProbeEligibilitySql("provider"), providerHasUrlSecurityExceptionSql("candidates.client_id"),
		providerUrlProbeSuccessWindowSql("candidates.client_id", "$1"), int(ProviderEgressProbeRefreshAge/time.Second))
}
