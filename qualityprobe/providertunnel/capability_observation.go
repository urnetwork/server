// A capability measurement tunnel (server/capprobe) observes what its
// provider negotiates on the wire: connect's per-peer evidence of the
// end-to-end session and of receive window advertisement (connect
// DESIGNNOTES §3.13). It is an ordinary provider tunnel whose generated
// clients attempt end-to-end sessions and record into an observer; the URL
// probe's tunnels keep their sessions off.
package providertunnel

import (
	"context"
	"errors"

	"github.com/urnetwork/connect"
)

// A capability observation without its observer would attempt sessions and
// record nothing.
var ErrCapabilityObservation = errors.New("providertunnel: a capability observation needs its observer")

// What a capability measurement tunnel changes in every client it generates.
// The observer belongs to the measurement that set it, never to the process.
type CapabilityObservation struct {
	// every generated client records its peers' evidence here; required
	Observations *connect.PeerCapabilityObservations
	// Resolves a peer's published identity key when no contract carried
	// one, so a session can verify that peer's identity proof. Nil leaves a
	// contract-carried key the only source.
	NewPeerClientPublicKeyFetcher func(peerId connect.Id) func(context.Context) ([]byte, error)
}

// Nil, the URL tunnel's, is valid.
func (self *CapabilityObservation) validate() error {
	if self != nil && self.Observations == nil {
		return ErrCapabilityObservation
	}
	return nil
}

// Changes one generated client's settings in place; nil changes nothing.
//
// Sessions are opportunistic. A provider without the end-to-end responder
// keeps carrying the probe's pings in plaintext, which is what lets the
// observer see its ClientHello go unanswered while the provider acknowledges
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
	settings.PeerCapabilityObservations = self.Observations
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
