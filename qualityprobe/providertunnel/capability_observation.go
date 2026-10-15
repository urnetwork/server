// A capability measurement tunnel (server/capprobe) observes what its
// provider negotiates on the wire: connect's per-peer evidence of the
// end-to-end session and of receive window advertisement (connect
// DESIGNNOTES §3.13). It is an ordinary provider tunnel whose generated
// clients attempt end-to-end sessions and record what they see in the
// measurement's recorder; the URL probe's tunnels keep their sessions off.
package providertunnel

import (
	"context"
	"errors"
	"time"

	"github.com/urnetwork/connect"
)

// A capability observation without its recorder would attempt sessions and
// record nothing.
var ErrCapabilityObservation = errors.New("providertunnel: a capability observation needs its recorder")

// What a capability measurement tunnel changes in every client it generates.
// The recorder belongs to the measurement that set it, never to the process.
type CapabilityObservation struct {
	// every generated client records its peers' evidence here; required
	Recorder *connect.PeerCapabilityRecorder
	// Resolves a peer's published identity key when no contract carried
	// one, so a session can verify that peer's identity proof. Nil leaves a
	// contract-carried key the only source.
	NewPeerClientPublicKeyFetcher func(peerId connect.Id) func(context.Context) ([]byte, error)
	// The least time from a failed attempt to admit the window's client to
	// the next attempt, before connect's jitter, which defers it by half to
	// all of this. About the measurement's own timeout, so a measurement
	// attempts its provider about once: each attempt mints, registers and
	// retires a derived client, and unpaced, an unreachable provider is
	// dialed again at every resize pass for the whole measurement, in each of
	// a pass's measurements at once. A client the window admitted and then
	// lost is dialed again at once whatever this is, and that measurement has
	// lost its tunnel anyway. Zero keeps a probe tunnel's unpaced re-dial.
	RedialMin time.Duration
}

// Nil, the URL tunnel's, is valid.
func (self *CapabilityObservation) validate() error {
	if self != nil && self.Recorder == nil {
		return ErrCapabilityObservation
	}
	return nil
}

// Paces the window's re-dial of a provider whose client it failed to admit
// (RedialMin); nil, or no RedialMin, changes nothing.
func (self *CapabilityObservation) paceRedial(settings *connect.MultiClientSettings) {
	if self == nil || self.RedialMin <= 0 {
		return
	}
	settings.FixedDestinationRetryMin = self.RedialMin
	settings.FixedDestinationRetryMax = max(settings.FixedDestinationRetryMax, self.RedialMin)
}

// Changes one generated client's settings in place; nil changes nothing.
//
// Sessions are opportunistic. A provider without the end-to-end responder
// keeps carrying the probe's pings in plaintext, which is what lets the
// recorder see its ClientHello go unanswered while the provider acknowledges
// the pings. Once this client has verified the provider, its own traffic is
// sealed, and the provider can open it only with the identity key the
// platform holds for this client, from a contract or from its /key/<client_id>
// fetch. So the client is admitted to the window only after the platform has
// processed its key registration (ClientKeyRegistrationRequired): the
// provider can resolve the key from the first frame, rather than dropping
// sealed frames until its own establishment window expires. The TLS timeouts
// stay the defaults, which is what capprobe reads a missing answer against.
func (self *CapabilityObservation) apply(settings *connect.ClientSettings) {
	if self == nil {
		return
	}
	settings.PeerCapabilityRecorder = self.Recorder
	// a copy, so a settings value a caller shares keeps its own encryption
	encryption := connect.DefaultEncryptionSettings()
	if settings.EncryptionSettings != nil {
		copied := *settings.EncryptionSettings
		encryption = &copied
	}
	encryption.Mode = connect.EncryptionModeOpportunistic
	if self.NewPeerClientPublicKeyFetcher != nil {
		encryption.NewPeerClientPublicKeyFetcher = self.NewPeerClientPublicKeyFetcher
	}
	settings.EncryptionSettings = encryption
	settings.ClientKeyRegistrationRequired = true
}
