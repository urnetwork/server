package monitor

import (
	"bytes"
	"context"
	"encoding/gob"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	providerEligibilityReadyKey   = "client_score_provider_eligibility_v1_ready"
	providerEligibilityReadyValue = "1"
)

// SIGNALS.md §2.9 maps to signal_selection_population.go and signal_selection_population_test.go.
func NewSelectionPopulationSignal() Signal {
	return &signalAdapter{number: "2.9", key: "selection-population", name: "Provider-selection population", probe: pgSelectionPopulationProbe{}}
}

type pgSelectionPopulationProbe struct{}

func (pgSelectionPopulationProbe) id() string             { return "pg/selection-empty" }
func (pgSelectionPopulationProbe) tier() string           { return tierPage }
func (pgSelectionPopulationProbe) cadence() time.Duration { return 5 * time.Minute }

func (pgSelectionPopulationProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	redisHost := env.cfg.hostByRole("redis-cluster")
	if redisHost == nil {
		return nil, fmt.Errorf("no redis-cluster host in inventory")
	}
	schemaRows, err := env.runner.pg(ctx, `
		SELECT EXISTS (
		 SELECT 1 FROM pg_attribute
		 WHERE attrelid='provider_egress_health'::regclass
		   AND attname='tls_authentication_failure'
		   AND NOT attisdropped
		);
	`)
	if err != nil {
		return nil, err
	}
	if len(schemaRows) != 1 || len(schemaRows[0]) != 1 {
		return nil, fmt.Errorf("provider population TLS-integrity schema query returned an invalid shape")
	}
	tlsIntegrityArmed, err := strconv.ParseBool(schemaRows[0].str(0))
	if err != nil {
		return nil, fmt.Errorf("provider population query returned invalid TLS-integrity arming state %q", schemaRows[0].str(0))
	}
	tlsPassingPredicate := ""
	tlsFailureCount := "0"
	if tlsIntegrityArmed {
		// Keep the pre-migration query parseable while allowing PostgreSQL to use
		// the partial boolean index once the append-only column is present. Row
		// conversion through to_jsonb would be schema-compatible but would also
		// serialize every wide health row on every five-minute observation.
		tlsPassingPredicate = "AND NOT peh.tls_authentication_failure"
		tlsFailureCount = "(SELECT count(*) FROM provider_egress_health WHERE tls_authentication_failure)"
	}
	rows, err := env.runner.pg(ctx, fmt.Sprintf(`
		WITH supply AS MATERIALIZED (
		 SELECT nc.active, nc.source_client_id
		 FROM network_client_location_reliability nclr
		 INNER JOIN network_client nc USING (client_id)
		 WHERE nclr.connected AND nclr.valid
		   AND EXISTS (
		    SELECT 1 FROM provide_key pk
		    WHERE pk.client_id=nclr.client_id AND pk.provide_mode IN (1,3)
		   )
		)
		SELECT
		 (SELECT count(DISTINCT ncc.client_id) FROM network_client_connection ncc
		  WHERE ncc.connected AND EXISTS (SELECT 1 FROM provide_key pk WHERE pk.client_id=ncc.client_id AND pk.provide_mode=3)),
		 (SELECT count(*) FROM supply),
		 (SELECT count(*) FROM supply WHERE active AND source_client_id IS NULL),
		 (SELECT count(*) FROM supply WHERE source_client_id IS NOT NULL),
		 (SELECT count(*) FROM supply WHERE NOT active AND source_client_id IS NULL),
		 (SELECT count(*) FROM provider_egress_health),
		 (SELECT count(*) FROM provider_egress_health peh
		  WHERE measured_at>=now()-interval '24 hours'
		    AND total_count>0 AND 10*ok_count>=9*total_count
		    %s),
		 (SELECT count(*) FROM provider_egress_location),
		 %s,
		 (SELECT location_id::text FROM location WHERE location_type='country' AND country_code='us' LIMIT 1);
	`, tlsPassingPredicate, tlsFailureCount))
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 || len(rows[0]) < 10 || rows[0].str(9) == "" {
		return nil, fmt.Errorf("provider population query returned no target location")
	}
	connected := atoiRow(rows[0], 0)
	rawSupply := atoiRow(rows[0], 1)
	eligible := atoiRow(rows[0], 2)
	derived := atoiRow(rows[0], 3)
	inactive := atoiRow(rows[0], 4)
	tlsAuthenticationFailures := atoiRow(rows[0], 8)
	targetLocation := rows[0].str(9)
	caller := "00000000-0000-0000-0000-000000000000"
	normalKey := fmt.Sprintf("{cs_0_q_%s_%s}c_l", caller, targetLocation)
	forcedKey := fmt.Sprintf("{cs_1_q_%s_%s}c_l", caller, targetLocation)
	normalRaw, err := env.runner.redisRaw(ctx, redisHost, redisHost.redisEntryPort, "-c", "--raw", "GET", normalKey)
	if err != nil {
		return nil, err
	}
	forcedRaw, err := env.runner.redisRaw(ctx, redisHost, redisHost.redisEntryPort, "-c", "--raw", "GET", forcedKey)
	if err != nil {
		return nil, err
	}
	normalCount, normalErr := decodeProviderCount([]byte(normalRaw))
	forcedCount, forcedErr := decodeProviderCount([]byte(forcedRaw))
	if normalErr != nil || forcedErr != nil {
		return nil, fmt.Errorf("decode provider score cache: normal=%v forced=%v", normalErr, forcedErr)
	}
	eligibilityMarker, err := env.runner.redis(
		ctx,
		redisHost,
		redisHost.redisEntryPort,
		"-c", "--raw", "GET", providerEligibilityReadyKey,
	)
	if err != nil {
		return nil, fmt.Errorf("read provider eligibility marker: %w", err)
	}
	eligibilityReady := strings.TrimSpace(eligibilityMarker) == providerEligibilityReadyValue

	findings := []finding{}
	if !eligibilityReady && derived+inactive > 0 {
		findings = append(findings, finding{
			probeId: "pg/selection-empty", tier: tierPage,
			class: "provider-supply-ineligible", target: pgTarget(env), frame: "legacy-filter", sustain: 1,
			symptom: fmt.Sprintf(
				"provider-score input contains %d derived and %d inactive providing clients without a completed eligibility-filter export",
				derived,
				inactive,
			),
			mechanism: "The legacy score queries trusted connected location rows without joining the durable client lifecycle. Derived window identities and inactive top-level clients could therefore be exported as provider supply; feeding short-lived consumer identities back into destination selection amplifies replacement churn. A completed current writer publishes the durable marker only after filtering both classes from every location and location-group export.",
			baseline:  "Only active top-level clients enter provider-score caches, and client_score_provider_eligibility_v1_ready=1 proves a complete filtered export.",
			observed: fmt.Sprintf(
				"raw_providing_supply=%d eligible_active_top_level=%d derived_providing=%d inactive_top_level_providing=%d eligibility_ready=%t normal_cache_count=%d forced_cache_count=%d tls_integrity_armed=%t tls_authentication_failures=%d",
				rawSupply,
				eligible,
				derived,
				inactive,
				eligibilityReady,
				normalCount,
				forcedCount,
				tlsIntegrityArmed,
				tlsAuthenticationFailures,
			),
			evidence: "PostgreSQL aggregates connected/valid clients holding Network or Public provide keys by active and source-client state. Redis supplies only the fixed rollout marker and aggregate decoded cache counts; no client identifier leaves either source.",
			context:  "Raw derived or inactive rows may remain for history after the fix, so their existence alone must not keep this alert open once a current, fully converged Taskworker completes the filtered export. The marker does not replace runtime provenance: an old Taskworker can still overwrite caches during a partial rollout.",
			action:   "Deploy every Taskworker from a Server descendant of b7599962 that contains the provider-eligibility ready marker. Let the existing serialized UpdateClientScores task complete and publish the marker; do not delete client, location, score-cache, or provide-key state manually.",
			verify:   "Every active Taskworker has the required ancestry, one post-convergence UpdateClientScores run completes, the marker equals 1, bounded cache samples contain only active top-level clients, and §2.7 child churn plus destination diversity recover for two mature cohorts.",
			playbook: "SIGNALS.md §2.9 and §2.16",
		})
	} else {
		findings = append(findings, healthyFinding(
			"pg/selection-empty", tierPage, "provider-supply-ineligible", pgTarget(env),
		))
	}
	if eligible <= 1000 || normalCount > 0 {
		findings = append(findings, healthyFinding("pg/selection-empty", tierPage, "selection-empty", pgTarget(env)))
		return findings, nil
	}
	mode := "upstream-empty"
	mechanism := "Both normal and ForceMinimum exports are empty, so provider supply/location input is empty before score minimums are applied."
	if forcedCount > 0 {
		mode = "gate-wipe"
		mechanism = "Connected eligible providers reach the score writer and ForceMinimum export, but a normal reliability, score, or egress predicate removes the entire market."
	}
	findings = append(findings, finding{
		probeId: "pg/selection-empty", tier: tierPage,
		class: "selection-empty", target: pgTarget(env), frame: mode, sustain: 2,
		symptom:   fmt.Sprintf("eligible score candidates=%d but normal score-cache export=%d (ForceMinimum=%d)", eligible, normalCount, forcedCount),
		mechanism: mechanism,
		baseline:  "The normal decoded provider sum is nonzero and tracks eligible supply; ForceMinimum is normally larger.",
		observed: fmt.Sprintf("connected=%d raw_supply=%d eligible=%d derived=%d inactive=%d eligibility_ready=%t normal=%d forced=%d egress_health=%s fresh_passing_excluding_tls=%s egress_locations=%s tls_integrity_armed=%t tls_authentication_failures=%d target=%s",
			connected, rawSupply, eligible, derived, inactive, eligibilityReady, normalCount, forcedCount, rows[0].str(5), rows[0].str(6), rows[0].str(7), tlsIntegrityArmed, tlsAuthenticationFailures, targetLocation),
		evidence: fmt.Sprintf("normal_key=%s bytes=%d; forced_key=%s bytes=%d; TLS-integrity evidence is aggregate-only and the compatibility query does not require the pending column to exist", normalKey, len(normalRaw), forcedKey, len(forcedRaw)),
		action:   "Split the score predicates and inspect the deployed provider.yml enable_egress_test value before changing provider connectivity or cache TTLs.",
		verify:   "A fresh UpdateClientScores run produces a nonzero decoded normal cache count consistent with eligible supply.",
		playbook: "SIGNALS.md §2.9 and §5.9",
	})
	return findings, nil
}

func decodeProviderCount(raw []byte) (int, error) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "" || strings.TrimSpace(string(raw)) == "(nil)" {
		return 0, nil
	}
	var counts []int
	if err := gob.NewDecoder(bytes.NewReader(raw)).Decode(&counts); err != nil {
		return 0, err
	}
	total := 0
	for _, count := range counts {
		total += count
	}
	return total, nil
}
