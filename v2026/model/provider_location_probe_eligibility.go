package model

import (
	"context"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Compute the fleet admission inputs before the location upsert takes row
// locks. The prospective rollup contains every connected location; rows absent
// from it become disconnected below and therefore cannot admit a URL probe.
// Keep the calculation and publication in the caller's transaction so failures
// still roll back locations and scheduling hints together.
func prepareLocationProbeEligibility(ctx context.Context, tx server.PgTx) time.Time {
	now := server.NowUtc()
	server.RaisePgResult(tx.Exec(ctx, `
		CREATE TEMPORARY TABLE temp_location_probe_eligibility ON COMMIT DROP AS
		WITH admitted AS (
		SELECT provider_location.client_id, LEAST($1::timestamp, COALESCE((
			SELECT intent_priority.priority_since FROM provider_intent_probe_priority AS intent_priority
			WHERE intent_priority.client_id=provider_location.client_id
		), $1::timestamp)) AS next_attempt_at
		FROM (
			SELECT location.*, (location.country_location_id IS NOT NULL
				AND location.client_address_hash_count=1 AND location.location_count=1) AS valid
			FROM temp_network_client_location_reliability AS location
		) AS provider_location
		JOIN network_client AS provider USING(client_id)
		WHERE provider_location.connected AND provider_location.valid
		AND (provider_location.ipv4_proven OR NOT provider_location.ipv6_proven)
		AND provider.active AND provider.source_client_id IS NULL
		AND EXISTS (SELECT 1 FROM provide_key
			WHERE provide_key.client_id=provider_location.client_id AND provide_mode=$2)
		AND `+providerUrlProbeAdmissionSql("provider_location")+`
		)
		SELECT COALESCE(cycle.client_id,admitted.client_id) AS client_id,
			admitted.client_id IS NOT NULL AS eligible,
			cycle.eligible AS previous_eligible,admitted.next_attempt_at
		FROM provider_egress_probe_cycle AS cycle
		FULL JOIN admitted USING(client_id)`, now, ProvideModePublic))
	// The validity expression above is the rollup table's generated-column
	// rule; parity controls compare this publication to that actual column.
	server.RaisePgResult(tx.Exec(ctx, `ALTER TABLE temp_location_probe_eligibility ADD PRIMARY KEY(client_id)`))
	server.RaisePgResult(tx.Exec(ctx, `ANALYZE temp_location_probe_eligibility`))
	return now
}

// Only indexed prepared identities remain after the location writes. Existing
// cycle identity, counters, claims and pacing are not reset. As with the normal
// fleet reconciler, a claim always rechecks current authoritative admission.
func publishLocationProbeEligibility(ctx context.Context, tx server.PgTx, now time.Time) {
	server.RaisePgResult(tx.Exec(ctx, `
		INSERT INTO provider_egress_probe_cycle(client_id,cycle_started_at,next_attempt_at,eligible)
		SELECT prepared.client_id,$1,prepared.next_attempt_at,true
		FROM temp_location_probe_eligibility AS prepared
		WHERE prepared.eligible
		AND NOT EXISTS (SELECT 1 FROM provider_egress_probe_cycle AS cycle WHERE cycle.client_id=prepared.client_id)
		ON CONFLICT(client_id) DO NOTHING`, now))
	// A per-client writer may have published a new admission or rejection while
	// this transaction was preparing or waiting for a location row. Preserve
	// that changed hint. The snapshot includes rejected cycles too; new cycles
	// have no previous hint and are left to the insert/conflict owner above.
	// This is a hint-change fence, not a serializable authority snapshot: claims
	// must still recheck current gates, including changes that leave a hint equal.
	server.RaisePgResult(tx.Exec(ctx, `
		UPDATE provider_egress_probe_cycle AS cycle SET eligible=prepared.eligible
		FROM temp_location_probe_eligibility AS prepared
		WHERE cycle.client_id=prepared.client_id AND prepared.previous_eligible IS NOT NULL
		AND cycle.eligible IS NOT DISTINCT FROM prepared.previous_eligible
		AND cycle.eligible IS DISTINCT FROM prepared.eligible`))
}
