// Indexed maintenance makes rolling completion priority exact before admission.
package model

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/urnetwork/server/v2026"
)

const (
	providerUrlProbeExpiryClients       = 128
	providerUrlProbeExpiryRunsPerClient = 32
	providerUrlProbePromoteLimit        = 4096
)

// Branches are constants from validated slot geometry, never user SQL. An
// empty shard seeks only owned slots; merging stops after the requested head.
func providerUrlProbeMaintenancePrefix(predicate, timeColumn string, shardIndex, shardCount int) string {
	if shardCount == 1 {
		return fmt.Sprintf(`WITH head AS MATERIALIZED (
			SELECT cycle.client_id,cycle.%s FROM provider_egress_probe_cycle AS cycle
			WHERE %s ORDER BY cycle.%s,cycle.client_id LIMIT $2
			FOR UPDATE OF cycle SKIP LOCKED
		)`, timeColumn, predicate, timeColumn)
	}
	branches := []string{}
	for slot := shardIndex; slot < ProviderUrlProbeSlotCount; slot += shardCount {
		branches = append(branches, fmt.Sprintf(`(SELECT cycle.client_id,cycle.%s
			FROM provider_egress_probe_cycle AS cycle WHERE cycle.slot_id=%d AND (%s)
			ORDER BY cycle.%s,cycle.client_id LIMIT $2)`, timeColumn, slot, predicate, timeColumn))
	}
	return fmt.Sprintf(`WITH slot_candidates AS MATERIALIZED (
		%s ORDER BY %s,client_id LIMIT $2
	), head AS MATERIALIZED (
		SELECT cycle.client_id,cycle.%s FROM slot_candidates
		CROSS JOIN LATERAL (
			SELECT cycle.client_id,cycle.%s FROM provider_egress_probe_cycle AS cycle
			WHERE cycle.client_id=slot_candidates.client_id AND (%s)
			OFFSET 0 FOR UPDATE OF cycle SKIP LOCKED
		) AS cycle
		ORDER BY slot_candidates.%s,slot_candidates.client_id LIMIT $2
	)`, strings.Join(branches, " UNION ALL "), timeColumn, timeColumn, timeColumn, predicate, timeColumn)
}

// Pending includes temporarily locked rows. Skipping a busy maintenance owner
// cannot certify an exact priority window or relabel backlog as zero due work.
func providerUrlProbeMaintenancePendingSql(predicate, timeColumn string, shardIndex, shardCount int) string {
	if shardCount == 1 {
		return fmt.Sprintf(`WITH pending AS MATERIALIZED (
			SELECT 1 FROM provider_egress_probe_cycle AS cycle WHERE %s
			ORDER BY cycle.%s,cycle.client_id LIMIT 1
		) SELECT EXISTS(SELECT 1 FROM pending)`, predicate, timeColumn)
	}
	branches := []string{}
	for slot := shardIndex; slot < ProviderUrlProbeSlotCount; slot += shardCount {
		branches = append(branches, fmt.Sprintf(`(SELECT 1 FROM provider_egress_probe_cycle AS cycle
			WHERE cycle.slot_id=%d AND (%s)
			ORDER BY cycle.%s,cycle.client_id LIMIT 1)`, slot, predicate, timeColumn))
	}
	return `WITH pending AS MATERIALIZED (` + strings.Join(branches, " UNION ALL ") + `) SELECT EXISTS(SELECT 1 FROM pending)`
}

// Locks at most128 provider rows and retires at most32 active receipts for each.
// Longer per-provider histories remain pending for another bounded invocation.
// Statement-local TIDs restrict receipt writes; the owning cycle locks serialize
// every counted receipt writer, so a selected active tuple cannot move meanwhile.
func providerUrlProbeExpirySql(shardIndex, shardCount int) string {
	return providerUrlProbeMaintenancePrefix(
		"cycle.completed_next_expiry_at<=$1", "completed_next_expiry_at", shardIndex, shardCount) + fmt.Sprintf(`,
	expired AS MATERIALIZED (
		SELECT run.client_id,run.claim_ordinal,run.ctid FROM head
		CROSS JOIN LATERAL (
			SELECT client_id,claim_ordinal,ctid FROM provider_url_probe_run AS run
			WHERE run.client_id=head.client_id AND run.counted
			AND run.completed_at<=$1::timestamp-interval '4 hours'
			ORDER BY run.completed_at,run.claim_ordinal LIMIT %d
		) AS run
	), retired AS (
		UPDATE provider_url_probe_run AS run SET counted=false FROM expired
		WHERE run.client_id=expired.client_id AND run.claim_ordinal=expired.claim_ordinal
		AND run.ctid=ANY(ARRAY(SELECT ctid FROM expired))
		RETURNING run.client_id,run.claim_ordinal
	), adjusted AS (
		UPDATE provider_egress_probe_cycle AS cycle SET
			completed_run_count=cycle.completed_run_count-
				(SELECT COUNT(*) FROM retired WHERE retired.client_id=cycle.client_id),
			completed_next_expiry_at=(
				SELECT run.completed_at+interval '4 hours' FROM provider_url_probe_run AS run
				WHERE run.client_id=cycle.client_id AND run.counted
				AND NOT EXISTS (SELECT 1 FROM expired
					WHERE expired.client_id=run.client_id AND expired.claim_ordinal=run.claim_ordinal)
				ORDER BY run.completed_at,run.claim_ordinal LIMIT 1
			),
			completed_priority_ready=false
		FROM head WHERE cycle.client_id=head.client_id
		AND cycle.client_id=ANY(ARRAY(SELECT client_id FROM head))
		RETURNING cycle.client_id
	)
	SELECT COUNT(*) FROM adjusted`, providerUrlProbeExpiryRunsPerClient)
}

// Existing pacing writes invalidate readiness through the narrow trigger. Only
// due, eligible rows with fully current counts enter the indexed priority set.
func providerUrlProbePromoteSql(shardIndex, shardCount int) string {
	return providerUrlProbeMaintenancePrefix(
		"cycle.eligible AND NOT cycle.completed_priority_ready AND cycle.next_attempt_at<=$1",
		"next_attempt_at", shardIndex, shardCount) + `, promoted AS (
		UPDATE provider_egress_probe_cycle AS cycle SET completed_priority_ready=true
		FROM head WHERE cycle.client_id=head.client_id
		AND cycle.client_id=ANY(ARRAY(SELECT client_id FROM head))
		AND (cycle.completed_next_expiry_at IS NULL OR cycle.completed_next_expiry_at>$1)
		RETURNING cycle.client_id
	) SELECT COUNT(*) FROM promoted`
}

// One fixed observation time covers maintenance and the following claim.
// False means more bounded work is required, not that the due cohort is empty.
func maintainProviderUrlProbeCompletedPriority(ctx context.Context, tx server.PgTx, now time.Time, shardIndex, shardCount int, priority bool, observation *ProviderUrlProbeDueObservation) bool {
	var changed int
	observation.measure(ProviderUrlProbeDueExpiry, func() {
		server.Raise(tx.QueryRow(ctx, providerUrlProbeExpirySql(shardIndex, shardCount),
			now.UTC(), providerUrlProbeExpiryClients).Scan(&changed))
	})
	var pending bool
	observation.measure(ProviderUrlProbeDueExpiryPending, func() {
		server.Raise(tx.QueryRow(ctx, providerUrlProbeMaintenancePendingSql(
			"cycle.completed_next_expiry_at<=$1", "completed_next_expiry_at", shardIndex, shardCount), now.UTC()).Scan(&pending))
	})
	if pending {
		return false
	}
	if !priority {
		return true
	}
	observation.measure(ProviderUrlProbeDuePromote, func() {
		server.Raise(tx.QueryRow(ctx, providerUrlProbePromoteSql(shardIndex, shardCount),
			now.UTC(), providerUrlProbePromoteLimit).Scan(&changed))
	})
	observation.measure(ProviderUrlProbeDuePromotePending, func() {
		server.Raise(tx.QueryRow(ctx, providerUrlProbeMaintenancePendingSql(
			"cycle.eligible AND NOT cycle.completed_priority_ready AND cycle.next_attempt_at<=$1",
			"next_attempt_at", shardIndex, shardCount), now.UTC()).Scan(&pending))
	})
	return !pending
}
