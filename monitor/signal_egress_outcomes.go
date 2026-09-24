package monitor

import (
	"context"
	"fmt"
	"time"

	"github.com/urnetwork/server/model"
)

// SIGNALS.md §2.23 maps to signal_egress_outcomes.go and
// signal_egress_outcomes_test.go. It reconstructs one privacy-bounded outcome
// for every currently eligible Public provider; it never diagnoses the
// survivor-biased attempt table by itself.
func NewEgressOutcomesSignal() Signal {
	return &signalAdapter{
		number: "2.23", key: "egress-outcomes", name: "Provider egress probe outcomes",
		probe: egressOutcomesProbe{},
	}
}

type egressOutcomesProbe struct{}

func (egressOutcomesProbe) id() string             { return "pg/egress-outcomes" }
func (egressOutcomesProbe) tier() string           { return tierPage }
func (egressOutcomesProbe) cadence() time.Duration { return 5 * time.Minute }

// egressOutcomesQuery returns one fixed-shape aggregate. The two durations are
// sourced from model constants so the monitor cannot silently drift from the
// production trust and retry contracts. Raw failure text and client identity
// are normalized before the final SELECT and can never leave PostgreSQL.
func egressOutcomesQuery() string {
	attemptSeconds := int64(model.ProviderEgressProbeAttemptBackoff / time.Second)
	locationSeconds := int64(model.ProviderEgressLocationMaxAge / time.Second)
	return fmt.Sprintf(`
/* monitor-signal-2.23-egress-outcomes */
WITH clock AS MATERIALIZED (
    SELECT now() AT TIME ZONE 'UTC' AS utc_now
), eligible AS MATERIALIZED (
    SELECT nclr.client_id
    FROM network_client_location_reliability nclr
    INNER JOIN network_client nc USING (client_id)
    WHERE nc.active
      AND nc.source_client_id IS NULL
      AND nclr.connected
      AND nclr.valid
      AND EXISTS (
          SELECT 1
          FROM provide_key pk
          WHERE pk.client_id = nclr.client_id
            AND pk.provide_mode = 3
      )
), joined AS MATERIALIZED (
    SELECT e.client_id,
           pea.client_id AS attempt_client_id,
           pea.probe_failure,
           pea.attempt_at,
           pea.update_time AS attempt_update,
           pel.client_id AS location_client_id,
           pel.observed_at,
           pel.update_time AS location_update,
           clock.utc_now,
           COALESCE(pea.attempt_at >= clock.utc_now - interval '%d seconds', false) AS attempt_current,
           COALESCE(pel.observed_at >= clock.utc_now - interval '%d seconds', false) AS location_current
    FROM eligible e
    CROSS JOIN clock
    LEFT JOIN provider_egress_probe_attempt pea USING (client_id)
    LEFT JOIN provider_egress_location pel USING (client_id)
), classified AS MATERIALIZED (
    SELECT joined.*,
           CASE
               WHEN attempt_current
                AND probe_failure <> ''
                AND (
                    NOT location_current OR
                    attempt_update > location_update
                )
               THEN CASE probe_failure
                   WHEN 'tunnel_failed' THEN 'tunnel_failed'
                   WHEN 'contract_failed' THEN 'contract_failed'
                   WHEN 'no_consensus' THEN 'no_consensus'
                   WHEN 'locate_failed' THEN 'locate_failed'
                   WHEN 'not_confident' THEN 'not_confident'
                   WHEN 'submit_failed' THEN 'submit_failed'
                   WHEN 'health_not_run' THEN 'health_not_run'
                   WHEN 'run_not_measured' THEN 'run_not_measured'
                   WHEN 'no_exit_ip' THEN 'no_exit_ip'
                   WHEN 'run_batch_guard' THEN 'run_batch_guard'
                   ELSE 'unknown_failure'
               END
               WHEN location_current
                AND (attempt_client_id IS NULL OR probe_failure = '')
               THEN ''
               WHEN attempt_current AND probe_failure = ''
               THEN 'inconsistent'
               ELSE 'unobserved'
           END AS outcome_class
    FROM joined
), states AS MATERIALIZED (
    SELECT classified.*,
           CASE
               WHEN outcome_class = '' THEN GREATEST(observed_at, attempt_at)
               WHEN outcome_class NOT IN ('unobserved', 'inconsistent') THEN attempt_at
               ELSE NULL
           END AS outcome_at
    FROM classified
), aggregate AS (
    SELECT count(*)::bigint AS eligible,
           count(*) FILTER (WHERE outcome_class NOT IN ('unobserved', 'inconsistent'))::bigint AS observed,
           count(*) FILTER (WHERE outcome_class = '')::bigint AS successes,
           count(*) FILTER (WHERE outcome_class NOT IN ('', 'unobserved', 'inconsistent'))::bigint AS failures,
           count(*) FILTER (WHERE outcome_class = 'tunnel_failed')::bigint AS tunnel_failed,
           count(*) FILTER (WHERE outcome_class = 'contract_failed')::bigint AS contract_failed,
           count(*) FILTER (WHERE outcome_class = 'no_consensus')::bigint AS no_consensus,
           count(*) FILTER (WHERE outcome_class = 'locate_failed')::bigint AS locate_failed,
           count(*) FILTER (WHERE outcome_class = 'not_confident')::bigint AS not_confident,
           count(*) FILTER (WHERE outcome_class = 'submit_failed')::bigint AS submit_failed,
           count(*) FILTER (WHERE outcome_class = 'health_not_run')::bigint AS health_not_run,
           count(*) FILTER (WHERE outcome_class = 'run_not_measured')::bigint AS run_not_measured,
           count(*) FILTER (WHERE outcome_class = 'no_exit_ip')::bigint AS no_exit_ip,
           count(*) FILTER (WHERE outcome_class = 'run_batch_guard')::bigint AS run_batch_guard,
           count(*) FILTER (WHERE outcome_class = 'unknown_failure')::bigint AS unknown_failure,
           count(*) FILTER (WHERE outcome_class = 'inconsistent')::bigint AS inconsistent,
           count(*) FILTER (WHERE outcome_class = 'unobserved')::bigint AS unobserved,
           CASE WHEN count(outcome_at) = 0 THEN -1::bigint
                ELSE GREATEST(0::bigint, floor(extract(epoch FROM (max(utc_now) - max(outcome_at))))::bigint)
           END AS newest_outcome_age_seconds,
           CASE WHEN count(outcome_at) = 0 THEN -1::bigint
                ELSE GREATEST(0::bigint, floor(extract(epoch FROM (max(utc_now) - min(outcome_at))))::bigint)
           END AS oldest_outcome_age_seconds
    FROM states
)
SELECT eligible::text,
       observed::text,
       successes::text,
       failures::text,
       tunnel_failed::text,
       contract_failed::text,
       no_consensus::text,
       locate_failed::text,
       not_confident::text,
       submit_failed::text,
       health_not_run::text,
       run_not_measured::text,
       no_exit_ip::text,
       run_batch_guard::text,
       unknown_failure::text,
       inconsistent::text,
       unobserved::text,
       newest_outcome_age_seconds::text,
       oldest_outcome_age_seconds::text
FROM aggregate;
`, attemptSeconds, locationSeconds)
}

type egressOutcomeSnapshot struct {
	eligible                int
	observed                int
	successes               int
	failures                int
	tunnelFailed            int
	contractFailed          int
	noConsensus             int
	locateFailed            int
	notConfident            int
	submitFailed            int
	healthNotRun            int
	runNotMeasured          int
	noExitIp                int
	runBatchGuard           int
	unknownFailure          int
	inconsistent            int
	unobserved              int
	newestOutcomeAgeSeconds int64
	oldestOutcomeAgeSeconds int64
}

func (s egressOutcomeSnapshot) tally() map[string]int {
	return map[string]int{
		model.ProbeAttemptSuccessClass:      s.successes,
		"tunnel_failed":                     s.tunnelFailed,
		"contract_failed":                   s.contractFailed,
		"no_consensus":                      s.noConsensus,
		"locate_failed":                     s.locateFailed,
		"not_confident":                     s.notConfident,
		"submit_failed":                     s.submitFailed,
		"health_not_run":                    s.healthNotRun,
		"run_not_measured":                  s.runNotMeasured,
		"no_exit_ip":                        s.noExitIp,
		model.ProbeRunBatchGuardClass:       s.runBatchGuard,
		model.ProbeFleetUnknownFailureClass: s.unknownFailure,
		model.ProbeFleetInconsistentClass:   s.inconsistent,
		model.ProbeFleetUnobservedClass:     s.unobserved,
	}
}

func parseEgressOutcomeSnapshot(rows []pgRow) (egressOutcomeSnapshot, error) {
	// seventeen counts, then the two ages, which are -1 when absent
	const countColumns = 17
	if len(rows) != 1 || len(rows[0]) != countColumns+2 {
		return egressOutcomeSnapshot{}, fmt.Errorf("provider egress outcomes returned an invalid aggregate shape")
	}
	values := make([]int64, countColumns+2)
	for index := range values {
		value, err := parseStrictInt64(rows[0].str(index))
		if err != nil || (index < countColumns && value < 0) || (countColumns <= index && value < -1) {
			return egressOutcomeSnapshot{}, fmt.Errorf("provider egress outcomes returned an invalid numeric field %d", index)
		}
		values[index] = value
	}
	maxInt := int64(^uint(0) >> 1)
	for index := 0; index < countColumns; index++ {
		if maxInt < values[index] {
			return egressOutcomeSnapshot{}, fmt.Errorf("provider egress outcomes field %d exceeds the local integer range", index)
		}
	}
	snapshot := egressOutcomeSnapshot{
		eligible: int(values[0]), observed: int(values[1]), successes: int(values[2]), failures: int(values[3]),
		tunnelFailed: int(values[4]), contractFailed: int(values[5]), noConsensus: int(values[6]),
		locateFailed: int(values[7]), notConfident: int(values[8]), submitFailed: int(values[9]),
		healthNotRun: int(values[10]), runNotMeasured: int(values[11]), noExitIp: int(values[12]),
		runBatchGuard:  int(values[13]),
		unknownFailure: int(values[14]), inconsistent: int(values[15]), unobserved: int(values[16]),
		newestOutcomeAgeSeconds: values[17], oldestOutcomeAgeSeconds: values[18],
	}
	knownFailures := snapshot.tunnelFailed + snapshot.contractFailed + snapshot.noConsensus +
		snapshot.locateFailed + snapshot.notConfident + snapshot.submitFailed +
		snapshot.healthNotRun + snapshot.runNotMeasured + snapshot.noExitIp + snapshot.runBatchGuard
	if snapshot.failures != knownFailures+snapshot.unknownFailure ||
		snapshot.observed != snapshot.successes+snapshot.failures ||
		snapshot.eligible != snapshot.observed+snapshot.inconsistent+snapshot.unobserved {
		return egressOutcomeSnapshot{}, fmt.Errorf("provider egress outcomes returned contradictory aggregate counts")
	}
	if snapshot.observed == 0 {
		if snapshot.newestOutcomeAgeSeconds != -1 || snapshot.oldestOutcomeAgeSeconds != -1 {
			return egressOutcomeSnapshot{}, fmt.Errorf("provider egress outcomes returned ages without observed outcomes")
		}
	} else if snapshot.newestOutcomeAgeSeconds < 0 ||
		snapshot.oldestOutcomeAgeSeconds < snapshot.newestOutcomeAgeSeconds {
		return egressOutcomeSnapshot{}, fmt.Errorf("provider egress outcomes returned contradictory aggregate ages")
	}
	assessment := model.AssessProbeFleetOutcomes(snapshot.tally())
	if assessment.Eligible != snapshot.eligible || assessment.Observed != snapshot.observed ||
		assessment.Successes != snapshot.successes || assessment.Failures != snapshot.failures ||
		assessment.Inconsistent != snapshot.inconsistent || assessment.Unobserved != snapshot.unobserved {
		return egressOutcomeSnapshot{}, fmt.Errorf("provider egress outcomes disagreed with the shared fleet classifier")
	}
	return snapshot, nil
}

func (egressOutcomesProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	rows, err := env.runner.pg(ctx, egressOutcomesQuery())
	if err != nil {
		return nil, err
	}
	snapshot, err := parseEgressOutcomeSnapshot(rows)
	if err != nil {
		return nil, err
	}
	target := pgTarget(env)
	findings := []finding{
		healthyFinding("pg/egress-outcomes", tierPage, "egress-common-mode", target),
		healthyFinding("pg/egress-outcomes", tierWarn, "egress-mixed-failure", target),
		healthyFinding("pg/egress-outcomes", tierWarn, "egress-outcome-inconsistent", target),
		healthyFinding("pg/egress-outcomes", tierWarn, "egress-outcome-unknown", target),
	}
	assessment := model.AssessProbeFleetOutcomes(snapshot.tally())
	if diagnosis := assessment.Dominant; diagnosis != nil {
		findings[0] = egressCommonModeFinding(target, snapshot, diagnosis)
	} else if assessment.FailureShareHigh {
		findings[1] = egressMixedFailureFinding(target, snapshot)
	}
	if snapshot.inconsistent > 0 {
		findings[2] = egressInconsistentFinding(target, snapshot)
	}
	if snapshot.unknownFailure > 0 {
		findings[3] = egressUnknownFinding(target, snapshot)
	}
	return findings, nil
}

func egressOutcomeObserved(snapshot egressOutcomeSnapshot) string {
	return fmt.Sprintf(
		"eligible=%d observed=%d success=%d failure=%d tunnel_failed=%d contract_failed=%d no_consensus=%d locate_failed=%d not_confident=%d submit_failed=%d health_not_run=%d run_not_measured=%d no_exit_ip=%d run_batch_guard=%d unknown_failure=%d inconsistent=%d unobserved=%d newest_outcome_age_seconds=%d oldest_outcome_age_seconds=%d",
		snapshot.eligible, snapshot.observed, snapshot.successes, snapshot.failures,
		snapshot.tunnelFailed, snapshot.contractFailed, snapshot.noConsensus,
		snapshot.locateFailed, snapshot.notConfident, snapshot.submitFailed,
		snapshot.healthNotRun, snapshot.runNotMeasured, snapshot.noExitIp, snapshot.runBatchGuard,
		snapshot.unknownFailure, snapshot.inconsistent, snapshot.unobserved,
		snapshot.newestOutcomeAgeSeconds, snapshot.oldestOutcomeAgeSeconds,
	)
}

func egressCommonModeFinding(target string, snapshot egressOutcomeSnapshot, diagnosis *model.ProbeFleetDiagnosis) finding {
	return finding{
		probeId: "pg/egress-outcomes", tier: tierPage, class: "egress-common-mode", target: target, sustain: 1,
		symptom: fmt.Sprintf(
			"%d of %d currently eligible providers share the egress-probe failure class %s",
			diagnosis.DominantCount, diagnosis.Eligible, diagnosis.DominantClass,
		),
		mechanism: "At least 20 providers have an observed outcome and one known failure class covers at least 90% of the complete current eligible population. The common boundary is therefore the shared prober/authentication/API path, not independent provider behavior. Unobserved and inconsistent providers remain in the denominator.",
		baseline:  "No known failure class covers 90% of the complete eligible Public-provider population, and at least 20 observed outcomes are required before attribution is allowed.",
		observed:  egressOutcomeObserved(snapshot) + fmt.Sprintf(" dominant_failure=%s dominant_count=%d", diagnosis.DominantClass, diagnosis.DominantCount),
		evidence:  "The query returns one fixed aggregate over active top-level connected valid Public providers. It normalizes failure classes inside PostgreSQL and excludes client IDs, raw failure text, locations, endpoints, credentials, and task IDs.",
		context:   "This is a software/operational common-path failure, not a Proxy hardware-capacity alert. A retained failure older than the six-hour retry window is unobserved rather than reused, and overloaded location update timestamps never prove recovery.",
		action:    egressCommonModeAction(diagnosis.DominantClass),
		verify:    egressOutcomeVerify(),
		playbook:  "SIGNALS.md §2.23, §2.19, and §8.9",
	}
}

func egressCommonModeAction(class string) string {
	switch class {
	case "no_consensus", "tunnel_failed", "contract_failed":
		return "Inspect the persisted prober_identity singleton for complete client credential and mint state, the ProberBootstrap task, and the prober network's transfer balance first. Then verify bounded platform/API reachability and egress confinement from the executing Taskworkers. Do not inspect individual providers first, reveal the stored token, or introduce a legacy environment credential."
	case "locate_failed", "not_confident":
		// retired with the vendor consensus (connect/GEOMAP.md §11.3); only
		// attempts written before that release still carry them
		return "These classes come only from probers that predate the operator's own /ip echo; confirm every Taskworker runs the current prober, whose runs report no_exit_ip instead. Do not investigate individual provider locations."
	case "no_exit_ip":
		return "Every run's warm-up fetches the operator's /ip echo on the public api address through the provider's tunnel (public_api_url); a common failure is that echo -- its reachability from the open internet, its certificate, or the api behind it -- not the providers. Check it from outside the operator network before anything else."
	case "health_not_run", "run_not_measured":
		return "A run that did not start or measured nothing is the prober's, not the provider's: inspect the Taskworker pass errors, the transport and tunnel re-creation budgets, platform reachability, and whether the shard's max time still covers a run at the load rules (§2.19)."
	case model.ProbeRunBatchGuardClass:
		return "The run batch guard held whole full batches back because their scored loads failed together: inspect the prober's request profile (egress-sites.yml), the destination pool, and the prober host's capacity, as the §2.19b prober-fault finding describes."
	case "submit_failed":
		return "Inspect the bounded provider-egress submission response class, operator authentication readiness, API availability, and deployed producer/API schema. Preserve the failed attempts; do not replay raw payloads or print operator credentials."
	default:
		return "Inspect the shared prober credential, balance, egress confinement, platform/API reachability, and deployed producer schema before investigating individual providers. Preserve the bounded evidence and do not expose credentials."
	}
}

func egressOutcomeVerify() string {
	return "After repairing the proved shared boundary, keep §2.19 advancing and wait for each provider's next applicable due cycle: the six-hour failure backoff for an absent or stale location, or up to the 12-hour health due age when the failed pass refreshed health but not a fresh location. Then allow the configured shard max_time, idle_delay, and one monitor cadence for replacement outcomes. Confirm the reconstructed population is healthy on two later cadences. Alert absence caused only by failures aging to unobserved is not recovery; never delete or rewrite attempt rows to clear it."
}

func egressMixedFailureFinding(target string, snapshot egressOutcomeSnapshot) finding {
	return finding{
		probeId: "pg/egress-outcomes", tier: tierWarn, class: "egress-mixed-failure", target: target, sustain: 2,
		symptom:   fmt.Sprintf("%d of %d currently eligible providers have current egress-probe failures that do not establish one dominant known class", snapshot.failures, snapshot.eligible),
		mechanism: "At least 20 outcomes are observed and total failures cover at least 90% of the complete eligible population, but no single known class reaches that share. This proves broad probe degradation without proving one shared cause; collapsed unknown classes cannot manufacture common-mode attribution.",
		baseline:  "Current egress-probe failures cover less than 90% of the complete eligible population, or one independently verified common class explains the event.",
		observed:  egressOutcomeObserved(snapshot),
		evidence:  "Only fixed aggregate counts, bounded class labels, and aggregate ages leave PostgreSQL. Raw classes and provider identity remain private.",
		context:   "This is a broad software/operational degradation without a proved common cause. It is not resolved by adding Proxy hardware.",
		action:    "Correlate §2.19 shard progress with bounded task pass errors, platform/API reachability, exit placements (urnetwork_egress_probe_locations_total) and the prober-side attempt classes in the observed counts (no_exit_ip, run_not_measured, health_not_run, run_batch_guard), the §2.19b site pool findings, submission status, and provider eligibility. Separate the failure classes before changing anything; do not relax provider gates or inspect customer/provider identifiers from this aggregate.",
		verify:    egressOutcomeVerify(),
		playbook:  "SIGNALS.md §2.23, §2.19, and §8.9",
	}
}

func egressInconsistentFinding(target string, snapshot egressOutcomeSnapshot) finding {
	return finding{
		probeId: "pg/egress-outcomes", tier: tierWarn, class: "egress-outcome-inconsistent", target: target, sustain: 2,
		symptom:   fmt.Sprintf("%d currently eligible provider(s) have a current successful attempt but no trusted egress location", snapshot.inconsistent),
		mechanism: "A success attempt is reported only after location submission succeeds, and the location outlives the attempt row. This state therefore proves an ingestion, persistence, deletion, timestamp, or ordering invariant failure; it is not a normal retry-cadence gap.",
		baseline:  "Every current empty failure class has a trusted egress location; providers without a current outcome remain explicitly unobserved.",
		observed:  egressOutcomeObserved(snapshot),
		evidence:  "The invariant is counted inside PostgreSQL and exports no provider, location, endpoint, payload, or credential identity.",
		context:   "This is a software data-integrity alert. It does not establish that the provider itself failed and cannot be fixed with additional Proxy hardware.",
		action:    "Trace the bounded location-submit and attempt-report ordering, monotonic upserts, retention task, and any direct mutation for the same interval. Preserve both tables; do not synthesize a location or delete the attempt to make the aggregate green.",
		verify:    "A normal probe writes a trusted location before its empty failure class, no inconsistent state remains for two cadences, and §2.19 continues advancing without manual table edits.",
		playbook:  "SIGNALS.md §2.23 and §2.19",
	}
}

func egressUnknownFinding(target string, snapshot egressOutcomeSnapshot) finding {
	return finding{
		probeId: "pg/egress-outcomes", tier: tierWarn, class: "egress-outcome-unknown", target: target, sustain: 2,
		symptom:   fmt.Sprintf("%d current provider egress-probe failure(s) use class values outside the monitor's bounded vocabulary", snapshot.unknownFailure),
		mechanism: "The writer and reader failure vocabularies have diverged, or invalid values reached the attempt table. Distinct raw values are deliberately collapsed and excluded from dominant-class attribution, so this alert cannot mistake heterogeneous unknowns for one common failure.",
		baseline:  "Every current failure uses one documented bounded class shared by the deployed producer, API, model, metrics, and monitor.",
		observed:  egressOutcomeObserved(snapshot),
		evidence:  "Unknown raw values are normalized to unknown_failure before aggregation; neither the value nor provider identity leaves PostgreSQL.",
		context:   "This is an observability/schema compatibility failure. The aggregate may coexist with mixed fleet degradation but cannot establish a credential cause.",
		action:    "Compare the exact deployed prober, Taskworker, API, and monitor source contracts. Add a reviewed bounded class and handling when the producer change is intentional; otherwise correct the producer. Never log or select the raw class merely to diagnose this alert.",
		verify:    "All current attempts map to the reviewed vocabulary for two cadences, the producer and reader artifacts agree, and any underlying fleet degradation is independently resolved.",
		playbook:  "SIGNALS.md §2.23 and §8.12",
	}
}
