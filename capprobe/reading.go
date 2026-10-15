// What one measurement's observation says about each capability, and the
// evidence a conclusive reading records.
package capprobe

import (
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server/model"
)

// What a measurement's observation says about the provider's end-to-end
// session. Only the four negotiated readings and no_responder are evidence;
// not_tested and pending say nothing about the provider.
type e2eReading string

const (
	// the send path accepted no ClientHello of the probe's
	e2eNotTested e2eReading = "not_tested"
	// the send path accepted a ClientHello, which is still within its
	// establishment window
	e2ePending e2eReading = "pending"
	// no handshake completed for a whole establishment window after a
	// ClientHello, while the provider acknowledged the probe's pings: the
	// provider has no end-to-end responder, or its answer failed, and either
	// way gives a client no session
	e2eNoResponder e2eReading = "no_responder"
	// a session negotiated a classical group, and did not or did seal
	e2eClassicalUnsealed e2eReading = "classical_unsealed"
	e2eClassicalSealed   e2eReading = "classical_sealed"
	// a session negotiated a post-quantum group, and did not or did seal:
	// sealed is authenticated by the provider's identity proof
	e2ePostQuantumUnsealed e2eReading = "post_quantum_unsealed"
	e2ePostQuantumSealed   e2eReading = "post_quantum_sealed"
)

// Every end-to-end reading, in the order the metrics list them.
var e2eReadings = []e2eReading{
	e2eNotTested,
	e2ePending,
	e2eNoResponder,
	e2eClassicalUnsealed,
	e2eClassicalSealed,
	e2ePostQuantumUnsealed,
	e2ePostQuantumSealed,
}

// What a measurement's observation says about the provider's receive
// window. Only fixed and resizable are evidence.
type windowReading string

const (
	// no acknowledgement of the probe's data arrived
	windowNotTested windowReading = "not_tested"
	// acknowledgements arrived without a window: a legacy provider
	windowFixed windowReading = "fixed"
	// an acknowledgement advertised a receive window
	windowResizable windowReading = "resizable"
)

// Every window reading, in the order the metrics list them.
var windowReadings = []windowReading{
	windowNotTested,
	windowFixed,
	windowResizable,
}

// Both capabilities as one observation reads at one instant.
type reading struct {
	e2e    e2eReading
	window windowReading
}

// Reads connect's per-peer evidence (DESIGNNOTES §3.13) at now. found false
// is no record at all. ackSeenAt is when the measurement first saw an
// acknowledgement, zero before one.
//
// Every flag of the observation is sticky evidence of something the provider
// did; a false flag is evidence of absence only under its precondition. The
// end-to-end fields come only from the probe's own ClientHello sessions, so a
// negotiated handshake is the provider's responder answering the probe, and
// reads by the group it chose from the probe's offer and by whether the
// probe verified its identity proof. An unanswered ClientHello is
// no_responder only once the establishment window has passed both since the
// send path first accepted a ClientHello and since the provider was first
// seen acknowledging the probe: until the provider demonstrably receives the
// probe's traffic, silence says nothing, and a ClientHello that a client the
// window discarded while it formed sent cannot shorten the window of the
// client whose pings were acknowledged. An acknowledgement that advertises a
// window is recorded with it in one step, so one without a window is a
// provider that does not advertise, whenever it is read.
func readObservation(observation connect.PeerCapabilityObservation, found bool, now time.Time, ackSeenAt time.Time, tlsTimeout time.Duration) reading {
	if !found {
		return reading{e2e: e2eNotTested, window: windowNotTested}
	}
	read := reading{e2e: e2eNotTested, window: windowNotTested}
	switch {
	case observation.E2eNegotiated || observation.E2eSealed:
		switch {
		case observation.PostQuantum && observation.E2eSealed:
			read.e2e = e2ePostQuantumSealed
		case observation.PostQuantum:
			read.e2e = e2ePostQuantumUnsealed
		case observation.E2eSealed:
			read.e2e = e2eClassicalSealed
		default:
			read.e2e = e2eClassicalUnsealed
		}
	case observation.E2eAttempted:
		read.e2e = e2ePending
		if !ackSeenAt.IsZero() {
			waitedFrom := observation.E2eFirstAttemptTime
			if waitedFrom.Before(ackSeenAt) {
				waitedFrom = ackSeenAt
			}
			if !now.Before(waitedFrom.Add(tlsTimeout)) {
				read.e2e = e2eNoResponder
			}
		}
	}
	switch {
	case observation.WindowAdvertised:
		read.window = windowResizable
	case observation.AckObserved:
		read.window = windowFixed
	}
	return read
}

// The reading's session negotiated.
func (self reading) negotiated() bool {
	switch self.e2e {
	case e2eClassicalUnsealed, e2eClassicalSealed, e2ePostQuantumUnsealed, e2ePostQuantumSealed:
		return true
	default:
		return false
	}
}

// The reading's session negotiated and has not sealed yet.
func (self reading) unsealed() bool {
	return self.e2e == e2eClassicalUnsealed || self.e2e == e2ePostQuantumUnsealed
}

// Both capabilities read as evidence about the provider.
func (self reading) conclusive() bool {
	return (self.negotiated() || self.e2e == e2eNoResponder) && self.window != windowNotTested
}

// The evidence the model records for a conclusive reading of observation,
// observed at observedAt, and false for any other reading: a capability that
// was not tested is never recorded, as a negative or otherwise.
//
// The model reads post-quantum as an end-to-end session negotiated with a
// post-quantum group, sealed or not, so an unsealed session records as
// negotiated; the metrics keep the two apart.
func (self reading) evidence(observation connect.PeerCapabilityObservation, observedAt time.Time) (model.ProviderModernEvidence, bool) {
	if !self.conclusive() {
		return model.ProviderModernEvidence{}, false
	}
	evidence := model.ProviderModernEvidence{
		E2eNegotiated:    self.negotiated(),
		PostQuantum:      self.e2e == e2ePostQuantumSealed || self.e2e == e2ePostQuantumUnsealed,
		WindowAdvertised: self.window == windowResizable,
		ObservedAt:       observedAt.UTC(),
	}
	if evidence.E2eNegotiated {
		evidence.KeyExchange = observation.KeyExchange
	}
	if evidence.WindowAdvertised {
		evidence.MaxReceiveWindowByteCount = model.ByteCount(observation.MaxReceiveWindowByteCount)
	}
	return evidence, true
}

// Whether the measurement can stop at now: the reading is conclusive and, for
// a negotiated session that has not sealed, the seal has had sealGrace since
// the measurement first saw the session negotiate (negotiatedSeenAt).
func (self reading) complete(now time.Time, negotiatedSeenAt time.Time, sealGrace time.Duration) bool {
	if !self.conclusive() {
		return false
	}
	if self.unsealed() {
		return !negotiatedSeenAt.IsZero() && !now.Before(negotiatedSeenAt.Add(sealGrace))
	}
	return true
}
