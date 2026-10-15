// What a URL probe run observed of the provider's end of the session it
// measured, for the FP2 modern provider gate (FP2FIX.md, 2026-10-14): the
// end-to-end encryption the probe client and the provider negotiated, and
// the transfer window the provider's acks advertised. The run reads it from
// the transport that carried its last load, before the tunnel closes, and
// the server records it beside the run's URL result.
package egresshealth

import (
	"net/http"
	"time"
)

// One observation of the provider's end of a session. A transport reports a
// negative only from a session that asked for end-to-end encryption and
// finished negotiating it; a probe client with encryption off, or one whose
// handshake had not concluded, observed nothing and reports the zero value.
type ModernEvidence struct {
	// an end-to-end encrypted session with the provider was established
	E2eNegotiated bool `json:"e2e_negotiated"`
	// the negotiated TLS key exchange group as crypto/tls names it, for
	// example X25519MLKEM768; "" for none
	KeyExchange string `json:"key_exchange"`
	// the negotiated group is a post-quantum hybrid
	PostQuantum bool `json:"post_quantum"`
	// the provider's acks carried receive_window_byte_count
	WindowAdvertised bool `json:"window_advertised"`
	// the largest receive window the provider advertised
	MaxReceiveWindowByteCount int64 `json:"max_receive_window_byte_count"`
	// when the session was observed, zero for no observation
	ObservedAt time.Time `json:"observed_at"`
}

// The transport of a client whose session can say what it negotiated with
// the provider. A provider tunnel's transport implements it from the connect
// library's session evidence. Any other transport, a test server's
// included, implements nothing, and its runs report no evidence.
type ModernEvidenceSource interface {
	// what the session has observed, the zero value for nothing
	ProviderModernEvidence() ModernEvidence
}

// The evidence the client's transport observed, nil for none.
func observeModernEvidence(client *http.Client) *ModernEvidence {
	if client == nil {
		return nil
	}
	source, ok := client.Transport.(ModernEvidenceSource)
	if !ok {
		return nil
	}
	evidence := source.ProviderModernEvidence()
	if evidence.ObservedAt.IsZero() {
		return nil
	}
	return &evidence
}
