package monitor

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

const (
	staleContractRange                   = "5 minutes"
	staleContractTimestampBoundaryMs     = 1000.0
	staleContractSuccessClass            = "stale-contract-success"
	staleContractTimestampAmbiguousClass = "stale-contract-timestamp-ambiguous"
)

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
	total                      int64
	sameNetwork                int64
	destinationDerived         int64
	sourceActiveTop            int64
	distinctDestinations       int64
	distinctSources            int64
	medianInactiveMilliseconds float64
	p95InactiveMilliseconds    float64
	sameDistinctDestinations   int64
	sameDistinctDestParents    int64
	sameDistinctDestDevices    int64
	sameDistinctSources        int64
	sameDistinctSourceDevices  int64
	sameDistinctNetworks       int64
	crossDestinationTop        int64
	crossSourceDerived         int64
	crossSourceParentActive    int64
	crossDistinctDestinations  int64
	crossDistinctSources       int64
	crossDistinctSourceParents int64
	crossDistinctSourceDevices int64
}

func (staleContractsProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	rows, err := env.runner.pg(ctx, `
		WITH stale_success AS MATERIALIZED (
			SELECT
				tc.source_id,
				tc.destination_id,
				tc.source_network_id,
				tc.source_network_id = tc.destination_network_id AS same_network,
				destination.source_client_id IS NOT NULL AS destination_derived,
				destination.source_client_id AS destination_parent_id,
				destination.device_id AS destination_device_id,
				source.source_client_id IS NOT NULL AS source_derived,
				source.source_client_id AS source_parent_id,
				source.device_id AS source_device_id,
				source_parent.active AS source_parent_active,
				source.active AND source.source_client_id IS NULL AS source_active_top,
				extract(epoch FROM tc.create_time - destination.deactivate_time) * 1000 AS inactive_milliseconds,
				CASE
					WHEN tc.create_time - destination.deactivate_time < interval '1 second'
						THEN 'stale-contract-timestamp-ambiguous'
					ELSE 'stale-contract-success'
				END AS boundary_class
			FROM transfer_contract tc
			JOIN network_client destination ON destination.client_id = tc.destination_id
			LEFT JOIN network_client source ON source.client_id = tc.source_id
			LEFT JOIN network_client source_parent ON source_parent.client_id = source.source_client_id
			WHERE
				tc.create_time >= now() - interval '5 minutes' AND
				tc.companion_contract_id IS NULL AND
				NOT destination.active AND
				destination.deactivate_time IS NOT NULL AND
				destination.deactivate_time <= tc.create_time
		)
		SELECT
			boundary_class,
			count(*),
			count(*) FILTER (WHERE same_network),
			count(*) FILTER (WHERE destination_derived),
			count(*) FILTER (WHERE source_active_top IS TRUE),
			count(DISTINCT destination_id),
			count(DISTINCT source_id),
			coalesce(round(percentile_cont(0.5) WITHIN GROUP (ORDER BY inactive_milliseconds)::numeric, 3), 0),
			coalesce(round(percentile_cont(0.95) WITHIN GROUP (ORDER BY inactive_milliseconds)::numeric, 3), 0),
			count(DISTINCT destination_id) FILTER (WHERE same_network),
			count(DISTINCT destination_parent_id) FILTER (WHERE same_network),
			count(DISTINCT destination_device_id) FILTER (WHERE same_network),
			count(DISTINCT source_id) FILTER (WHERE same_network),
			count(DISTINCT source_device_id) FILTER (WHERE same_network),
			count(DISTINCT source_network_id) FILTER (WHERE same_network),
			count(*) FILTER (WHERE NOT same_network AND NOT destination_derived),
			count(*) FILTER (WHERE NOT same_network AND source_derived),
			count(*) FILTER (WHERE NOT same_network AND source_parent_active IS TRUE),
			count(DISTINCT destination_id) FILTER (WHERE NOT same_network),
			count(DISTINCT source_id) FILTER (WHERE NOT same_network),
			count(DISTINCT source_parent_id) FILTER (WHERE NOT same_network),
			count(DISTINCT source_device_id) FILTER (WHERE NOT same_network)
		FROM stale_success
		GROUP BY boundary_class
		ORDER BY boundary_class;
	`)
	if err != nil {
		return nil, err
	}
	observations, err := parseStaleContractObservations(rows)
	if err != nil {
		return nil, err
	}
	target := pgTarget(env)
	findings := []finding{}
	if observation, ok := observations[staleContractSuccessClass]; ok {
		findings = append(findings, staleContractSuccessFinding(observation, target))
	} else {
		findings = append(findings, healthyFinding("pg/stale-contracts", tierPage, staleContractSuccessClass, target))
	}
	if observation, ok := observations[staleContractTimestampAmbiguousClass]; ok {
		findings = append(findings, staleContractTimestampAmbiguousFinding(observation, target))
	} else {
		findings = append(findings, healthyFinding("pg/stale-contracts", tierPage, staleContractTimestampAmbiguousClass, target))
	}
	return findings, nil
}

func staleContractSuccessFinding(observation staleContractObservation, target string) finding {
	return finding{
		probeId: "pg/stale-contracts", tier: tierPage,
		class: staleContractSuccessClass, target: target, frame: "inactive-before-create", sustain: 1,
		symptom: fmt.Sprintf(
			"%d successful non-companion contracts in the last five minutes targeted destinations recorded inactive at least one second before creation",
			observation.total,
		),
		mechanism: "Under the fleet's bounded clock discipline, a destination recorded inactive at least one second before contract creation is operationally classified as stale acceptance outside the conservative subsecond legacy-clock band. The timestamp is not transaction-order proof for an arbitrarily skewed legacy host, so clock health and writer provenance remain required controls. A stale provide advertisement or return-path reference can otherwise authorize work that the destination can no longer receive.",
		baseline:  "Zero successful contracts have inactive-before-create ordering; stale attempts are rejected by the API lifecycle guard before mode selection and again at the write boundary.",
		observed:  staleContractObserved(observation, "at-least-one-second"),
		evidence:  "PostgreSQL joins only the recent successful contract cohort to the current source and destination lifecycle rows, partitions subsecond from at-least-one-second timestamp ordering, and exports bounded counts plus exact-millisecond deactivation-age quantiles; no client, network, connection, contract, or destination identifier leaves the database.",
		context:   "This is an operational contract-correctness page under the fleet clock envelope, not merely a high rejection rate, provider-score-cache contamination, or a Proxy hardware-capacity alert. It is not transaction-order proof if an unbounded legacy-clock fault is still possible. Same-network plus derived-destination dominance identifies a stale return-path cohort; its bounded network, source-device, and destination-parent/device cardinalities distinguish one concentrated relationship/window boundary from distributed fleet churn without exporting identities. Cross-network rows to inactive top-level destinations from derived sources whose parents remain active can identify a retained Public client route; concentration into one destination and one parent/device distinguishes one window churning derived identities from fleet-wide cache contamination, but requires a bounded current-cache control before assignment. Failed missing-origin requests are not present in transfer_contract and remain covered by §2.17.",
		action:    "Preserve the aggregate cohort and use §8.12 to verify the fleet clock envelope and compare every API artifact first with transactional lifecycle serialization commit 883d39c8 and then with the database-clock lifecycle boundary that stamps contract creation and deactivation from PostgreSQL after their row locks. The older c8dfe570 pre-selection guard alone is not closure. If the current fleet contains both boundaries, audit for a production insert or deactivation writer that bypasses the locked model paths. Deploy Connect-bearing clients containing the matching Reliability route-retirement behavior separately to remove retrying stale channels. Compare same-network cardinalities to decide whether one relationship/window or multiple networks are producing stale returns. For a concentrated cross-network cohort, use its bounded current-cache control; do not call one retained route global provider-cache contamination. Do not delete contract rows, inactive clients, or Redis provide keys to manufacture zero.",
		verify:    "Every API artifact contains transactional lifecycle serialization, and every API and Taskworker artifact contains the PostgreSQL-clock timestamp boundary; after the last writer converges, two consecutive five-minute cohorts contain neither affirmative nor subsecond-ambiguous inactive-before-create rows; §2.18 exposes both initialized rejection partitions; and a Reliability result retires only its emitting client route before refill.",
		playbook:  "SIGNALS.md §2.20, §2.18, §2.17, and §8.12",
	}
}

func staleContractTimestampAmbiguousFinding(observation staleContractObservation, target string) finding {
	finding := staleContractSuccessFinding(observation, target)
	finding.class = staleContractTimestampAmbiguousClass
	finding.frame = "subsecond-order-ambiguous"
	finding.symptom = fmt.Sprintf(
		"%d successful non-companion contracts in the last five minutes have subsecond create/deactivate timestamp ordering that cannot prove which transaction won",
		observation.total,
	)
	finding.mechanism = "Application hosts historically supplied both transfer_contract.create_time and network_client.deactivate_time. A subsecond positive difference can therefore be produced by cross-host clock offset even when PostgreSQL row locks serialized the contract before deactivation; the timestamps alone do not prove stale acceptance."
	finding.observed = staleContractObserved(observation, "subsecond-ambiguous")
	finding.context = "This immediate page preserves the zero-success lifecycle invariant without falsely attributing a subsecond application-clock inversion to the API guard. If every running writer already uses the PostgreSQL primary clock, a new row instead means an unreviewed writer bypasses the serialized boundary or the boundary regressed; it must not be dismissed as NTP noise. Failed missing-origin requests remain covered by §2.17."
	finding.action = "Preserve the aggregate and use §8.12 to prove every API artifact contains transactional lifecycle serialization commit 883d39c8. Then prove API and Taskworker artifacts stamp both contract creation and client deactivation with the PostgreSQL primary clock after their lifecycle row locks. Deploy only an owning artifact that lacks that database-clock correction. Audit every production transfer_contract INSERT and active=false writer if a database-clock generation still emits this class. Do not call the subsecond row a proven stale acceptance, widen the ambiguity band, or delete durable rows to clear the page."
	return finding
}

func staleContractObserved(observation staleContractObservation, timestampClass string) string {
	crossNetwork := observation.total - observation.sameNetwork
	destinationTop := observation.total - observation.destinationDerived
	sourceOther := observation.total - observation.sourceActiveTop
	return fmt.Sprintf(
		"successful_contracts=%d range=%q noncompanion_only=true timestamp_class=%q timestamp_boundary_ms=%.3f same_network=%d cross_network=%d destination_derived=%d destination_top=%d source_active_top=%d source_other=%d distinct_destinations=%d distinct_sources=%d median_inactive_before_create_ms=%.3f p95_inactive_before_create_ms=%.3f same_distinct_destinations=%d same_distinct_destination_parents=%d same_distinct_destination_devices=%d same_distinct_sources=%d same_distinct_source_devices=%d same_distinct_networks=%d cross_destination_top=%d cross_source_derived=%d cross_source_parent_active=%d cross_distinct_destinations=%d cross_distinct_sources=%d cross_distinct_source_parents=%d cross_distinct_source_devices=%d",
		observation.total,
		staleContractRange,
		timestampClass,
		staleContractTimestampBoundaryMs,
		observation.sameNetwork,
		crossNetwork,
		observation.destinationDerived,
		destinationTop,
		observation.sourceActiveTop,
		sourceOther,
		observation.distinctDestinations,
		observation.distinctSources,
		observation.medianInactiveMilliseconds,
		observation.p95InactiveMilliseconds,
		observation.sameDistinctDestinations,
		observation.sameDistinctDestParents,
		observation.sameDistinctDestDevices,
		observation.sameDistinctSources,
		observation.sameDistinctSourceDevices,
		observation.sameDistinctNetworks,
		observation.crossDestinationTop,
		observation.crossSourceDerived,
		observation.crossSourceParentActive,
		observation.crossDistinctDestinations,
		observation.crossDistinctSources,
		observation.crossDistinctSourceParents,
		observation.crossDistinctSourceDevices,
	)
}

func parseStaleContractObservations(rows []pgRow) (map[string]staleContractObservation, error) {
	if 2 < len(rows) {
		return nil, fmt.Errorf("stale contracts query returned %d rows, want at most 2", len(rows))
	}
	observations := map[string]staleContractObservation{}
	for rowIndex, row := range rows {
		if len(row) != 22 {
			return nil, fmt.Errorf("stale contracts query returned malformed row %d with %d columns", rowIndex, len(row))
		}
		class := strings.TrimSpace(row.str(0))
		if class != staleContractSuccessClass && class != staleContractTimestampAmbiguousClass {
			return nil, fmt.Errorf("stale contracts query returned invalid class %q", class)
		}
		if _, duplicate := observations[class]; duplicate {
			return nil, fmt.Errorf("stale contracts query returned duplicate class %q", class)
		}
		observation, err := parseStaleContractObservation(row)
		if err != nil {
			return nil, err
		}
		if class == staleContractTimestampAmbiguousClass && staleContractTimestampBoundaryMs <= observation.p95InactiveMilliseconds {
			return nil, fmt.Errorf("stale contracts ambiguous p95 %.3fms reaches boundary %.3fms", observation.p95InactiveMilliseconds, staleContractTimestampBoundaryMs)
		}
		if class == staleContractSuccessClass && observation.medianInactiveMilliseconds < staleContractTimestampBoundaryMs {
			return nil, fmt.Errorf("stale contracts affirmative median %.3fms is below boundary %.3fms", observation.medianInactiveMilliseconds, staleContractTimestampBoundaryMs)
		}
		observations[class] = observation
	}
	return observations, nil
}

func parseStaleContractObservation(row pgRow) (staleContractObservation, error) {
	values := make([]int64, 19)
	for valueIndex, columnIndex := range []int{1, 2, 3, 4, 5, 6, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21} {
		value, err := strconv.ParseInt(strings.TrimSpace(row.str(columnIndex)), 10, 64)
		if err != nil || value < 0 {
			return staleContractObservation{}, fmt.Errorf("stale contracts query returned invalid column %d value %q", columnIndex, row.str(columnIndex))
		}
		values[valueIndex] = value
	}
	medianMilliseconds, err := strconv.ParseFloat(strings.TrimSpace(row.str(7)), 64)
	if err != nil || medianMilliseconds < 0 || math.IsNaN(medianMilliseconds) || math.IsInf(medianMilliseconds, 0) {
		return staleContractObservation{}, fmt.Errorf("stale contracts query returned invalid column 7 value %q", row.str(7))
	}
	p95Milliseconds, err := strconv.ParseFloat(strings.TrimSpace(row.str(8)), 64)
	if err != nil || p95Milliseconds < 0 || math.IsNaN(p95Milliseconds) || math.IsInf(p95Milliseconds, 0) {
		return staleContractObservation{}, fmt.Errorf("stale contracts query returned invalid column 8 value %q", row.str(8))
	}
	observation := staleContractObservation{
		total:                      values[0],
		sameNetwork:                values[1],
		destinationDerived:         values[2],
		sourceActiveTop:            values[3],
		distinctDestinations:       values[4],
		distinctSources:            values[5],
		medianInactiveMilliseconds: medianMilliseconds,
		p95InactiveMilliseconds:    p95Milliseconds,
		sameDistinctDestinations:   values[6],
		sameDistinctDestParents:    values[7],
		sameDistinctDestDevices:    values[8],
		sameDistinctSources:        values[9],
		sameDistinctSourceDevices:  values[10],
		sameDistinctNetworks:       values[11],
		crossDestinationTop:        values[12],
		crossSourceDerived:         values[13],
		crossSourceParentActive:    values[14],
		crossDistinctDestinations:  values[15],
		crossDistinctSources:       values[16],
		crossDistinctSourceParents: values[17],
		crossDistinctSourceDevices: values[18],
	}
	if observation.total == 0 {
		return staleContractObservation{}, fmt.Errorf("stale contracts query returned empty grouped row")
	}
	crossNetwork := observation.total - observation.sameNetwork
	for name, value := range map[string]int64{
		"same_network":                      observation.sameNetwork,
		"destination_derived":               observation.destinationDerived,
		"source_active_top":                 observation.sourceActiveTop,
		"distinct_destinations":             observation.distinctDestinations,
		"distinct_sources":                  observation.distinctSources,
		"same_distinct_destinations":        observation.sameDistinctDestinations,
		"same_distinct_destination_parents": observation.sameDistinctDestParents,
		"same_distinct_destination_devices": observation.sameDistinctDestDevices,
		"same_distinct_sources":             observation.sameDistinctSources,
		"same_distinct_source_devices":      observation.sameDistinctSourceDevices,
		"same_distinct_networks":            observation.sameDistinctNetworks,
		"cross_destination_top":             observation.crossDestinationTop,
		"cross_source_derived":              observation.crossSourceDerived,
		"cross_source_parent_active":        observation.crossSourceParentActive,
		"cross_distinct_destinations":       observation.crossDistinctDestinations,
		"cross_distinct_sources":            observation.crossDistinctSources,
		"cross_distinct_source_parents":     observation.crossDistinctSourceParents,
		"cross_distinct_source_devices":     observation.crossDistinctSourceDevices,
	} {
		if value > observation.total {
			return staleContractObservation{}, fmt.Errorf("stale contracts query returned %s=%d above total=%d", name, value, observation.total)
		}
	}
	for name, value := range map[string]int64{
		"same_distinct_destinations":        observation.sameDistinctDestinations,
		"same_distinct_destination_parents": observation.sameDistinctDestParents,
		"same_distinct_destination_devices": observation.sameDistinctDestDevices,
		"same_distinct_sources":             observation.sameDistinctSources,
		"same_distinct_source_devices":      observation.sameDistinctSourceDevices,
		"same_distinct_networks":            observation.sameDistinctNetworks,
	} {
		if value > observation.sameNetwork {
			return staleContractObservation{}, fmt.Errorf("stale contracts query returned %s=%d above same_network=%d", name, value, observation.sameNetwork)
		}
	}
	for name, value := range map[string]int64{
		"cross_destination_top":         observation.crossDestinationTop,
		"cross_source_derived":          observation.crossSourceDerived,
		"cross_source_parent_active":    observation.crossSourceParentActive,
		"cross_distinct_destinations":   observation.crossDistinctDestinations,
		"cross_distinct_sources":        observation.crossDistinctSources,
		"cross_distinct_source_parents": observation.crossDistinctSourceParents,
		"cross_distinct_source_devices": observation.crossDistinctSourceDevices,
	} {
		if value > crossNetwork {
			return staleContractObservation{}, fmt.Errorf("stale contracts query returned %s=%d above cross_network=%d", name, value, crossNetwork)
		}
	}
	for _, check := range []struct {
		name  string
		value int64
		max   int64
	}{
		{name: "same_distinct_destinations", value: observation.sameDistinctDestinations, max: observation.distinctDestinations},
		{name: "same_distinct_sources", value: observation.sameDistinctSources, max: observation.distinctSources},
		{name: "same_distinct_destination_parents", value: observation.sameDistinctDestParents, max: observation.sameDistinctDestinations},
		{name: "same_distinct_destination_devices", value: observation.sameDistinctDestDevices, max: observation.sameDistinctDestinations},
		{name: "same_distinct_source_devices", value: observation.sameDistinctSourceDevices, max: observation.sameDistinctSources},
		{name: "same_distinct_networks", value: observation.sameDistinctNetworks, max: observation.sameDistinctSources},
		{name: "cross_distinct_destinations", value: observation.crossDistinctDestinations, max: observation.distinctDestinations},
		{name: "cross_distinct_sources", value: observation.crossDistinctSources, max: observation.distinctSources},
		{name: "cross_distinct_source_parents", value: observation.crossDistinctSourceParents, max: observation.crossDistinctSources},
		{name: "cross_distinct_source_devices", value: observation.crossDistinctSourceDevices, max: observation.crossDistinctSources},
	} {
		if check.value > check.max {
			return staleContractObservation{}, fmt.Errorf("stale contracts query returned %s=%d above enclosing count=%d", check.name, check.value, check.max)
		}
	}
	if observation.medianInactiveMilliseconds > observation.p95InactiveMilliseconds {
		return staleContractObservation{}, fmt.Errorf(
			"stale contracts query returned median inactive age %.3fms above p95 %.3fms",
			observation.medianInactiveMilliseconds,
			observation.p95InactiveMilliseconds,
		)
	}
	return observation, nil
}
