// Package probeverdict turns a geolocation probe submission into a verdict:
// verified, unverified, or suspect. It is pure decision logic with no I/O, so
// it is fully table-testable independent of how a submission arrived.
//
// Deliberately absent from Input: the mmdb-derived country for the same
// connection, and any RTT or coordinate fields. A probed country differing
// from what the free mmdb would have said is the entire point of this
// project and must never be treated as suspicious -- keeping that field off
// Input makes the omission structural. An RTT-distance corroboration was
// designed and dropped before implementation (see the spec's "The RTT floor
// was designed, then dropped"): it needs a fixed reference point that this
// system does not have a single answer for once more than one deployment
// instance exists, and a wrong reference point does not fail safe -- it can
// flag an honest provider as suspect. Do not reintroduce ObservedRTT or
// coordinate fields here without first resolving that reference-point
// problem in the spec.
package probeverdict

import "time"

type Input struct {
	CountryConfident bool
	CountryCode      string

	PreviousCountryCode string
	PreviousObservedAt  time.Time
	Now                 time.Time
}

type Verdict struct {
	State  string
	Reason string
}

// unstableWindow is how recently a prior, different country counts as a
// flip-flop rather than a legitimate correction.
const unstableWindow = 24 * time.Hour

func Evaluate(in Input) Verdict {
	if !in.CountryConfident {
		return Verdict{State: "unverified", Reason: "no_consensus"}
	}

	if in.PreviousCountryCode != "" && in.PreviousCountryCode != in.CountryCode {
		now := in.Now
		if now.IsZero() {
			now = time.Now()
		}
		if now.Sub(in.PreviousObservedAt) < unstableWindow {
			return Verdict{State: "suspect", Reason: "unstable"}
		}
	}

	return Verdict{State: "verified"}
}
