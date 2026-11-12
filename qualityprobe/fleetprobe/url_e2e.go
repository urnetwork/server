// The end-to-end session of a URL turn that measures post-quantum
// (FP2-PROTOCOL.md §12.2): the claim marks the turn (prober.Provider.
// PostQuantum), its tunnel's clients open an opportunistic session with the
// provider after registering their key, hold their first sends for the seal
// up to SealWait, and record what the provider negotiated in the turn's
// recorder (providertunnel.CapabilityObservation). The turn reads the
// recorder when its IPv4 HTTPS leg writes its first request and once its
// loads end, and reports e2e evidence (egresshealth.UrlProbeE2eEvidence).
//
// Opportunistic means no URL leg depends on the session: a provider without
// the responder carries the turn in plaintext once the hold ends, and a
// replaced window client keeps the tunnel (KeepTunnelOnReplacement). Such a
// replacement, or a tunnel the turn re-created, leaves the session
// unmeasured: the recorder then holds what more than one client saw.
package fleetprobe

import (
	"context"
	"sync"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server/qualityprobe/egresshealth"
	"github.com/urnetwork/server/qualityprobe/providertunnel"
)

// The peers one turn's recorder holds: its provider, with room to spare.
const urlProbeE2eMaxObservedPeerCount = 8

// A post-quantum turn's timings.
type UrlProbePostQuantumSettings struct {
	// How long the turn's first sends wait for the session to seal before
	// they go in plaintext (pq_seal_wait): 3 s, the bound an HTTPS connect
	// is given. A provider that seals in time carries the turn sealed; one
	// without the responder costs the turn this much once.
	SealWait time.Duration
	// A post-quantum session that negotiated and had not sealed this long
	// after its first ClientHello reads negative, a younger one unmeasured:
	// the capability probe's seal grace.
	UnsealedGrace time.Duration
	// A ClientHello unanswered this long, while the provider acknowledged
	// the turn's data, reads negative (no responder): connect's establishment
	// window, after which a session gives up. Most turns end sooner, and
	// their unanswered ClientHello reads unmeasured.
	NoResponderWindow time.Duration
}

// The post-quantum turn's settings.
func DefaultUrlProbePostQuantumSettings() *UrlProbePostQuantumSettings {
	return &UrlProbePostQuantumSettings{
		SealWait:          3 * time.Second,
		UnsealedGrace:     10 * time.Second,
		NoResponderWindow: connect.DefaultEncryptionSettings().TlsTimeout,
	}
}

// The capability observation a post-quantum turn's tunnels apply, each with
// recorder, the turn's own.
func urlProbeE2eCapabilityObservation(recorder *connect.PeerCapabilityRecorder, settings *UrlProbePostQuantumSettings, newPeerClientPublicKeyFetcher func(peerId connect.Id) func(context.Context) ([]byte, error)) *providertunnel.CapabilityObservation {
	return &providertunnel.CapabilityObservation{
		Recorder:                      recorder,
		NewPeerClientPublicKeyFetcher: newPeerClientPublicKeyFetcher,
		EstablishHold:                 settings.SealWait,
		KeepTunnelOnReplacement:       true,
	}
}

// One post-quantum turn's session as its path observes it. Implements
// egresshealth.UrlProbeE2eObserver; safe for concurrent use.
type urlProbeE2eObservation struct {
	// what the turn's recorder holds of its provider, false for nothing
	read     func() (connect.PeerCapabilityObservation, bool)
	settings *UrlProbePostQuantumSettings
	// whether the turn re-created its tunnel or its window client was
	// replaced, either of which leaves the session unmeasured
	unmeasurable func() bool
	now          func() time.Time

	stateLock       sync.Mutex
	requestWritten  bool
	sealedAtRequest bool
}

// Implements egresshealth.UrlProbeE2eObserver.
func (self *urlProbeE2eObservation) SealWait() time.Duration {
	return self.settings.SealWait
}

// Implements egresshealth.UrlProbeE2eObserver.
func (self *urlProbeE2eObservation) RequestWritten() {
	observation, found := self.read()
	sealed := found && observation.E2eSealed
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if !self.requestWritten {
		self.requestWritten = true
		self.sealedAtRequest = sealed
	}
}

// Implements egresshealth.UrlProbeE2eObserver.
func (self *urlProbeE2eObservation) Evidence(httpsPassed bool) *egresshealth.UrlProbeE2eEvidence {
	observation, found := self.read()
	sealedAtRequest := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		sealedAtRequest = self.sealedAtRequest
	}()
	return readUrlProbeE2e(observation, found, self.now(), sealedAtRequest, httpsPassed, self.unmeasurable(), self.settings)
}

// The e2e evidence of one turn's recorder at now, nil when it recorded
// nothing of the provider (FP2-PROTOCOL.md §12.2):
//
//   - positive: a post-quantum group sealed before the IPv4 HTTPS leg wrote
//     its request, and that leg passed, on the turn's one tunnel and client;
//   - negative: a classical group, sealed or not; a post-quantum session
//     that had not sealed UnsealedGrace after its first ClientHello; or a
//     ClientHello unanswered for NoResponderWindow while the provider
//     acknowledged the turn's data. A re-created tunnel or replaced client
//     keeps only the classical group, which the provider chose whichever
//     client saw it;
//   - unmeasured: anything else.
//
// The window reads resizable when an acknowledgement advertised one, fixed
// when acknowledgements came without, and not_tested otherwise.
func readUrlProbeE2e(observation connect.PeerCapabilityObservation, found bool, now time.Time, sealedAtRequest bool, httpsPassed bool, unmeasurable bool, settings *UrlProbePostQuantumSettings) *egresshealth.UrlProbeE2eEvidence {
	if !found {
		return nil
	}
	e2e := &egresshealth.UrlProbeE2eEvidence{Pq: egresshealth.UrlProbePqUnmeasured, Window: egresshealth.UrlProbeWindowNotTested}
	switch {
	case observation.WindowAdvertised:
		e2e.Window = egresshealth.UrlProbeWindowResizable
	case observation.AckObserved:
		e2e.Window = egresshealth.UrlProbeWindowFixed
	}
	negotiated := observation.E2eNegotiated || observation.E2eSealed
	if negotiated {
		e2e.KeyExchange = observation.KeyExchange
		e2e.SealedBeforeRequest = sealedAtRequest && observation.E2eSealed && observation.KeyExchange != ""
	}
	switch {
	case negotiated && !observation.PostQuantum:
		e2e.Pq = egresshealth.UrlProbePqNegative
	case unmeasurable:
	case negotiated && observation.E2eSealed:
		if e2e.SealedBeforeRequest && httpsPassed {
			e2e.Pq = egresshealth.UrlProbePqPositive
		}
	case negotiated:
		if !now.Before(observation.E2eFirstAttemptTime.Add(settings.UnsealedGrace)) {
			e2e.Pq = egresshealth.UrlProbePqNegative
		}
	case observation.E2eAttempted && observation.AckObserved:
		if !now.Before(observation.E2eFirstAttemptTime.Add(settings.NoResponderWindow)) {
			e2e.Pq = egresshealth.UrlProbePqNegative
		}
	}
	return e2e
}
