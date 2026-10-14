// Shared provider reliability and security gates for selection and probe admission.
package model

import (
	"context"
	"fmt"

	"github.com/urnetwork/server"
)

// Missing reliability history is neutral for serving (URL probe admission
// requires a witness lookback score); every observed lookback must pass its floor.
func providerReliabilityMinimums() map[int]float64 {
	if NormalNetworkConditions() {
		return map[int]float64{1: 0.95, 2: 0.7, 3: 0.6}
	}
	return map[int]float64{1: 0.8, 2: 0.6, 3: 0.6}
}

// Tests the same observed lookbacks used by the SQL admission predicate.
func providerReliabilityPasses(weights map[int]float64, minimums map[int]float64) bool {
	for lookback, weight := range weights {
		if weight < minimums[lookback] {
			return false
		}
	}
	return true
}

// The caller supplies a trusted SQL expression, never request text.
func providerReliabilityEligibilitySql(clientIdExpression string) string {
	minimums := providerReliabilityMinimums()
	return fmt.Sprintf(`NOT EXISTS (
		SELECT 1 FROM client_connection_reliability_score AS provider_reliability
		WHERE provider_reliability.client_id = %s
		AND provider_reliability.independent_reliability_weight < CASE provider_reliability.lookback_index
			WHEN 1 THEN %g WHEN 2 THEN %g WHEN 3 THEN %g ELSE 0 END
	)`, clientIdExpression, minimums[1], minimums[2], minimums[3])
}

// The caller enforces active identity, connected/valid location and provide-key scope.
// Non-quality and ordinary probe failures do not exclude URL probes or online supply.
func providerEgressEligibilitySql(rollupAlias string) string {
	return fmt.Sprintf(`%s AND NOT (%s)`, providerProbeEligibilitySql(rollupAlias), providerHasUrlSecurityExceptionSql(rollupAlias+".client_id"))
}

// Indexed exact-URL findings and the legacy snapshot both quarantine serving;
// neither may suppress the URL rechecks needed to recover the provider.
func providerHasUrlSecurityExceptionSql(clientExpression string) string {
	return fmt.Sprintf(`EXISTS (
		SELECT 1 FROM provider_egress_health AS provider_security
		WHERE provider_security.client_id = %s
		AND (provider_security.tls_authentication_failure OR provider_security.legacy_tls_authentication_failure)
	) OR EXISTS (
		SELECT 1 FROM provider_egress_url_security AS url_security
		WHERE url_security.client_id = %s AND url_security.tls_failure
	)`, clientExpression, clientExpression)
}

// Security quarantine affects serving traffic but must remain eligible for a clean recheck.
func providerProbeEligibilitySql(rollupAlias string) string {
	return fmt.Sprintf(`NOT %s.arin_risk AND %s`, rollupAlias, providerReliabilityEligibilitySql(rollupAlias+".client_id"))
}

// The one hour lookback (ClientLookbacks index 1) witnesses current history.
// The windows nest (5 minutes, one hour, 12 hours) and a lookback's score row
// is deleted once its window holds no observation, so a provider offline for
// over an hour keeps only its 12 hour row, which can still pass.
const providerReliabilityWitnessLookbackIndex = 1

// True when the client has a witness lookback score. The caller supplies a
// trusted SQL expression; the score primary key serves the exact lookup.
func providerReliabilityWitnessSql(clientIdExpression string) string {
	return fmt.Sprintf(`EXISTS (
		SELECT 1 FROM client_connection_reliability_score AS provider_witness
		WHERE provider_witness.client_id = %s AND provider_witness.lookback_index = %d
	)`, clientIdExpression, providerReliabilityWitnessLookbackIndex)
}

// URL probe admission needs current reliability history: a witness lookback
// score. The location pass commits before the score pass and reads the previous
// generation. Admitting missing history as neutral there would count and claim
// a new connection until its first failing scores, and start its cycle hours
// before its real admission; a provider back after more than an hour offline
// would be judged on its 12 hour row alone until the score pass rewrote its
// hour. The first cycle opens at the first admission that sees passing history:
// the score pass that writes it, or a later location pass or public-mode change.
// An intent provider in its qualification grace is admitted without history
// (provider_intent_model.go): the grace is judged on probes, and a new
// provider's 12 hour weight stays under the floor for hours. Serving
// eligibility (providerEgressEligibilitySql) keeps missing history neutral.
func providerUrlProbeAdmissionSql(rollupAlias string) string {
	return fmt.Sprintf(
		`NOT %s.arin_risk AND ((%s AND %s) OR %s)`,
		rollupAlias,
		providerReliabilityWitnessSql(rollupAlias+".client_id"),
		providerReliabilityEligibilitySql(rollupAlias+".client_id"),
		providerIntentProbePrioritySql(rollupAlias+".client_id"),
	)
}

// Public-mode changes already own a client transaction. Read only that client's
// admission inputs; missing location still waits for the ordinary rollup, and
// missing current reliability history for the score pass.
func providerUrlProbeClientEligibilitySql() string {
	return `SELECT EXISTS (
		SELECT 1 FROM network_client_location_reliability AS provider_location
		JOIN network_client AS provider USING (client_id)
		WHERE provider_location.client_id = $1
		AND provider_location.connected AND provider_location.valid
		AND (provider_location.ipv4_proven OR NOT provider_location.ipv6_proven)
		AND provider.active AND provider.source_client_id IS NULL
		AND EXISTS (SELECT 1 FROM provide_key WHERE provide_key.client_id = $1 AND provide_mode = $2)
		AND ` + providerUrlProbeAdmissionSql("provider_location") + `)`
}

// Repair the changed identity before publishing its provide keys. Preserve all
// measurement, claim and pacing state; fleet reconciliation remains the backstop
// for admission inputs changed by other owners.
func updateProviderUrlProbeEligibilityForClient(ctx context.Context, tx server.PgTx, clientId server.Id) {
	var eligible bool
	server.Raise(tx.QueryRow(ctx, providerUrlProbeClientEligibilitySql(), clientId, ProvideModePublic).Scan(&eligible))
	if eligible {
		server.RaisePgResult(tx.Exec(ctx, `
			WITH reactivated AS (
				UPDATE provider_egress_probe_cycle SET eligible = true
				WHERE client_id = $1 AND NOT eligible
			)
			INSERT INTO provider_egress_probe_cycle (client_id, cycle_started_at, next_attempt_at, eligible)
			SELECT $1, $2, $2, true
			WHERE NOT EXISTS (SELECT 1 FROM provider_egress_probe_cycle WHERE client_id = $1)
			ON CONFLICT (client_id) DO NOTHING`, clientId, server.NowUtc()))
	} else {
		server.RaisePgResult(tx.Exec(ctx, `
			UPDATE provider_egress_probe_cycle SET eligible = false
			WHERE client_id = $1 AND eligible`, clientId))
	}
}

// Synchronize indexed scheduling with location and score publication. A first
// passing history or a score recovery can admit a provider that had no cycle at
// the preceding location pass, or reactivate a false hint. Keep this maintenance
// outside claim calls; existing tokens, progress and retry deadlines never move.
func updateProviderUrlProbeEligibility(ctx context.Context, tx server.PgTx) {
	server.RaisePgResult(tx.Exec(ctx, `
		INSERT INTO provider_egress_probe_cycle (client_id, cycle_started_at, next_attempt_at, eligible)
		SELECT provider_location.client_id, $1, LEAST($1::timestamp, COALESCE((
			SELECT intent_priority.priority_since FROM provider_intent_probe_priority AS intent_priority
			WHERE intent_priority.client_id = provider_location.client_id
		), $1::timestamp)), true
		FROM network_client_location_reliability AS provider_location
		JOIN network_client AS provider USING (client_id)
		WHERE provider_location.connected AND provider_location.valid
		AND (provider_location.ipv4_proven OR NOT provider_location.ipv6_proven)
		AND provider.active AND provider.source_client_id IS NULL
		AND NOT EXISTS (SELECT 1 FROM provider_egress_probe_cycle AS cycle WHERE cycle.client_id = provider_location.client_id)
		AND EXISTS (SELECT 1 FROM provide_key WHERE provide_key.client_id = provider_location.client_id AND provide_mode = $2)
		AND `+providerUrlProbeAdmissionSql("provider_location")+`
		ON CONFLICT (client_id) DO NOTHING`, server.NowUtc(), ProvideModePublic))

	// Claims still recheck current gates. Reconcile both admission and rejection
	// in the same transaction as the scores that determine their eligibility.
	server.RaisePgResult(tx.Exec(ctx, `
		WITH eligibility AS (
			SELECT cycle.client_id, EXISTS (
				SELECT 1 FROM network_client_location_reliability AS provider_location
				JOIN network_client AS provider USING (client_id)
				WHERE provider_location.client_id = cycle.client_id
				AND provider_location.connected AND provider_location.valid
				AND (provider_location.ipv4_proven OR NOT provider_location.ipv6_proven)
				AND provider.active AND provider.source_client_id IS NULL
				AND EXISTS (SELECT 1 FROM provide_key WHERE provide_key.client_id = cycle.client_id AND provide_mode = $1)
				AND `+providerUrlProbeAdmissionSql("provider_location")+`
			) AS eligible FROM provider_egress_probe_cycle AS cycle
		)
		UPDATE provider_egress_probe_cycle AS cycle SET eligible = eligibility.eligible
		FROM eligibility WHERE cycle.client_id = eligibility.client_id
		AND cycle.eligible IS DISTINCT FROM eligibility.eligible`, ProvideModePublic))
}
