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
// provider_egress_probe_attempt is upserted per client_id and swept at 4x
// ProviderEgressProbeAttemptBackoff, so it holds AT MOST ONE ROW PER PROVIDER:
// that provider's latest attempt. It is a snapshot of the fleet's current
// state, not an event stream, which is exactly the shape wanted here -- "what
// is every provider's most recent probe outcome" -- but two consequences follow
// and must not be "fixed" away:
//
//   - A successful attempt stores probe_failure = '' (see
//     RecordProviderEgressProbeAttempt). The empty class therefore counts the
//     HEALTHY providers. A dominant-class computation that does a plain argmax
//     over the tally would diagnose a perfectly healthy fleet as "100% failing
//     with class ''". ProbeAttemptSuccessClass is excluded for that reason.
//   - The diagnosis cannot fire the instant an incident begins. The table starts
//     out mixed and converges as providers are re-attempted, which at a 6h
//     backoff takes a few hours -- comfortably inside the eight-hour incident,
//     but it means this is a trend detector and not a trip wire. Do not convert
//     it into a per-request trigger to make it faster; a single failing
//     submission carries no fleet-wide information at all.

// ProbeAttemptSuccessClass is the probe_failure value stored for an attempt
// that SUCCEEDED. It is the empty string because the column is NOT NULL and a
// success has no failure to name.
const ProbeAttemptSuccessClass = ""

// MinProbeFleetDiagnosisAttempts is the floor on how many providers must have a
// recorded attempt before any fleet-wide diagnosis is made.
//
// Without a floor this is a wolf-crier. A cold deployment whose first three
// probes all failed is at a 100% failure rate with a single dominant class, and
// warning there would teach operators to ignore the message -- at which point
// the real incident goes unread too. Three providers failing is not evidence
// about a fleet; it is evidence about three providers.
const MinProbeFleetDiagnosisAttempts = 20

// The share of ALL attempts a single failure class must reach before the fault
// is attributed to the prober. Compared as exact integers
// (denominator*count >= numerator*attempts) rather than through a float, so the
// boundary cannot drift with rounding -- the same reason
// minEgressHealthOKNumerator/Denominator are written this way.
//
// The share is deliberately taken over every attempt rather than over the
// failures alone. "90% of failures are no_consensus" is unremarkable and true
// in normal operation, because a fleet has a characteristic failure mode. "90%
// of the entire fleet just failed, all identically" is the incident.
const (
	probeFleetFailureShareNumerator   = 9
	probeFleetFailureShareDenominator = 10
)

// ProbeFleetDiagnosis is the finding: one failure class accounts for
// essentially the whole fleet.
type ProbeFleetDiagnosis struct {
	// Attempts is every provider with a recorded attempt, successful or not.
	Attempts int
	// DominantClass is the failure class covering nearly all of them. Never
	// ProbeAttemptSuccessClass.
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
		// the incident verbatim. no_consensus means the geolocation lookups
		// never reached agreement, which happens when nothing is carried
		// through the tunnel at all -- including when no tunnel is ever
		// established because the prober's own credential was refused
		return "CREDENTIAL FIRST: the prober's platform jwt (UR_PROBER_BY_JWT) may be expired or " +
			"rejected, so no tunnel is being established and the geolocation lookups return nothing " +
			"to agree on. This exact shape was an 8h outage. Check the prober's auth before " +
			"investigating any provider"
	case "tunnel_failed", "contract_failed":
		return "CREDENTIAL FIRST: the prober cannot open a tunnel to ANY provider. Check its " +
			"platform jwt (UR_PROBER_BY_JWT), then its network's transfer balance -- a prober " +
			"network with no balance cannot form a contract with anyone"
	default:
		return "the common factor across this many providers is the prober, not the providers. " +
			"Check the prober's credentials, its egress confinement, and its connectivity to the " +
			"platform before investigating individual providers"
	}
}

// DiagnoseProbeFleet decides whether a tally of latest-attempt outcomes says the
// PROBER is broken rather than the fleet. It returns nil when it does not.
//
// Pure: no I/O, no clock, no database. The caller supplies the tally, so the
// rule is table-testable on its own, which matters because this decides whether
// a warning is emitted and a warning that never fires is indistinguishable from
// one that was never written.
//
// tally maps probe_failure to the number of providers whose LATEST attempt
// carried it, including ProbeAttemptSuccessClass for the successes.
func DiagnoseProbeFleet(tally map[string]int) *ProbeFleetDiagnosis {
	attempts := 0
	dominantClass := ""
	dominantCount := 0

	// sorted so a tie between two equally-sized classes resolves the same way
	// every time; an unstable diagnosis would flap between two names in the log
	// and read as two different faults
	classes := make([]string, 0, len(tally))
	for class := range tally {
		classes = append(classes, class)
	}
	sort.Strings(classes)

	for _, class := range classes {
		count := tally[class]
		if count <= 0 {
			// a zero or negative bucket is not an observation of anything, and
			// counting it would drag the denominator around
			continue
		}
		attempts += count
		// successes are counted into attempts (they are the denominator the
		// failure share is measured against) but can never BE the dominant
		// failure class -- see the file comment
		if class == ProbeAttemptSuccessClass {
			continue
		}
		if dominantCount < count {
			dominantClass = class
			dominantCount = count
		}
	}

	if attempts < MinProbeFleetDiagnosisAttempts {
		return nil
	}
	if dominantClass == ProbeAttemptSuccessClass {
		// no failures at all, or only zero-count buckets.
		//
		// Deliberately redundant with the ProbeAttemptSuccessClass skip in the
		// loop above: either one alone is sufficient to keep a healthy fleet
		// from being diagnosed. Both are kept because the failure they prevent
		// -- reporting a fleet where every probe SUCCEEDED as one where every
		// probe failed with class "" -- is the worst possible output of this
		// function, and it is one deleted `continue` away in a naive rewrite.
		return nil
	}
	if probeFleetFailureShareDenominator*dominantCount < probeFleetFailureShareNumerator*attempts {
		return nil
	}

	return &ProbeFleetDiagnosis{
		Attempts:      attempts,
		DominantClass: dominantClass,
		DominantCount: dominantCount,
		Hint:          probeFleetHint(dominantClass),
	}
}

// GetProviderEgressProbeAttemptTally counts providers by the failure class of
// their most recent probe attempt.
//
// One row per provider is already the table's shape (upsert on client_id), so
// this is a GROUP BY over a few hundred rows and needs no time bound of its own
// -- RemoveExpiredProviderEgressProbeAttempts keeps the table to the live
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

// DiagnoseProviderEgressProbeFleet reads the current tally and applies
// DiagnoseProbeFleet to it. Returns nil when the fleet looks normal.
func DiagnoseProviderEgressProbeFleet(ctx context.Context) *ProbeFleetDiagnosis {
	return DiagnoseProbeFleet(GetProviderEgressProbeAttemptTally(ctx))
}
