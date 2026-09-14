package model

import (
	"context"
	"sort"

	"github.com/urnetwork/server/v2026"
)

// Fleet-wide diagnosis of the probe pipeline.
//
// # The incident this exists for
//
// A prober whose platform jwt had been rejected reported EVERY provider as
// `no_consensus` for eight hours. Every individual record was well-formed, every
// endpoint returned 200, the due queue kept handing out work and the prober kept
// taking it. Per-provider the data was indistinguishable from a genuinely
// unreachable fleet, and nothing anywhere said "credential".
//
// The tell was only ever visible in aggregate: when essentially every provider
// fails, and they all fail the SAME way, the one thing they have in common is
// not the providers -- it is the prober. That is a statement about the
// distribution of failures, so it cannot be made per submission; it has to be
// made over the population. This file makes it.
//
// # What the table actually holds
//
// provider_egress_probe_attempt is upserted per client_id, so it holds at most
// one attempt per provider. It is NOT, by itself, an unbiased current-fleet
// snapshot. Failures are retried after ProviderEgressProbeAttemptBackoff while
// successful locations are not due again for much longer, and attempt rows are
// eventually swept while successful locations remain trusted. Diagnosing the
// retained attempt table alone therefore overweights failures.
//
// A fleet diagnosis must reconstruct one state for every currently eligible
// provider: active, top-level, connected, valid, and holding a Public provide
// key. A current failed attempt is a failure when it is newer than the trusted
// location; a newer location update makes the ordering ambiguous, never a
// success, because verdict reprioritisation also writes that timestamp. An
// older retained failure becomes unobserved rather than resurrecting that
// location as success. Unobserved and inconsistent providers stay in the
// denominator without becoming a failure class. Two consequences follow and
// must not be "fixed" away:
//
//   - A successful outcome is represented by ''. A dominant-class computation
//     that does a plain argmax would diagnose a perfectly healthy fleet as
//     failing. ProbeAttemptSuccessClass is excluded for that reason.
//   - The diagnosis cannot fire the instant an incident begins. The eligible
//     population converges as providers are re-attempted, so this is a trend
//     detector rather than a per-request trip wire. One failed submission does
//     not carry fleet-wide information.

// ProbeAttemptSuccessClass is the probe_failure value stored for an attempt
// that SUCCEEDED. It is the empty string because the column is NOT NULL and a
// success has no failure to name.
const ProbeAttemptSuccessClass = ""

// ProbeFleetUnobservedClass keeps an eligible provider with no current outcome
// in the denominator. It is deliberately neutral: coverage and queue progress
// are diagnosed separately, and treating an unobserved provider as either a
// success or a failure would distort common-mode attribution.
const ProbeFleetUnobservedClass = "unobserved"

// ProbeFleetInconsistentClass means a current attempt claims success but no
// trusted location exists. It is an ingestion/data-integrity invariant, not a
// provider failure, so it remains in the denominator and is alerted separately.
const ProbeFleetInconsistentClass = "inconsistent"

// ProbeFleetUnknownFailureClass is the bounded replacement for any raw failure
// vocabulary the current reader does not know. It counts toward total failure
// share, but can never establish a dominant common class: several distinct raw
// values may have collapsed into this one redacted bucket.
const ProbeFleetUnknownFailureClass = "unknown_failure"

// MinProbeFleetDiagnosisAttempts is the floor on how many providers must have
// a reconstructed observed success or failure before any fleet-wide diagnosis
// is made. Unobserved and inconsistent eligible providers stay in the share
// denominator but cannot satisfy this evidence floor.
//
// Without a floor this is a wolf-crier. A cold deployment whose first three
// probes all failed is at a 100% failure rate with a single dominant class, and
// warning there would teach operators to ignore the message -- at which point
// the real incident goes unread too. Three providers failing is not evidence
// about a fleet; it is evidence about three providers.
const MinProbeFleetDiagnosisAttempts = 20

// The share of the complete eligible population a single failure class must
// reach before the fault is attributed to the prober. Compared as exact
// integers (denominator*count >= numerator*eligible) rather than through a
// float, so the boundary cannot drift with rounding -- the same reason
// minEgressHealthOKNumerator/Denominator are written this way.
//
// The share is deliberately taken over every eligible provider rather than
// over failures alone. "90% of failures are no_consensus" is unremarkable and
// true in normal operation, because a fleet has a characteristic failure mode.
// "90% of the eligible fleet has a current proved failure, all identically" is
// the incident.
const (
	probeFleetFailureShareNumerator   = 9
	probeFleetFailureShareDenominator = 10
)

// ProbeFleetDiagnosis is the finding: one failure class accounts for
// essentially the whole fleet.
type ProbeFleetDiagnosis struct {
	// Eligible is every provider in the current population denominator.
	Eligible int
	// Attempts is every provider with a classified success or failure. An
	// unobserved or inconsistent provider remains in Eligible but not here.
	Attempts int
	// DominantClass is the failure class covering nearly all Eligible providers.
	// Never ProbeAttemptSuccessClass.
	DominantClass string
	// DominantCount is how many providers reported DominantClass.
	DominantCount int
	// Hint names the prober-side cause to check first. It is the part the
	// incident was missing: the operator had the failure class all along and
	// still had no reason to suspect a credential.
	Hint string
}

// probeFleetHint maps a failure class to the prober-side cause worth checking
// before anyone starts investigating providers.
//
// Every hint leads with the prober rather than the fleet, because reaching this
// function already means the fleet-wide test passed -- the providers have
// already been ruled out as the common factor by the distribution itself.
func probeFleetHint(class string) string {
	switch class {
	case "no_consensus":
		return "CREDENTIAL READINESS FIRST: inspect the persisted prober_identity singleton, its " +
			"bootstrap/mint state, and its network balance. Then verify platform and API reachability " +
			"plus egress confinement before investigating individual providers"
	case "tunnel_failed", "contract_failed":
		return "CREDENTIAL READINESS FIRST: inspect the persisted prober_identity singleton and " +
			"bootstrap/mint state, then its network transfer balance. A prober network with no usable " +
			"credential or balance cannot form contracts with any provider"
	case ProbeFleetUnknownFailureClass:
		return "the producer emitted one or more failure classes outside the reader's bounded " +
			"vocabulary. Compare deployed producer and reader schemas before attributing a common cause"
	default:
		return "the common factor across this many providers is the prober, not the providers. " +
			"Check the prober's credentials, its egress confinement, and its connectivity to the " +
			"platform before investigating individual providers"
	}
}

// ProbeFleetAssessment is the complete, population-aware result used by both
// metrics and monitor alerts. Eligible is the denominator; Unobserved and
// Inconsistent remain inside it but are not counted as provider failures.
type ProbeFleetAssessment struct {
	Eligible         int
	Observed         int
	Successes        int
	Failures         int
	Unobserved       int
	Inconsistent     int
	FailureShareHigh bool
	Dominant         *ProbeFleetDiagnosis
}

// AssessProbeFleetOutcomes summarizes one classified state per eligible
// provider. Nonpositive buckets are ignored, matching DiagnoseProbeFleet's
// long-standing behavior.
func AssessProbeFleetOutcomes(tally map[string]int) ProbeFleetAssessment {
	assessment := ProbeFleetAssessment{}
	failureTally := map[string]int{}
	for class, count := range tally {
		if count <= 0 {
			continue
		}
		assessment.Eligible += count
		switch class {
		case ProbeAttemptSuccessClass:
			assessment.Successes += count
		case ProbeFleetUnobservedClass:
			assessment.Unobserved += count
		case ProbeFleetInconsistentClass:
			assessment.Inconsistent += count
		default:
			assessment.Failures += count
			failureTally[class] += count
		}
	}
	assessment.Observed = assessment.Successes + assessment.Failures
	assessment.FailureShareHigh = probeFleetFailureShareHigh(
		assessment.Eligible,
		assessment.Observed,
		assessment.Failures,
	)
	assessment.Dominant = DiagnoseProbeFleetPopulation(
		assessment.Eligible,
		assessment.Observed,
		failureTally,
	)
	return assessment
}

func probeFleetFailureShareHigh(eligible int, observed int, failures int) bool {
	return MinProbeFleetDiagnosisAttempts <= observed &&
		0 <= failures && failures <= observed && observed <= eligible &&
		probeFleetFailureShareDenominator*failures >= probeFleetFailureShareNumerator*eligible
}

// DiagnoseProbeFleetPopulation attributes a common prober-side fault when one
// known failure class covers the configured share of the complete eligible
// population. UnknownFailure is excluded from dominance because multiple raw
// classes are deliberately collapsed into that redacted bucket.
func DiagnoseProbeFleetPopulation(eligible int, observed int, failureTally map[string]int) *ProbeFleetDiagnosis {
	if observed < MinProbeFleetDiagnosisAttempts || eligible < observed {
		return nil
	}

	classes := make([]string, 0, len(failureTally))
	for class := range failureTally {
		classes = append(classes, class)
	}
	sort.Strings(classes)

	dominantClass := ""
	dominantCount := 0
	totalFailures := 0
	for _, class := range classes {
		count := failureTally[class]
		if count <= 0 {
			continue
		}
		totalFailures += count
		if class == ProbeFleetUnknownFailureClass || class == ProbeAttemptSuccessClass ||
			class == ProbeFleetUnobservedClass || class == ProbeFleetInconsistentClass {
			continue
		}
		if dominantCount < count {
			dominantClass = class
			dominantCount = count
		}
	}
	if observed < totalFailures || dominantClass == "" ||
		probeFleetFailureShareDenominator*dominantCount < probeFleetFailureShareNumerator*eligible {
		return nil
	}

	return &ProbeFleetDiagnosis{
		Eligible:      eligible,
		Attempts:      observed,
		DominantClass: dominantClass,
		DominantCount: dominantCount,
		Hint:          probeFleetHint(dominantClass),
	}
}

// DiagnoseProbeFleet is the compatibility entry point for a complete outcome
// tally. It decides whether the PROBER is broken rather than the fleet and
// returns nil when the distribution does not establish that attribution.
//
// Pure: no I/O, no clock, no database. The caller supplies the tally, so the
// rule is table-testable on its own, which matters because this decides whether
// a warning is emitted and a warning that never fires is indistinguishable from
// one that was never written.
//
// Production callers must supply the reconstructed eligible-population tally
// from GetProviderEgressProbeFleetOutcomeTally, not the retained attempt table.
// Tests and other pure callers may supply a complete attempt cohort, including
// ProbeAttemptSuccessClass for successes.
func DiagnoseProbeFleet(tally map[string]int) *ProbeFleetDiagnosis {
	return AssessProbeFleetOutcomes(tally).Dominant
}

// GetProviderEgressProbeAttemptTally counts retained attempt rows by their raw
// failure class. It is useful for attempt-table instrumentation only. It must
// not be passed to fleet attribution: failures retry much sooner than
// successes and therefore survive in this table disproportionately.
//
// One row per provider is already the table's shape (upsert on client_id), so
// this is a GROUP BY over a few hundred retained rows and needs no time bound
// of its own for instrumentation. Retention does not make it a current eligible
// population.
func GetProviderEgressProbeAttemptTally(ctx context.Context) map[string]int {
	tally := map[string]int{}

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				probe_failure,
				COUNT(*)
			FROM provider_egress_probe_attempt
			GROUP BY probe_failure
			`,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var probeFailure string
				var count int
				server.Raise(result.Scan(&probeFailure, &count))
				tally[probeFailure] = count
			}
		})
	})

	return tally
}

// GetProviderEgressProbeFleetOutcomeTally reconstructs one bounded state for
// every currently eligible provider. observed_at is used only for the
// seven-day trust bound. location update_time can prove that a current failure
// came after a location, but the reverse is only ambiguous because verdict
// reprioritisation also changes it. A failed attempt is current for the
// six-hour retry window. Older failures do not resurrect an older success:
// they become unobserved until §2.19 proves the due queue advances and records
// a new outcome.
func GetProviderEgressProbeFleetOutcomeTally(ctx context.Context) map[string]int {
	tally := map[string]int{}
	now := server.NowUtc()
	minAttemptAt := now.Add(-ProviderEgressProbeAttemptBackoff)
	minObservedAt := now.Add(-ProviderEgressLocationMaxAge)

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			WITH eligible AS MATERIALIZED (
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
					  AND pk.provide_mode = $1
				  )
			), classified AS (
				SELECT CASE
					WHEN pea.attempt_at >= $2
					 AND pea.probe_failure <> ''
					 AND (
						pel.client_id IS NULL OR
						pel.observed_at < $3 OR
						pea.update_time > pel.update_time
					 )
					THEN CASE pea.probe_failure
						WHEN 'tunnel_failed' THEN 'tunnel_failed'
						WHEN 'contract_failed' THEN 'contract_failed'
						WHEN 'no_consensus' THEN 'no_consensus'
						WHEN 'locate_failed' THEN 'locate_failed'
						WHEN 'not_confident' THEN 'not_confident'
						WHEN 'submit_failed' THEN 'submit_failed'
						ELSE 'unknown_failure'
					END
					WHEN pel.observed_at >= $3
					 AND (pea.client_id IS NULL OR pea.probe_failure = '')
					THEN ''
					WHEN pea.attempt_at >= $2 AND pea.probe_failure = ''
					THEN 'inconsistent'
					ELSE 'unobserved'
				END AS outcome_class
				FROM eligible e
				LEFT JOIN provider_egress_probe_attempt pea USING (client_id)
				LEFT JOIN provider_egress_location pel USING (client_id)
			)
			SELECT outcome_class, count(*)
			FROM classified
			GROUP BY outcome_class
			`,
			ProvideModePublic,
			minAttemptAt.UTC(),
			minObservedAt.UTC(),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var class string
				var count int
				server.Raise(result.Scan(&class, &count))
				tally[class] = count
			}
		})
	})

	return tally
}

// DiagnoseProviderEgressProbeFleet reconstructs the complete current eligible
// population before applying the pure classifier. Returns nil when no known
// failure class establishes common-mode attribution.
func DiagnoseProviderEgressProbeFleet(ctx context.Context) *ProbeFleetDiagnosis {
	return AssessProbeFleetOutcomes(GetProviderEgressProbeFleetOutcomeTally(ctx)).Dominant
}
