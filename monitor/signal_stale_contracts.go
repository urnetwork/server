package monitor

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const staleContractRange = "5 minutes"

// Signal stale-contracts implements SIGNALS.md §2.20. It measures the durable
// success-side invariant paired with §2.18: a contract must never be created
// after its destination was already marked inactive.
func NewStaleContractsSignal() Signal {
	return &signalAdapter{
		number: "2.20", key: "stale-contracts", name: "Successful contracts to inactive destinations",
		probe: staleContractsProbe{},
	}
}

type staleContractsProbe struct{}

func (staleContractsProbe) id() string             { return "pg/stale-contracts" }
func (staleContractsProbe) tier() string           { return tierPage }
func (staleContractsProbe) cadence() time.Duration { return time.Minute }

type staleContractObservation struct {
	total                 int64
	sameNetwork           int64
	destinationDerived    int64
	sourceActiveTop       int64
	distinctDestinations  int64
	distinctSources       int64
	medianInactiveSeconds int64
	p95InactiveSeconds    int64
}

func (staleContractsProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	rows, err := env.runner.pg(ctx, `
		WITH stale_success AS MATERIALIZED (
			SELECT
				tc.source_id,
				tc.destination_id,
				tc.source_network_id = tc.destination_network_id AS same_network,
				destination.source_client_id IS NOT NULL AS destination_derived,
				source.active AND source.source_client_id IS NULL AS source_active_top,
				extract(epoch FROM tc.create_time - destination.deactivate_time) AS inactive_seconds
			FROM transfer_contract tc
			JOIN network_client destination ON destination.client_id = tc.destination_id
			LEFT JOIN network_client source ON source.client_id = tc.source_id
			WHERE
				tc.create_time >= now() - interval '5 minutes' AND
				tc.companion_contract_id IS NULL AND
				NOT destination.active AND
				destination.deactivate_time IS NOT NULL AND
				destination.deactivate_time <= tc.create_time
		)
		SELECT
			count(*),
			count(*) FILTER (WHERE same_network),
			count(*) FILTER (WHERE destination_derived),
			count(*) FILTER (WHERE source_active_top IS TRUE),
			count(DISTINCT destination_id),
			count(DISTINCT source_id),
			coalesce(percentile_cont(0.5) WITHIN GROUP (ORDER BY inactive_seconds), 0)::bigint,
			coalesce(percentile_cont(0.95) WITHIN GROUP (ORDER BY inactive_seconds), 0)::bigint
		FROM stale_success;
	`)
	if err != nil {
		return nil, err
	}
	observation, err := parseStaleContractObservation(rows)
	if err != nil {
		return nil, err
	}
	target := pgTarget(env)
	if observation.total == 0 {
		return []finding{healthyFinding(
			"pg/stale-contracts", tierPage, "stale-contract-success", target,
		)}, nil
	}

	crossNetwork := observation.total - observation.sameNetwork
	destinationTop := observation.total - observation.destinationDerived
	sourceOther := observation.total - observation.sourceActiveTop
	return []finding{{
		probeId: "pg/stale-contracts", tier: tierPage,
		class: "stale-contract-success", target: target, frame: "inactive-before-create", sustain: 1,
		symptom: fmt.Sprintf(
			"%d successful non-companion contracts in the last five minutes targeted destinations already inactive before creation",
			observation.total,
		),
		mechanism: "The API accepted a destination after its durable client lifecycle had ended. The destination's recorded deactivate_time is no later than the contract create_time, so this excludes a healthy contract whose destination disconnected only after creation. A stale provide advertisement or return-path reference can otherwise authorize work that the destination can no longer receive.",
		baseline:  "Zero successful contracts are created after their destination's recorded deactivation; stale attempts are rejected by the API lifecycle guard before mode selection and again at the write boundary.",
		observed: fmt.Sprintf(
			"successful_contracts=%d range=%q noncompanion_only=true same_network=%d cross_network=%d destination_derived=%d destination_top=%d source_active_top=%d source_other=%d distinct_destinations=%d distinct_sources=%d median_inactive_before_create_s=%d p95_inactive_before_create_s=%d",
			observation.total,
			staleContractRange,
			observation.sameNetwork,
			crossNetwork,
			observation.destinationDerived,
			destinationTop,
			observation.sourceActiveTop,
			sourceOther,
			observation.distinctDestinations,
			observation.distinctSources,
			observation.medianInactiveSeconds,
			observation.p95InactiveSeconds,
		),
		evidence: "PostgreSQL joins only the recent successful contract cohort to the current source and destination lifecycle rows. It exports bounded counts and deactivation-age quantiles; no client, network, connection, contract, or destination identifier leaves the database.",
		context:  "This is an affirmative contract-correctness failure, not merely a high rejection rate, provider-score-cache contamination, or a Proxy hardware-capacity alert. Same-network plus derived-destination dominance identifies a stale return-path cohort; cross-network dominance points to a different producer. Failed missing-origin requests are not present in transfer_contract and remain covered by §2.17.",
		action:   "Use §8.12 to compare every API artifact with server commit c8dfe570. Satisfy the selected artifact's append-only migration prerequisite, then deploy the lifecycle guard everywhere it is absent. Deploy Connect-bearing clients containing the matching Reliability route-retirement behavior separately to remove retrying stale channels. If a proven current API still creates one of these rows, preserve the aggregate cohort and treat it as a guard regression. Do not delete contract rows, inactive clients, or Redis provide keys to manufacture zero.",
		verify:   "Every API artifact contains c8dfe570; two consecutive five-minute cohorts contain zero successful contracts whose destination was already inactive; §2.18 exposes both initialized rejection partitions; and a Reliability result retires only its emitting client route before refill.",
		playbook: "SIGNALS.md §2.20, §2.18, §2.17, and §8.12",
	}}, nil
}

func parseStaleContractObservation(rows []pgRow) (staleContractObservation, error) {
	if len(rows) != 1 || len(rows[0]) < 8 {
		return staleContractObservation{}, fmt.Errorf("stale contracts query returned %d malformed rows", len(rows))
	}
	values := make([]int64, 8)
	for i := range values {
		value, err := strconv.ParseInt(strings.TrimSpace(rows[0].str(i)), 10, 64)
		if err != nil || value < 0 {
			return staleContractObservation{}, fmt.Errorf("stale contracts query returned invalid column %d value %q", i, rows[0].str(i))
		}
		values[i] = value
	}
	observation := staleContractObservation{
		total:                 values[0],
		sameNetwork:           values[1],
		destinationDerived:    values[2],
		sourceActiveTop:       values[3],
		distinctDestinations:  values[4],
		distinctSources:       values[5],
		medianInactiveSeconds: values[6],
		p95InactiveSeconds:    values[7],
	}
	for name, value := range map[string]int64{
		"same_network":          observation.sameNetwork,
		"destination_derived":   observation.destinationDerived,
		"source_active_top":     observation.sourceActiveTop,
		"distinct_destinations": observation.distinctDestinations,
		"distinct_sources":      observation.distinctSources,
	} {
		if value > observation.total {
			return staleContractObservation{}, fmt.Errorf("stale contracts query returned %s=%d above total=%d", name, value, observation.total)
		}
	}
	if observation.medianInactiveSeconds > observation.p95InactiveSeconds {
		return staleContractObservation{}, fmt.Errorf(
			"stale contracts query returned median inactive age %d above p95 %d",
			observation.medianInactiveSeconds,
			observation.p95InactiveSeconds,
		)
	}
	return observation, nil
}
