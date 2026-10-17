// Provider e2e key-publication coverage (SIGNALS.md 15.1).
//
// A DeviceLocal provider enables the responder side of e2e encryption and
// publishes its TLS certificate through EncryptedKey. Ordinary outbound and
// return-path clients can also create network_client_connection/provide_key
// rows, so they are not a valid denominator for provider rollout coverage.
package monitor

import (
	"context"
	"fmt"
	"strconv"
	"time"
)

// SIGNALS.md §15.1 maps to signal_key_publication.go and
// signal_key_publication_test.go.
func NewKeyPublicationSignal() Signal {
	return &signalAdapter{number: "15.1", key: "key-publication", name: "E2E key-publication coverage", probe: pgE2eKeyProbe{}}
}

type pgE2eKeyProbe struct{}

const (
	e2eCoverageMetricPrefix       = "pg/e2e-provider-key-coverage"
	e2eCoverageMinimumCohort      = 100
	e2eCoverageMinimumMedian      = 0.05
	e2eCoverageBaselineSamples    = 12
	e2eCoverageBaselineWindow     = 24 * time.Hour
	e2eCoverageBaselineMinimumAge = 45 * time.Minute
	keyPublicationStateVersion    = 1
)

var e2eProvideModeNames = map[int]string{
	1: "network",
	2: "friends",
	3: "public",
}

// keyPublicationQuery keeps the rollout denominator on explicit provider
// modes. ProvideMode_Stream (4) is intentionally absent: every ordinary
// client advertises that mode for return traffic, including clients whose
// transfer encryption is off. ConnectNetworkClient either sets auth_time to
// connect_time or proves it was already no more than one hour old in the same
// transaction. The inclusive two-hour auth_time range is therefore a lossless,
// index-backed prefilter for the exact one-hour connection EXISTS. It prevents
// PostgreSQL from hashing the full recent connection table while preserving
// the provider population and avoiding child-client churn.
const keyPublicationQuery = `
WITH provider_clients AS MATERIALIZED (
    SELECT pk.client_id, pk.provide_mode
    FROM provide_key pk
    INNER JOIN network_client nc USING (client_id)
    WHERE pk.provide_mode IN (1, 2, 3)
      AND nc.active
      AND nc.source_client_id IS NULL
      AND nc.auth_time >= now() - interval '2 hours'
      AND EXISTS (
          SELECT 1
          FROM network_client_connection ncc
          WHERE ncc.client_id = pk.client_id
            AND ncc.connect_time >= now() - interval '1 hour'
      )
), provider_counts AS (
    SELECT
        pc.provide_mode,
        count(*) AS active,
        count(ctc.client_id) AS covered
    FROM provider_clients pc
    LEFT JOIN client_tls_certificate ctc USING (client_id)
    GROUP BY pc.provide_mode
), current_certificates AS MATERIALIZED (
    SELECT DISTINCT pc.client_id, ctc.set_time
    FROM provider_clients pc
    INNER JOIN client_tls_certificate ctc USING (client_id)
), freshness AS (
    SELECT
        count(*) FILTER (WHERE set_time >= now() - interval '15 minutes') AS fresh_15m,
        count(*) FILTER (WHERE set_time >= now() - interval '1 hour') AS fresh_1h,
        COALESCE(extract(epoch FROM now() - max(set_time))::bigint, -1) AS newest_age_seconds
    FROM current_certificates
)
SELECT
    modes.provide_mode,
    COALESCE(provider_counts.active, 0),
    COALESCE(provider_counts.covered, 0),
    freshness.fresh_15m,
    freshness.fresh_1h,
    freshness.newest_age_seconds
FROM (VALUES (1), (2), (3)) AS modes(provide_mode)
LEFT JOIN provider_counts USING (provide_mode)
CROSS JOIN freshness
ORDER BY modes.provide_mode;
`

type e2eCoverageObservation struct {
	mode      int
	modeName  string
	active    int64
	covered   int64
	fresh15m  int64
	fresh1h   int64
	newestAge int64
}

type keyPublicationPersistedState struct {
	CoverageArmed    bool    `json:"coverage_armed"`
	CoverageBaseline float64 `json:"coverage_baseline"`
}

func validateKeyPublicationState(state keyPublicationPersistedState) error {
	if !state.CoverageArmed {
		if state.CoverageBaseline != 0 {
			return fmt.Errorf("unarmed key-publication state has a coverage baseline")
		}
		return nil
	}
	if state.CoverageBaseline < e2eCoverageMinimumMedian || 1 < state.CoverageBaseline {
		return fmt.Errorf("key-publication state has an invalid armed coverage baseline")
	}
	return nil
}

func parseE2ECoverageCount(row pgRow, column int, name string) (int64, error) {
	if column >= len(row) {
		return 0, fmt.Errorf("e2e key-publication row is missing %s", name)
	}
	value, err := strconv.ParseInt(row.str(column), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("e2e key-publication row has invalid %s %q", name, row.str(column))
	}
	return value, nil
}

func parseE2ECoverageRows(rows []pgRow) ([]e2eCoverageObservation, error) {
	if len(rows) != len(e2eProvideModeNames) {
		return nil, fmt.Errorf("e2e key-publication query returned %d rows, want %d", len(rows), len(e2eProvideModeNames))
	}

	observations := make([]e2eCoverageObservation, 0, len(rows))
	seenModes := map[int]bool{}
	var common *e2eCoverageObservation
	for rowIndex, row := range rows {
		if len(row) != 6 {
			return nil, fmt.Errorf("e2e key-publication row %d has %d columns, want 6", rowIndex, len(row))
		}
		values := make([]int64, 6)
		names := []string{"provide_mode", "active", "covered", "fresh_15m", "fresh_1h", "newest_age_seconds"}
		for column := range values {
			value, err := parseE2ECoverageCount(row, column, names[column])
			if err != nil {
				return nil, err
			}
			values[column] = value
		}

		mode := int(values[0])
		modeName, ok := e2eProvideModeNames[mode]
		if !ok || seenModes[mode] {
			return nil, fmt.Errorf("e2e key-publication query returned unexpected or duplicate provide mode %d", mode)
		}
		seenModes[mode] = true
		observation := e2eCoverageObservation{
			mode: mode, modeName: modeName,
			active: values[1], covered: values[2],
			fresh15m: values[3], fresh1h: values[4], newestAge: values[5],
		}
		if observation.active < 0 || observation.covered < 0 || observation.covered > observation.active ||
			observation.fresh15m < 0 || observation.fresh1h < observation.fresh15m ||
			observation.newestAge < -1 || (observation.fresh1h > 0 && observation.newestAge < 0) {
			return nil, fmt.Errorf("e2e key-publication row %d contains inconsistent aggregate counts", rowIndex)
		}
		if common != nil && (observation.fresh15m != common.fresh15m ||
			observation.fresh1h != common.fresh1h || observation.newestAge != common.newestAge) {
			return nil, fmt.Errorf("e2e key-publication rows contain inconsistent common controls")
		}
		if common == nil {
			copy := observation
			common = &copy
		}
		observations = append(observations, observation)
	}
	return observations, nil
}

func e2eCoverage(observation e2eCoverageObservation) float64 {
	if observation.active == 0 {
		return 0
	}
	return float64(observation.covered) / float64(observation.active)
}

func e2eModeControls(observations []e2eCoverageObservation) string {
	controls := ""
	for _, observation := range observations {
		controls += fmt.Sprintf(" %s_active=%d %s_covered=%d", observation.modeName, observation.active, observation.modeName, observation.covered)
	}
	return controls
}

func (self pgE2eKeyProbe) id() string             { return "pg/e2e-key-publication" }
func (self pgE2eKeyProbe) tier() string           { return tierWarn }
func (self pgE2eKeyProbe) cadence() time.Duration { return 5 * time.Minute }

func (self pgE2eKeyProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	target := "pg"
	if h := env.cfg.hostByRole("pg-primary"); h != nil {
		target = h.name
	}
	rows, err := env.runner.pg(ctx, keyPublicationQuery)
	if err != nil {
		return nil, err
	}
	observations, err := parseE2ECoverageRows(rows)
	if err != nil {
		return nil, err
	}

	// Public is the presently observable provider-eligibility boundary. Network
	// and Friends modes are retained as aggregate controls. An explicit
	// persisted capability marker can broaden the alert denominator later
	// without guessing from provider roles.
	public := e2eCoverageObservation{}
	for _, observation := range observations {
		if observation.mode == 3 {
			public = observation
			break
		}
	}
	if public.mode != 3 {
		return nil, fmt.Errorf("e2e key-publication query omitted Public providers")
	}
	common := observations[0]
	coverage := e2eCoverage(public)
	metric := fmt.Sprintf("%s/%s", e2eCoverageMetricPrefix, public.modeName)

	stateLock, err := lockProviderState(ctx, env.cfg.stateDir, "key-publication")
	if err != nil {
		return nil, err
	}
	defer stateLock.Close()
	state := keyPublicationPersistedState{}
	if _, err := loadProviderState(env.cfg.stateDir, "key-publication", keyPublicationStateVersion, &state); err != nil {
		return nil, err
	}
	if err := validateKeyPublicationState(state); err != nil {
		return nil, err
	}

	measuredMedian := 0.0
	medianAvailable := false
	if env.baseline != nil {
		if candidate, _, ok := env.baseline.trailingMedianSpanning(
			metric,
			e2eCoverageBaselineWindow,
			e2eCoverageBaselineSamples,
			e2eCoverageBaselineMinimumAge,
		); ok {
			measuredMedian = candidate
			medianAvailable = true
		}
	}
	eligible := e2eCoverageMinimumCohort <= public.active
	if !state.CoverageArmed && eligible && medianAvailable && e2eCoverageMinimumMedian <= measuredMedian {
		state.CoverageArmed = true
		state.CoverageBaseline = measuredMedian
	}
	if state.CoverageArmed && eligible && medianAvailable &&
		e2eCoverageMinimumMedian <= measuredMedian && 0.5*state.CoverageBaseline <= coverage {
		state.CoverageBaseline = measuredMedian
	}
	if env.baseline != nil && eligible &&
		(!state.CoverageArmed || 0.5*state.CoverageBaseline <= coverage) {
		// An undersized cohort and an active regression are not observations of
		// normal coverage. Recording either lets an outage erase its own learned
		// expectation and can manufacture a later recovery.
		env.baseline.record(metric, time.Now(), coverage)
	}
	if state.CoverageArmed {
		if err := saveProviderState(env.cfg.stateDir, "key-publication", keyPublicationStateVersion, state); err != nil {
			return nil, err
		}
	}
	median := state.CoverageBaseline
	findings := []finding{}
	controls := e2eModeControls(observations)

	publicationStalled := state.CoverageArmed && eligible && common.fresh15m == 0 &&
		(common.newestAge < 0 || int64((15*time.Minute)/time.Second) < common.newestAge)
	if publicationStalled {
		findings = append(findings, finding{
			probeId: "pg/e2e-key-publication", tier: tierWarn,
			class: "e2e-key-publication-stalled", target: target, frame: "provider-cohort", sustain: 3,
			symptom:   "E2E certificate publication stopped for more than 15 minutes in the established provider population.",
			mechanism: "No recently connected, active top-level provider certificate advanced inside the current 15-minute window. Freshness stays armed by durable established Public-provider coverage, so a complete outage cannot erase the alert after its last successful writes age out of the one-hour counter.",
			baseline:  "After Public-provider coverage establishes and persists the feature arm, the newest provider certificate publication remains within 15 minutes.",
			observed:  fmt.Sprintf("fresh_15m=%d fresh_1h=%d newest_age_seconds=%d coverage_baseline=%.3f%s", common.fresh15m, common.fresh1h, common.newestAge, median, controls),
			evidence:  "PostgreSQL returns aggregate mode-scoped eligible-provider counts, certificate counts, refresh counts, and newest-row age. No client, network, certificate, connection, or build identity leaves the database.",
			context:   "Correlate with bounded encrypted_key control-frame failures and exact Connect artifact ancestry. A mode-specific coverage change without this class does not prove the shared write path stopped.",
			action:    "Establish whether EncryptedKey frames stopped arriving, failed validation, or failed at the PostgreSQL upsert boundary before changing clients or services.",
			verify:    "For three consecutive five-minute samples, fresh_15m is positive, newest_age_seconds is at most 900, and the Network/Friends/Public controls remain observable.",
			playbook:  "SIGNALS.md §15.1",
		})
	}

	if !state.CoverageArmed || !eligible || coverage >= 0.5*median {
		return findings, nil
	}

	sharedPathLive := 0 < common.fresh15m && common.newestAge <= int64((15*time.Minute)/time.Second)
	mechanism := "Public providers' stored-certificate coverage fell below half its own established baseline. The eligible denominator excludes child, outbound, and Stream-only return-path clients, so their connection-cohort churn cannot create this ratio."
	action := "Segment Public providers by bounded connection service/build aggregates, correlate tls-cert-publish-invalid, and compare exact client/server artifact ancestry before assigning a fleet rollback or client regression."
	if sharedPathLive {
		mechanism += " Aggregate certificate refreshes remain current, which rules out a complete shared publication/store outage but not a Public-provider generation regression."
		action = "The shared publication/store path is live. Segment Public providers by bounded connection service/build aggregates and identify the regressed client generation; do not redeploy Connect solely from a changing connection cohort."
	}
	findings = append(findings, finding{
		probeId: "pg/e2e-key-publication", tier: tierWarn,
		class: "e2e-key-coverage", target: target, frame: "mode=public", sustain: 3,
		symptom: fmt.Sprintf(
			"E2E key coverage for Public providers is %.1f%% (%d/%d), below half its trailing baseline %.1f%%.",
			100*coverage, public.covered, public.active, 100*median),
		mechanism: mechanism,
		baseline:  "Recently connected Public-provider coverage retains at least half its own trailing 24-hour median; the median must be at least 5% and span at least 45 minutes before arming.",
		observed: fmt.Sprintf(
			"mode=public covered=%d active=%d coverage=%.3f median=%.3f fresh_15m=%d fresh_1h=%d newest_age_seconds=%d%s",
			public.covered, public.active, coverage, median,
			common.fresh15m, common.fresh1h, common.newestAge, controls),
		evidence: "PostgreSQL returns aggregate Public-provider counts, other mode controls, and shared refresh counts. No client, network, certificate, connection, or build identity leaves the database.",
		context:  "ProvideMode_Stream is deliberately excluded because ordinary clients advertise it for return traffic. Inactive and derived clients are excluded. Network and Friends remain non-causal controls until an explicit publisher-capability marker exists.",
		action:   action,
		verify:   "For three consecutive five-minute samples, Public-provider coverage is at least half its established median, shared refreshes remain current, and the Network/Friends controls remain observable.",
		playbook: "SIGNALS.md §15.1",
	})
	return findings, nil
}
