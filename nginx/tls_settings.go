package nginx

// TLS policy for the fronted names (owner: s-tls). The defaults pin what the
// lb image's nginx negotiates (warp/warpctl/config.go:1284-1298:
// `ssl_protocols TLSv1.2 TLSv1.3`, `ssl_ciphers EECDH+AESGCM:EDH+AESGCM`,
// `ssl_ecdh_curve X25519:prime256v1:secp384r1`), with the parity differences
// LBALT-IMPL.md §4 accepts: X25519MLKEM768 is offered first, and TLS 1.2
// keeps only the ECDHE AES-GCM suites, since Go has no finite-field DHE.
// Session tickets are Go's per-process default (D6c), so nothing here sets
// them.

import (
	"crypto/tls"
)

// The front's TLS policy. Read-only once the front is built.
type TlsSettings struct {
	// the oldest version a client may negotiate: TLS 1.2, as nginx's
	// `ssl_protocols TLSv1.2 TLSv1.3`. Zero means Go's server default, which
	// is TLS 1.2 today.
	MinVersion uint16
	// the TLS 1.2 suites: ECDHE with AES-GCM, as nginx's `EECDH+AESGCM`. Go
	// orders them itself, and TLS 1.3 suites are not configurable. Empty
	// means Go's defaults, which add ChaCha20 suites nginx doesn't offer.
	CipherSuites []uint16
	// the key exchange groups in preference order: X25519MLKEM768, then
	// nginx's X25519, P-256 and P-384. Empty means Go's defaults.
	CurvePreferences []tls.CurveID
}

// A new instance on every call, with nginx's policy.
func DefaultTlsSettings() *TlsSettings {
	return &TlsSettings{
		MinVersion: tls.VersionTLS12,
		CipherSuites: []uint16{
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
		},
		CurvePreferences: []tls.CurveID{
			tls.X25519MLKEM768,
			tls.X25519,
			tls.CurveP256,
			tls.CurveP384,
		},
	}
}
