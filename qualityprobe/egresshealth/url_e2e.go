// The end-to-end session of a URL turn's tunnel (FP2-PROTOCOL.md §12.2,
// §12.3): what the provider's responder negotiated with the probe client, and
// whether its acknowledgements advertise a receive window. The quality probe
// replaces the capability probe as the source of both: strict post-quantum
// selection requires a positive outcome from a URL turn in the eight-hour
// window, and FP2-MODERN.md's resizable windows come from the same session.
//
// Only a turn the server's claim marks for it opens the session (the claim's
// post-quantum turn): each such turn registers its derived client's key, a
// durable signed row, so the server measures each provider a few times per
// window rather than on every turn. Every other turn's evidence omits e2e.
//
// The session is opportunistic: a provider without the end-to-end responder
// keeps carrying the turn in plaintext, so no URL leg, verdict or count
// depends on it.
package egresshealth

import (
	"crypto/tls"
	"fmt"
	"time"
	"unicode/utf8"
)

// The end-to-end session of a turn that measures post-quantum, as the path
// that opened it observes it (fleetprobe url_e2e.go). Methods are safe for
// concurrent use.
type UrlProbeE2eObserver interface {
	// How long the tunnel holds the turn's first sends for the session to
	// seal (pq_seal_wait): counted in the run's budget.
	SealWait() time.Duration
	// The turn's IPv4 HTTPS leg wrote its first request: whether the session
	// had sealed by then is the evidence's sealed_before_request.
	RequestWritten()
	// The turn's evidence once its loads ended, given whether its IPv4 HTTPS
	// leg passed; nil when the session observed nothing.
	Evidence(httpsPassed bool) *UrlProbeE2eEvidence
}

// The outcome of a turn's post-quantum session, the json pq value.
const (
	// the session negotiated a post-quantum group and had sealed when the
	// turn's IPv4 HTTPS leg wrote its request, and that leg passed
	UrlProbePqPositive = "positive"
	// the session negotiated a classical group; or a post-quantum session had
	// not sealed by the run's end; or a ClientHello went unanswered for a
	// whole establishment window while the provider acknowledged the turn's
	// traffic
	UrlProbePqNegative = "negative"
	// anything else: a handshake still pending at the run's end, a lost or
	// replaced tunnel client, a session that sealed only after the request,
	// or a run whose IPv4 HTTPS leg failed or was not measured
	UrlProbePqUnmeasured = "unmeasured"
)

// What the provider's acknowledgements showed of its receive window, the
// json window value.
const (
	// an acknowledgement advertised a receive window
	UrlProbeWindowResizable = "resizable"
	// acknowledgements arrived without one: a legacy provider
	UrlProbeWindowFixed = "fixed"
	// no acknowledgement of the turn's data arrived
	UrlProbeWindowNotTested = "not_tested"
)

// The values of the history's pq_outcome column.
const (
	UrlProbePqOutcomeUnmeasured int16 = 0
	UrlProbePqOutcomePositive   int16 = 1
	UrlProbePqOutcomeNegative   int16 = 2
)

// The longest key exchange name the evidence carries, in bytes. The names
// crypto/tls gives a group are a few ASCII characters; provider_modern_evidence
// holds the same bound.
const urlProbeE2eKeyExchangeMaxLength = 64

// The ML-KEM groups by IANA NamedGroup codepoint, hybrid or pure, as connect
// classifies a negotiated group (connect transfer_peer_capability_observation.go).
// A name is post-quantum when it is the name crypto/tls gives one of these,
// so the classification follows the toolchain that named it.
var urlProbePostQuantumKeyExchanges = []tls.CurveID{
	tls.X25519MLKEM768,
	tls.CurveID(0x11eb),
	tls.CurveID(0x11ed),
	tls.CurveID(0x0200),
	tls.CurveID(0x0201),
	tls.CurveID(0x0202),
}

// Whether a negotiated group's name, as connect reports it, carries an
// ML-KEM component.
func UrlProbeKeyExchangePostQuantum(keyExchange string) bool {
	if keyExchange == "" {
		return false
	}
	for _, curveId := range urlProbePostQuantumKeyExchanges {
		if curveId.String() == keyExchange {
			return true
		}
	}
	return false
}

// The end-to-end evidence of one URL turn, omitted from UrlProbeEvidence when
// the turn opened no session.
type UrlProbeE2eEvidence struct {
	// UrlProbePq*
	Pq string `json:"pq"`
	// the group the provider's responder chose, as crypto/tls names it, e.g.
	// X25519MLKEM768; empty when no handshake completed
	KeyExchange string `json:"key_exchange,omitempty"`
	// the session had sealed when the IPv4 HTTPS leg wrote its request, so
	// that leg's request and response rode the sealed session
	SealedBeforeRequest bool `json:"sealed_before_request"`
	// UrlProbeWindow*
	Window string `json:"window"`
}

// Every invariant of the record alone; ValidateOutcome adds those that need
// the outcome's counts.
func (self *UrlProbeE2eEvidence) validate() error {
	switch self.Pq {
	case UrlProbePqPositive, UrlProbePqNegative, UrlProbePqUnmeasured:
	default:
		return fmt.Errorf("URL end-to-end evidence has no post-quantum outcome %q", self.Pq)
	}
	switch self.Window {
	case UrlProbeWindowResizable, UrlProbeWindowFixed, UrlProbeWindowNotTested:
	default:
		return fmt.Errorf("URL end-to-end evidence has no window reading %q", self.Window)
	}
	if urlProbeE2eKeyExchangeMaxLength < len(self.KeyExchange) || !utf8.ValidString(self.KeyExchange) {
		return fmt.Errorf("URL end-to-end key exchange name is not %d bytes or fewer of utf-8", urlProbeE2eKeyExchangeMaxLength)
	}
	if self.SealedBeforeRequest && self.KeyExchange == "" {
		return fmt.Errorf("URL end-to-end evidence sealed a session that negotiated no group")
	}
	postQuantum := UrlProbeKeyExchangePostQuantum(self.KeyExchange)
	switch self.Pq {
	case UrlProbePqPositive:
		// a prober cannot claim positive without a post-quantum key exchange
		// that carried the IPv4 HTTPS leg's request sealed
		if !postQuantum || !self.SealedBeforeRequest {
			return fmt.Errorf("URL end-to-end evidence is positive without a sealed post-quantum session")
		}
	case UrlProbePqNegative:
		// a post-quantum session that sealed is never negative
		if postQuantum && self.SealedBeforeRequest {
			return fmt.Errorf("URL end-to-end evidence is negative for a post-quantum session that sealed")
		}
	}
	return nil
}

// The pq_outcome the store keeps for an outcome of total measured loads:
// positive or negative for a measured outcome whose end-to-end evidence
// concluded, and unmeasured for every other. ValidateOutcome has already
// refused a positive one without a passed IPv4 HTTPS leg.
func (self *UrlProbeEvidence) PqOutcome(total int) int16 {
	if self == nil || self.E2e == nil || total != 1 {
		return UrlProbePqOutcomeUnmeasured
	}
	switch self.E2e.Pq {
	case UrlProbePqPositive:
		return UrlProbePqOutcomePositive
	case UrlProbePqNegative:
		return UrlProbePqOutcomeNegative
	default:
		return UrlProbePqOutcomeUnmeasured
	}
}
