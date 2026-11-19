package nginx

// Certificates and TLS configs for the fronted names (owner: s-tls; D6, D9).
//
// Certificates load once, at start, before the block reports ready; a new
// certificate takes effect on the next connect deploy (D6, rotation (a)). The
// lookup is warpctl's findLatestTls, over every vault layout a block can see:
// the build machine's <vault>/<env>/tls and <vault>/all/tls, and the flat
// <vault>/tls a host has, which xops merges from both. Each key must match
// its certificate, as nginx requires too: OpenSSL's SSL_CTX_use_PrivateKey
// checks the key against the certificate loaded before it ("key values
// mismatch"), and nginx then refuses its configuration. The store checks one
// thing nginx never did: each certificate's SANs cover the name it serves.
//
// A leaf outside its validity window is loaded and served all the same, as
// nginx and warpctl load it: neither reads a NotAfter, so a lapsed pair
// fails only its own names' clients. Refusing it would stop every connect
// block of an lb_alt host at its next start over one name. The load logs
// each such name instead, the front's certificate expiry metric and
// X-UR-Front-Cert-Expiry show the earliest NotAfter, a lapsed one included,
// and NotValidNames lists them.
//
// The two base configs are built once and shared by every socket of the
// block, so Go's automatic per-process session ticket keys span all of them
// (D6c). crypto/tls takes the ticket keys from the config a listener was
// built with unless the config its GetConfigForClient returns sets keys of its
// own (Config.ticketKeys, crypto/tls common.go), and no config here sets any.
// quic-go under go1.27 uses the listener's config without cloning it
// (third_party/quic-go/internal/handshake/tls_config_go127.go). So a ticket
// resumes on any socket of the process, and on no other process.

import (
	"cmp"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/coreos/go-semver/semver"
	"github.com/urnetwork/glog"
)

// ALPN protocol ids (RFC 7301's registry).
const (
	tlsNextProtoHttp1 = "http/1.1"
	tlsNextProtoH2    = "h2"
	tlsNextProtoH3    = "h3"
)

// The handshake error when the front has no certificate for a ClientHello:
// on QUIC a missing, invalid or unknown server name, and on TCP any name the
// default server can't stand in for. It names no server name, since the
// client chose it.
var errTlsNoCertificate = errors.New("lbalt: no certificate for the server name")

// Every served name's certificate and key. Read-only once loaded, so safe
// for concurrent use.
type CertificateStore struct {
	// the pair serving each served name, keyed as the route table writes it
	// (a wildcard as *.b.c). Names whose files are the same share one pair.
	nameCertificates map[string]*tls.Certificate
	// the pairs of the served wildcards, each *.b.c keyed by its base b.c
	wildcardBaseCertificates map[string]*tls.Certificate
	// the longest base in wildcardBaseCertificates, in bytes: no longer part
	// of a name can match, so certificate looks none up
	maxWildcardBaseLength int
	// the earliest NotAfter of the leaves
	earliestExpiry time.Time
}

// Loads a certificate and key pair for every name the route table serves
// (RouteTable.ServedNames), by warpctl's findLatestTls rule (findLatestTls
// below) over <vault>/<env>/tls, then <vault>/all/tls, then the flat host
// layout <vault>/tls. Each name then needs:
//   - a key that matches its certificate;
//   - a leaf whose SANs cover the name. An exact name, found in its own
//     directory, a star.<parent> one or its parent's, must verify as the
//     leaf's host name. A wildcard *.x must be a SAN itself, since it serves
//     every label under x.
//
// Any miss fails, naming the server name, so a block never reports ready
// without every certificate. A leaf not valid now is no miss: it is loaded,
// as nginx loads it, and logged at error level with its name and path. env
// names the vault's environment directory.
func LoadCertificateStore(vaultDir string, env string, routeTable *RouteTable) (*CertificateStore, error) {
	if env == "" || env == "." || env == ".." || strings.ContainsAny(env, `/\`) {
		return nil, fmt.Errorf("certificates: env %q is not a vault directory name", env)
	}
	tlsHomes := []string{
		filepath.Join(vaultDir, env),
		filepath.Join(vaultDir, "all"),
		// a host's vault, which xops merges from all and the env
		vaultDir,
	}
	now := time.Now()

	// whether leaf covers name, the server name it serves
	checkCovers := func(leaf *x509.Certificate, name string) error {
		if strings.HasPrefix(name, "*.") {
			// only the wildcard SAN covers every label under the base: an
			// exact SAN under it covers only itself
			hasWildcard := slices.ContainsFunc(leaf.DNSNames, func(dnsName string) bool {
				return strings.EqualFold(dnsName, name)
			})
			if !hasWildcard {
				return fmt.Errorf("lists no subject alternative name %s (it lists %s)", name, strings.Join(leaf.DNSNames, ", "))
			}
			return nil
		}
		if err := leaf.VerifyHostname(name); err != nil {
			return fmt.Errorf("doesn't cover the name (it lists %s)", strings.Join(leaf.DNSNames, ", "))
		}
		return nil
	}

	// each pair loads once, however many names share it
	pemPathCertificates := map[string]*tls.Certificate{}
	loadPair := func(tlsFiles *tlsVaultFiles) (*tls.Certificate, error) {
		if certificate, ok := pemPathCertificates[tlsFiles.pemPath]; ok {
			return certificate, nil
		}
		pemBytes, err := os.ReadFile(tlsFiles.pemPath)
		if err != nil {
			return nil, err
		}
		keyBytes, err := os.ReadFile(tlsFiles.keyPath)
		if err != nil {
			return nil, err
		}
		// refuses a key that doesn't match the leaf's public key
		certificate, err := tls.X509KeyPair(pemBytes, keyBytes)
		if err != nil {
			return nil, fmt.Errorf("%s and %s: %w", tlsFiles.pemPath, tlsFiles.keyPath, err)
		}
		leaf, err := x509.ParseCertificate(certificate.Certificate[0])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", tlsFiles.pemPath, err)
		}
		certificate.Leaf = leaf
		pemPathCertificates[tlsFiles.pemPath] = &certificate
		return &certificate, nil
	}

	certificateStore := &CertificateStore{
		nameCertificates:         map[string]*tls.Certificate{},
		wildcardBaseCertificates: map[string]*tls.Certificate{},
	}
	for _, name := range routeTable.ServedNames() {
		tlsFiles, err := findLatestTls(tlsHomes, name)
		if err != nil {
			return nil, fmt.Errorf("certificate for %s: %w", name, err)
		}
		certificate, err := loadPair(tlsFiles)
		if err != nil {
			return nil, fmt.Errorf("certificate for %s: %w", name, err)
		}
		if err := checkCovers(certificate.Leaf, name); err != nil {
			return nil, fmt.Errorf("certificate for %s: %s %w", name, tlsFiles.pemPath, err)
		}
		if !certificateValidAt(certificate.Leaf, now) {
			glog.Errorf(
				"[front]certificate for %s: %s is valid only from %s to %s, and serves as nginx would serve it\n",
				name,
				tlsFiles.pemPath,
				certificate.Leaf.NotBefore.UTC().Format(time.RFC3339),
				certificate.Leaf.NotAfter.UTC().Format(time.RFC3339),
			)
		}

		certificateStore.nameCertificates[name] = certificate
		if base, ok := strings.CutPrefix(name, "*."); ok {
			certificateStore.wildcardBaseCertificates[base] = certificate
			certificateStore.maxWildcardBaseLength = max(certificateStore.maxWildcardBaseLength, len(base))
		}
		if certificateStore.earliestExpiry.IsZero() || certificate.Leaf.NotAfter.Before(certificateStore.earliestExpiry) {
			certificateStore.earliestExpiry = certificate.Leaf.NotAfter
		}
	}
	return certificateStore, nil
}

// The earliest NotAfter of the loaded certificates, reported in
// X-UR-Front-Cert-Expiry: in the past when a served name's leaf has lapsed.
func (self *CertificateStore) EarliestExpiry() time.Time {
	return self.earliestExpiry
}

// The served names, as the route table writes them, whose leaf isn't valid
// at now, sorted: expired, or not yet valid. Their clients fail to verify
// the certificate, as they would through nginx.
func (self *CertificateStore) NotValidNames(now time.Time) []string {
	notValidNames := []string{}
	for name, certificate := range self.nameCertificates {
		if !certificateValidAt(certificate.Leaf, now) {
			notValidNames = append(notValidNames, name)
		}
	}
	slices.Sort(notValidNames)
	return notValidNames
}

// Whether a leaf is valid at now, within its NotBefore and NotAfter, both
// included, as crypto/x509 verifies.
func certificateValidAt(leaf *x509.Certificate, now time.Time) bool {
	return !now.Before(leaf.NotBefore) && !now.After(leaf.NotAfter)
}

// The certificate for a served name as the route table writes it (a wildcard
// as *.b.c), or for a normalized name that a served wildcard covers, by
// nginx's rule: one or more leading labels, the longest wildcard first. False
// for any other name. A long name costs no more than a short one, as in
// nameMatcher.match.
func (self *CertificateStore) certificate(name string) (*tls.Certificate, bool) {
	if certificate, ok := self.nameCertificates[name]; ok {
		return certificate, true
	}
	for base := range wildcardBases(name, self.maxWildcardBaseLength) {
		if certificate, ok := self.wildcardBaseCertificates[base]; ok {
			return certificate, true
		}
	}
	return nil, false
}

// The files of one certificate and key pair in the vault.
type tlsVaultFiles struct {
	// <home>/tls/<version>/<dir>/<dir>.pem: the leaf, then its chain
	pemPath string
	// <home>/tls/<version>/<dir>/<dir>.key
	keyPath string
}

// The pair warpctl's findLatestTls (warp/warpctl/config.go:910-988) picks for
// a served name, with the fallbacks of NewNginxConfig (:841-895), over
// tlsHomes in order.
//
// The directories a name's pair may live in come first, in order:
//   - a wildcard *.x: star.x only;
//   - any other name: its own; then, for a name of more than two labels,
//     star.<parent> and <parent>, whose certificates must list the name
//     (LoadCertificateStore checks, where warpctl didn't).
//
// For each directory in turn, each home in turn is searched, and the first
// home that has the directory decides. In <home>/tls, a candidate is a
// subdirectory, not a symlink, whose name parses as semver (coreos
// go-semver, as warpctl) and which holds both <dir>/<dir>.pem and
// <dir>/<dir>.key. The latest candidate wins: by Major, Minor and Patch, then
// Metadata in string order, with PreRelease ignored (warpctl's
// semverCmpWithBuild, util.go:298-313). Each name takes its own latest, so
// two names can come from different versions.
//
// On a tie, which warpctl breaks by map order, the greatest directory name
// wins: the one warpctl's map keeps when two names parse to the same version.
// warpctl's flat-file fallback, a <home>/<dir>/<dir>.pem with no version
// directory, is not ported: it returned a path missing its directory.
func findLatestTls(tlsHomes []string, name string) (*tlsVaultFiles, error) {
	dirNames := []string{}
	if base, ok := strings.CutPrefix(name, "*."); ok {
		dirNames = append(dirNames, "star."+base)
	} else {
		dirNames = append(dirNames, name)
		if labels := strings.Split(name, "."); 2 < len(labels) {
			parent := strings.Join(labels[1:], ".")
			dirNames = append(dirNames, "star."+parent, parent)
		}
	}

	// the latest version directory of tlsHome/tls holding dirName's pair
	findIn := func(tlsHome string, dirName string) (*tlsVaultFiles, bool) {
		versionsPath := filepath.Join(tlsHome, "tls")
		entries, err := os.ReadDir(versionsPath)
		if err != nil {
			return nil, false
		}
		hasFile := func(path string) bool {
			_, err := os.Stat(path)
			return err == nil
		}
		var latestVersion *semver.Version
		var latestTlsFiles *tlsVaultFiles
		// ReadDir sorts by name, so a later candidate that ties replaces the
		// earlier
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			version, err := semver.NewVersion(entry.Name())
			if err != nil {
				continue
			}
			tlsFiles := &tlsVaultFiles{
				pemPath: filepath.Join(versionsPath, entry.Name(), dirName, dirName+".pem"),
				keyPath: filepath.Join(versionsPath, entry.Name(), dirName, dirName+".key"),
			}
			if !hasFile(tlsFiles.pemPath) || !hasFile(tlsFiles.keyPath) {
				continue
			}
			if latestVersion != nil {
				order := cmp.Or(
					cmp.Compare(version.Major, latestVersion.Major),
					cmp.Compare(version.Minor, latestVersion.Minor),
					cmp.Compare(version.Patch, latestVersion.Patch),
					strings.Compare(version.Metadata, latestVersion.Metadata),
				)
				if order < 0 {
					continue
				}
			}
			latestVersion = version
			latestTlsFiles = tlsFiles
		}
		return latestTlsFiles, latestTlsFiles != nil
	}

	for _, dirName := range dirNames {
		for _, tlsHome := range tlsHomes {
			if tlsFiles, ok := findIn(tlsHome, dirName); ok {
				return tlsFiles, nil
			}
		}
	}
	versionsPaths := []string{}
	for _, tlsHome := range tlsHomes {
		versionsPaths = append(versionsPaths, filepath.Join(tlsHome, "tls"))
	}
	return nil, fmt.Errorf(
		"no version directory holds both <dir>/<dir>.pem and <dir>/<dir>.key for %s under %s",
		strings.Join(dirNames, ", "),
		strings.Join(versionsPaths, ", "),
	)
}

// The protocols to advertise to one QUIC client: its offer intersected with
// h3, the one protocol the front speaks on QUIC, or nil when they share none
// (alt's negotiableNextProtos, alt/alt.go:376-387). RFC 9001 makes a QUIC
// server that advertises any protocol refuse a client that offers none, and
// raw-QUIC connect offers none, so advertising h3 to every client would shut
// connect out.
func quicNegotiableNextProtos(offeredNextProtos []string) []string {
	nextProtos := []string{}
	for _, nextProto := range []string{tlsNextProtoH3} {
		if slices.Contains(offeredNextProtos, nextProto) {
			nextProtos = append(nextProtos, nextProto)
		}
	}
	if len(nextProtos) == 0 {
		return nil
	}
	return nextProtos
}

// Builds the front's two base configs, each the long-lived config every
// socket of its kind serves through (D6c).
//
// Every config a handshake gets comes from GetConfigForClient, and all of
// them are built here, once: one per served name and protocol answer, with
// the name's certificate (certificateStore.certificate) and settings'
// versions, suites and groups. None sets ticket keys, so crypto/tls uses the
// base's. Server names are normalized first (normalizeHost) and matched by
// matcher. certificateStore and matcher come from the same table; a served
// name the store lacks is treated as unknown.
//
// tlsConfig serves TCP 443. A served name gets its certificate and ALPN by
// class (D9): http/1.1 only for connect names, whose websocket and H1+
// carriers need it, and h2 then http/1.1 for the rest, as nginx prefers. A
// missing, invalid or unknown SNI gets the default server, nginx's first lb
// server: defaultName's certificate, with h2 and http/1.1. An SNI ending in a
// dot is none of these: crypto/tls refuses its ClientHello with decode_error
// before GetConfigForClient runs, where nginx drops the dot and serves the
// name (RFC 6066 forbids the dot). A client offering protocols but none of
// these is refused with an alert, as nginx refuses it, though nginx also
// selects http/1.0 and http/0.9, which the front refuses; one offering none
// gets HTTP/1.1.
//
// quicTlsConfig serves QUIC, and ports alt's tlsConfigForClient
// (alt/alt.go:356-374): a served name gets its certificate, and the ALPN is
// quicNegotiableNextProtos of the client's offer, so raw-QUIC connect without
// ALPN still passes. Every served name completes its handshake, and connect's
// dispatch refuses any but connect and api names with 0x1000. A missing,
// invalid or unknown SNI gets no certificate, as in alt, so its handshake
// fails. QUIC itself requires TLS 1.3.
//
// Early data stays off (D9): Go's TLS never accepts it over TCP, and on QUIC
// tickets carry it only when connect's public quic.Config allows 0-RTT,
// which it doesn't.
func newTlsConfigs(certificateStore *CertificateStore, matcher *nameMatcher, defaultName string, settings *TlsSettings) (tlsConfig *tls.Config, quicTlsConfig *tls.Config) {
	newNameTlsConfig := func(certificate *tls.Certificate, nextProtos []string) *tls.Config {
		return &tls.Config{
			Certificates:     []tls.Certificate{*certificate},
			NextProtos:       nextProtos,
			MinVersion:       settings.MinVersion,
			CipherSuites:     slices.Clone(settings.CipherSuites),
			CurvePreferences: slices.Clone(settings.CurvePreferences),
		}
	}
	// connect names serve their websocket and H1+ carriers over h1 only
	connectNextProtos := []string{tlsNextProtoHttp1}
	// nginx's order for every other name, and for the default server
	otherNextProtos := []string{tlsNextProtoH2, tlsNextProtoHttp1}

	tcpNameTlsConfigs := map[string]*tls.Config{}
	// for a QUIC client that offers h3, and for one that doesn't
	quicH3NameTlsConfigs := map[string]*tls.Config{}
	quicRawNameTlsConfigs := map[string]*tls.Config{}
	for _, match := range matcher.servedMatches() {
		certificate, ok := certificateStore.certificate(match.name)
		if !ok {
			continue
		}
		tcpNextProtos := otherNextProtos
		if match.class == NameClassConnect {
			tcpNextProtos = connectNextProtos
		}
		tcpNameTlsConfigs[match.name] = newNameTlsConfig(certificate, slices.Clone(tcpNextProtos))
		quicH3NameTlsConfigs[match.name] = newNameTlsConfig(certificate, []string{tlsNextProtoH3})
		quicRawNameTlsConfigs[match.name] = newNameTlsConfig(certificate, nil)
	}
	var defaultTlsConfig *tls.Config
	if certificate, ok := certificateStore.certificate(defaultName); ok {
		defaultTlsConfig = newNameTlsConfig(certificate, slices.Clone(otherNextProtos))
	}

	tlsConfig = &tls.Config{
		NextProtos:       slices.Clone(otherNextProtos),
		MinVersion:       settings.MinVersion,
		CipherSuites:     slices.Clone(settings.CipherSuites),
		CurvePreferences: slices.Clone(settings.CurvePreferences),
		GetConfigForClient: func(clientHello *tls.ClientHelloInfo) (*tls.Config, error) {
			if serverName, ok := normalizeHost(clientHello.ServerName); ok {
				if match, ok := matcher.match(serverName); ok {
					if nameTlsConfig, ok := tcpNameTlsConfigs[match.name]; ok {
						return nameTlsConfig, nil
					}
				}
			}
			if defaultTlsConfig == nil {
				return nil, errTlsNoCertificate
			}
			return defaultTlsConfig, nil
		},
	}
	quicTlsConfig = &tls.Config{
		MinVersion:       settings.MinVersion,
		CipherSuites:     slices.Clone(settings.CipherSuites),
		CurvePreferences: slices.Clone(settings.CurvePreferences),
		GetConfigForClient: func(clientHello *tls.ClientHelloInfo) (*tls.Config, error) {
			serverName, ok := normalizeHost(clientHello.ServerName)
			if !ok {
				return nil, errTlsNoCertificate
			}
			match, ok := matcher.match(serverName)
			if !ok {
				return nil, errTlsNoCertificate
			}
			nameTlsConfigs := quicRawNameTlsConfigs
			if quicNegotiableNextProtos(clientHello.SupportedProtos) != nil {
				nameTlsConfigs = quicH3NameTlsConfigs
			}
			nameTlsConfig, ok := nameTlsConfigs[match.name]
			if !ok {
				return nil, errTlsNoCertificate
			}
			return nameTlsConfig, nil
		},
	}
	return tlsConfig, quicTlsConfig
}
