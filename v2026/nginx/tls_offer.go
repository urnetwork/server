package nginx

// What a TCP ClientHello offered, read from the ClientHello the TCP base
// config's GetConfigForClient sees, for the handshake counters (LBALTOPT2 R22
// (b)). It tells apart handshakes that look alike in version, resumption and
// ALPN, such as the full handshakes that offer no ALPN and never resume:
//   - the class of the server name the client presented, or none;
//   - what the client offered to resume a session with;
//   - the shape of its supported_versions extension, GREASE included, which
//     browsers send and Go's and OpenSSL's clients don't.
//
// Every field is a label value from a closed set. The name, the extensions'
// contents and the client's address never reach one.

import (
	"context"
	"crypto/tls"
	"slices"
)

// A TCP ClientHello's offer, each field a label value. The zero value, for a
// handshake whose ClientHello the front never read, reads as
// tlsClientOfferUnread in each field (Labels).
type TlsClientOffer struct {
	// the class of the server name: none for no server name; the class of a
	// served name (connect, api, lb, forwarded, redirect); unknown for any
	// other name, one nginx's rule refuses included, which gets the default
	// server
	NameClass string
	// what the client offered to resume with: psk, a TLS 1.3 pre_shared_key,
	// so a ticket it holds; psk_modes, TLS 1.3 PSK modes without a PSK, so a
	// client that keeps tickets and holds none for the name; ticket, only TLS
	// 1.2's session_ticket extension; none, neither
	Resumption string
	// the supported_versions extension: tls13, TLS 1.3 and not 1.2; tls13_tls12,
	// both; tls12, TLS 1.2 and not 1.3; each with _grease when the list holds
	// a GREASE value; legacy without the extension, a client of TLS 1.2 or
	// older; other for any other list
	Versions string
}

// The label values of an offer the front never read.
const tlsClientOfferUnread = "-"

// The offer's label values: NameClass, Resumption and Versions, each the
// unread value when empty.
func (self TlsClientOffer) Labels() (nameClass string, resumption string, versions string) {
	orUnread := func(value string) string {
		if value == "" {
			return tlsClientOfferUnread
		}
		return value
	}
	return orUnread(self.NameClass), orUnread(self.Resumption), orUnread(self.Versions)
}

// The NameClass of a ClientHello that presents no server name.
const tlsClientOfferNameNone = "none"

// The Resumption values.
const (
	tlsClientOfferResumptionPsk      = "psk"
	tlsClientOfferResumptionPskModes = "psk_modes"
	tlsClientOfferResumptionTicket   = "ticket"
	tlsClientOfferResumptionNone     = "none"
)

// The Versions values.
const (
	tlsClientOfferVersionsTls13      = "tls13"
	tlsClientOfferVersionsTls13Tls12 = "tls13_tls12"
	tlsClientOfferVersionsTls12      = "tls12"
	tlsClientOfferGreaseSuffix       = "_grease"
	tlsClientOfferVersionsLegacy     = "legacy"
	tlsClientOfferVersionsOther      = "other"
)

// TLS extension ids (IANA's TLS ExtensionType Values).
const (
	tlsExtensionSessionTicket       uint16 = 35
	tlsExtensionPreSharedKey        uint16 = 41
	tlsExtensionSupportedVersions   uint16 = 43
	tlsExtensionPskKeyExchangeModes uint16 = 45
)

// Whether value is a GREASE value (RFC 8701): 0x?A?A with both bytes equal.
func tlsGrease(value uint16) bool {
	return value&0x0f0f == 0x0a0a && value>>8 == value&0xff
}

// The offer of clientHello, whose server name's class is nameClass.
func readTlsClientOffer(clientHello *tls.ClientHelloInfo, nameClass string) TlsClientOffer {
	offer := TlsClientOffer{
		NameClass:  nameClass,
		Resumption: tlsClientOfferResumptionNone,
		Versions:   tlsClientOfferVersionsOther,
	}
	switch {
	case slices.Contains(clientHello.Extensions, tlsExtensionPreSharedKey):
		offer.Resumption = tlsClientOfferResumptionPsk
	case slices.Contains(clientHello.Extensions, tlsExtensionPskKeyExchangeModes):
		offer.Resumption = tlsClientOfferResumptionPskModes
	case slices.Contains(clientHello.Extensions, tlsExtensionSessionTicket):
		offer.Resumption = tlsClientOfferResumptionTicket
	}

	if !slices.Contains(clientHello.Extensions, tlsExtensionSupportedVersions) {
		// crypto/tls lists the versions up to the legacy one in its place
		offer.Versions = tlsClientOfferVersionsLegacy
		return offer
	}
	tls13 := slices.Contains(clientHello.SupportedVersions, tls.VersionTLS13)
	tls12 := slices.Contains(clientHello.SupportedVersions, tls.VersionTLS12)
	grease := slices.ContainsFunc(clientHello.SupportedVersions, tlsGrease)
	switch {
	case tls13 && tls12:
		offer.Versions = tlsClientOfferVersionsTls13Tls12
	case tls13:
		offer.Versions = tlsClientOfferVersionsTls13
	case tls12:
		offer.Versions = tlsClientOfferVersionsTls12
	default:
		return offer
	}
	if grease {
		offer.Versions += tlsClientOfferGreaseSuffix
	}
	return offer
}

// The key of the context value through which a TCP handshake's
// GetConfigForClient hands its ClientHello's offer to the connection
// (frontTlsConn.HandshakeContext): a *TlsClientOffer.
type tlsClientOfferKey struct{}

// Records clientHello's offer in the slot its handshake's context carries,
// when it carries one: only a front TLS listener's connection that observes
// its handshake asks for it.
func recordTlsClientOffer(clientHello *tls.ClientHelloInfo, nameClass string) {
	ctx := clientHello.Context()
	if ctx == nil {
		return
	}
	clientOffer, ok := ctx.Value(tlsClientOfferKey{}).(*TlsClientOffer)
	if !ok {
		return
	}
	*clientOffer = readTlsClientOffer(clientHello, nameClass)
}

// ctx with a slot for its handshake's offer.
func withTlsClientOffer(ctx context.Context, clientOffer *TlsClientOffer) context.Context {
	return context.WithValue(ctx, tlsClientOfferKey{}, clientOffer)
}
