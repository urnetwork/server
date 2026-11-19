package nginx

// Certificates and TLS configs for the fronted names (owner: s-tls; D6, D9).
//
// Certificates load once, at start, before the block reports ready; a new
// certificate takes effect on the next connect deploy (D6, rotation (a)).
//
// The two base configs are built once and shared by every socket of the
// block, so Go's automatic per-process session ticket keys span all of them
// (D6c): a config returned by GetConfigForClient keeps the base's ticket keys
// unless it sets its own, and quic-go uses the listener's config without
// cloning it.

import (
	"crypto/tls"
	"errors"
	"time"
)

// Every served name's certificate and key. Read-only once loaded, so safe
// for concurrent use.
type CertificateStore struct {
}

// Loads a certificate and key pair for every name the route table serves
// (RouteTable.ServedNames), by warpctl's findLatestTls rule
// (warp/warpctl/config.go:910-988) over <vault>/<env>/tls, then
// <vault>/all/tls, then the flat host layout <vault>/tls: the highest semver
// version directory holding both files for the name, then for
// star.<parent>, then for the parent; a wildcard *.b.c looks for star.b.c.
// It fails if any name has no complete pair, or if a certificate's SANs don't
// cover the name it serves, aliases, the parent fallback and wildcards
// included.
func LoadCertificateStore(vaultDir string, env string, routeTable *RouteTable) (*CertificateStore, error) {
	return nil, errors.New("lbalt: certificate store not implemented")
}

// The earliest NotAfter of the loaded certificates, reported in
// X-UR-Front-Cert-Expiry.
func (self *CertificateStore) EarliestExpiry() time.Time {
	panic("lbalt: not implemented")
}

// The certificate for a served name as the route table writes it (a wildcard
// as *.b.c), or false.
func (self *CertificateStore) certificate(name string) (*tls.Certificate, bool) {
	panic("lbalt: not implemented")
}

// Builds the front's two base configs.
//
// tlsConfig serves TCP 443. Its GetConfigForClient picks the certificate by
// SNI through matcher, with defaultName's, the first lb name and nginx's
// default server, for a missing or unknown SNI, and ALPN by name class:
// http/1.1 only for connect names, whose websocket and H1+ carriers need
// it, and h2 then http/1.1 for the rest, as nginx offers.
//
// quicTlsConfig serves QUIC, where connect dispatches only connect and api
// names. ALPN is the client's offer intersected with [h3], as alt's is, so
// raw-QUIC connect without ALPN still passes.
//
// Both pin settings' versions, suites and groups. Early data stays off (D9):
// Go's TLS never accepts it over TCP, and connect's public QUIC config sets
// Allow0RTT false.
func newTlsConfigs(certificateStore *CertificateStore, matcher *nameMatcher, defaultName string, settings *TlsSettings) (tlsConfig *tls.Config, quicTlsConfig *tls.Config) {
	panic("lbalt: not implemented")
}
