package providertunnel

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"testing"
	"time"
)

func selfSigned(t *testing.T, host string) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{host},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, key
}

// selfSignedCA generates a minimal self-signed CA certificate, for building
// a multi-certificate chain in tests below. It follows the same pattern as
// generateTestCa in tunnel_test.go, but lives here (rather than being
// shared) because these tests never install it as a trusted root -- checkPin
// parses and pins raw certificates directly, it does not perform chain
// verification, so there is no need for the CA to be trusted by the process.
func selfSignedCA(t *testing.T, cn string) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, key
}

// issueChain builds a three-certificate chain -- root CA, an intermediate
// CA signed by the root, and a leaf for host signed by the intermediate --
// and returns the leaf and intermediate (the two certificates a real TLS
// handshake would present via rawCerts; the root is not sent on the wire
// and is not returned). This models the shape of the chains
// cmd/egress-prober/main.go actually pins against: a leaf issued by a named
// intermediate (e.g. "Let's Encrypt YE2"), not a bare self-signed leaf.
func issueChain(t *testing.T, host string) (leaf, intermediate *x509.Certificate) {
	t.Helper()
	rootCert, rootKey := selfSignedCA(t, "test root CA")

	intKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	intTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(2),
		Subject:               pkix.Name{CommonName: "test intermediate CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	intDER, err := x509.CreateCertificate(rand.Reader, intTmpl, rootCert, &intKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	intCert, err := x509.ParseCertificate(intDER)
	if err != nil {
		t.Fatal(err)
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(3),
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, intCert, &leafKey.PublicKey, intKey)
	if err != nil {
		t.Fatal(err)
	}
	leafCert, err := x509.ParseCertificate(leafDER)
	if err != nil {
		t.Fatal(err)
	}

	return leafCert, intCert
}

// chainOf builds the verifiedChains argument crypto/tls hands
// VerifyPeerCertificate after a successful chain verification. These tests
// call the verifier directly rather than standing up a handshake, so they
// must supply it themselves: checkPin matches against the VERIFIED chain and
// has no fallback to the peer-controlled rawCerts, which is what makes the
// dead-weight-intermediate bypass unreachable.
func chainOf(certs ...*x509.Certificate) [][]*x509.Certificate {
	return [][]*x509.Certificate{certs}
}

// TestPinnedTLSConfigAcceptsIntermediateMatchOnLeafRotation is the rotation
// scenario this task exists to fix: the LEAF's pin is absent from the
// allowed set (as happens the moment a host rotates its leaf certificate --
// routine, roughly every 90 days), but the INTERMEDIATE that issued it is
// still pinned. checkPin must walk the whole presented chain, not just
// rawCerts[0], and accept on the intermediate match. Against the old
// leaf-only implementation this fails, because rawCerts[0] (the leaf) never
// matches the pinned intermediate SPKI.
func TestPinnedTLSConfigAcceptsIntermediateMatchOnLeafRotation(t *testing.T) {
	const host = "pinned.example"
	leaf, intermediate := issueChain(t, host)

	cfg := PinnedTlsConfig(map[string][]string{
		host: {SpkiPin(intermediate)}, // only the intermediate is pinned
	})
	cfg.ServerName = host

	// rawCerts as a real handshake presents them: leaf first, then the
	// intermediate(s) the server sent.
	err := cfg.VerifyPeerCertificate([][]byte{leaf.Raw, intermediate.Raw}, chainOf(leaf, intermediate))
	if err != nil {
		t.Fatalf("a pinned intermediate must accept a chain whose leaf rotated, got %v", err)
	}
}

// TestPinnedTLSConfigRejectsWhenNeitherLeafNorIntermediateMatch is the
// negative counterpart: when NO certificate anywhere in the presented chain
// -- leaf or intermediate -- has a pin in the allowed set, the check must
// still fail closed with ErrPinMismatch, exactly as it did before chain-wide
// matching was added. This guards against a chain-wide implementation that
// accidentally accepts anything once it starts iterating past rawCerts[0].
func TestPinnedTLSConfigRejectsWhenNeitherLeafNorIntermediateMatch(t *testing.T) {
	const host = "pinned.example"
	leaf, intermediate := issueChain(t, host)

	cfg := PinnedTlsConfig(map[string][]string{
		host: {"not-a-real-pin-for-this-chain"},
	})
	cfg.ServerName = host

	err := cfg.VerifyPeerCertificate([][]byte{leaf.Raw, intermediate.Raw}, chainOf(leaf, intermediate))
	if err != ErrPinMismatch {
		t.Fatalf("err = %v, want ErrPinMismatch when neither leaf nor intermediate matches", err)
	}
}

// TestCheckPinRejectsAnEmptyVerifiedChain: with no verified chain there is
// nothing trustworthy to match a pin against, and the peer-controlled
// rawCerts are not a substitute -- matching them is exactly the
// dead-weight-intermediate bypass (an attacker pads the wire chain with a
// legitimately pinned certificate that was never on the validated path).
// checkPin used to fall back to rawCerts here, inert in production only
// because nothing in this package sets InsecureSkipVerify on the exported,
// mutable configs it hands out; a single debugging line elsewhere would have
// re-armed the full bypass with no test noticing. The fallback is gone, so
// this must fail closed.
func TestCheckPinRejectsAnEmptyVerifiedChain(t *testing.T) {
	cert, _ := selfSigned(t, "pinned.example")
	cfg := PinnedTlsConfigForHost(map[string][]string{
		"pinned.example": {SpkiPin(cert)},
	}, "pinned.example")

	// The pin matches the presented certificate -- the ONLY thing missing is
	// a verified chain, which is precisely the situation that must not pass.
	if err := cfg.VerifyPeerCertificate([][]byte{cert.Raw}, nil); err != ErrPinMismatch {
		t.Fatalf("err = %v, want ErrPinMismatch: a pin must never be matched against unverified peer certificates", err)
	}
}

func TestSPKIPinStableAndDistinct(t *testing.T) {
	a, _ := selfSigned(t, "a.example")
	b, _ := selfSigned(t, "b.example")
	if SpkiPin(a) == "" {
		t.Fatal("pin must not be empty")
	}
	if SpkiPin(a) != SpkiPin(a) {
		t.Fatal("pin must be stable for the same cert")
	}
	if SpkiPin(a) == SpkiPin(b) {
		t.Fatal("different keys must produce different pins")
	}
}

func TestPinnedTLSConfigAcceptsMatchingPin(t *testing.T) {
	cert, _ := selfSigned(t, "pinned.example")
	cfg := PinnedTlsConfig(map[string][]string{
		"pinned.example": {SpkiPin(cert)},
	})
	cfg.ServerName = "pinned.example"
	err := cfg.VerifyPeerCertificate([][]byte{cert.Raw}, chainOf(cert))
	if err != nil {
		t.Fatalf("matching pin must verify, got %v", err)
	}
}

func TestPinnedTLSConfigRejectsWrongPin(t *testing.T) {
	good, _ := selfSigned(t, "pinned.example")
	evil, _ := selfSigned(t, "pinned.example")
	cfg := PinnedTlsConfig(map[string][]string{
		"pinned.example": {SpkiPin(good)},
	})
	cfg.ServerName = "pinned.example"
	err := cfg.VerifyPeerCertificate([][]byte{evil.Raw}, chainOf(evil))
	if err != ErrPinMismatch {
		t.Fatalf("err = %v, want ErrPinMismatch (a provider must not be able to MITM)", err)
	}
}

func TestPinnedTLSConfigIgnoresUnpinnedHost(t *testing.T) {
	cert, _ := selfSigned(t, "other.example")
	cfg := PinnedTlsConfig(map[string][]string{
		"pinned.example": {"someotherpin"},
	})
	cfg.ServerName = "other.example"
	if err := cfg.VerifyPeerCertificate([][]byte{cert.Raw}, chainOf(cert)); err != nil {
		t.Fatalf("unpinned host must pass the pin check, got %v", err)
	}
}

// TestPinnedTLSConfigCloneWithDifferentServerNameFailsClosed reproduces the
// critical fail-open defect: PinnedTlsConfig returns a template, a later
// caller does the idiomatic-looking `clone := template.Clone(); clone.ServerName
// = host`, and presents a certificate with the WRONG key for a pinned host.
// Under the old implementation the closure still read the ORIGINAL template's
// (empty) ServerName, found no matching pin-map entry, and returned nil --
// silently accepting an attacker certificate. It must be rejected.
func TestPinnedTLSConfigCloneWithDifferentServerNameFailsClosed(t *testing.T) {
	good, _ := selfSigned(t, "pinned.example")
	evil, _ := selfSigned(t, "pinned.example")
	template := PinnedTlsConfig(map[string][]string{
		"pinned.example": {SpkiPin(good)},
	})

	perHost := template.Clone()
	perHost.ServerName = "pinned.example"

	err := perHost.VerifyPeerCertificate([][]byte{evil.Raw}, chainOf(evil))
	if err == nil {
		t.Fatal("FAIL-OPEN: clone accepted an attacker cert with the wrong key for a pinned host")
	}
}

func TestPinnedTLSConfigForHostAcceptsMatchingPin(t *testing.T) {
	cert, _ := selfSigned(t, "pinned.example")
	cfg := PinnedTlsConfigForHost(map[string][]string{
		"pinned.example": {SpkiPin(cert)},
	}, "pinned.example")

	if cfg.ServerName != "pinned.example" {
		t.Fatalf("ServerName = %q, want %q", cfg.ServerName, "pinned.example")
	}
	if cfg.MinVersion < tls.VersionTLS12 {
		t.Fatalf("MinVersion = %v, want at least TLS 1.2", cfg.MinVersion)
	}
	if err := cfg.VerifyPeerCertificate([][]byte{cert.Raw}, chainOf(cert)); err != nil {
		t.Fatalf("matching pin must verify, got %v", err)
	}
}

func TestPinnedTLSConfigForHostRejectsWrongKey(t *testing.T) {
	good, _ := selfSigned(t, "pinned.example")
	evil, _ := selfSigned(t, "pinned.example")
	cfg := PinnedTlsConfigForHost(map[string][]string{
		"pinned.example": {SpkiPin(good)},
	}, "pinned.example")

	err := cfg.VerifyPeerCertificate([][]byte{evil.Raw}, chainOf(evil))
	if err != ErrPinMismatch {
		t.Fatalf("err = %v, want ErrPinMismatch (a provider must not be able to MITM)", err)
	}
}

// TestPinnedTLSConfigForHostSurvivesCloneMutation demonstrates that, unlike
// PinnedTlsConfig, a config built by PinnedTlsConfigForHost is safe to
// Clone() and does not depend on the clone's ServerName field at all: the
// verifier closes over its own immutable copy of the host.
func TestPinnedTLSConfigForHostSurvivesCloneMutation(t *testing.T) {
	good, _ := selfSigned(t, "pinned.example")
	cfg := PinnedTlsConfigForHost(map[string][]string{
		"pinned.example": {SpkiPin(good)},
	}, "pinned.example")

	clone := cfg.Clone()
	clone.ServerName = "unrelated.example"

	if err := clone.VerifyPeerCertificate([][]byte{good.Raw}, chainOf(good)); err != nil {
		t.Fatalf("PinVerifier must not depend on the config's mutable ServerName, got %v", err)
	}
}

// TestSPKIPinStableAcrossReissuance proves SPKI pinning survives certificate
// renewal: the same key, re-issued with a different serial number, CN, and
// validity window, must produce an identical pin. This is the whole point of
// pinning the key instead of the certificate.
func TestSPKIPinStableAcrossReissuance(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	tmpl1 := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "original.example"},
		NotBefore:    time.Now().Add(-2 * time.Hour),
		NotAfter:     time.Now().Add(-time.Hour),
		DNSNames:     []string{"pinned.example"},
	}
	der1, err := x509.CreateCertificate(rand.Reader, tmpl1, tmpl1, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert1, err := x509.ParseCertificate(der1)
	if err != nil {
		t.Fatal(err)
	}

	tmpl2 := &x509.Certificate{
		SerialNumber: big.NewInt(987654),
		Subject:      pkix.Name{CommonName: "renewed.example"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(365 * 24 * time.Hour),
		DNSNames:     []string{"pinned.example"},
	}
	der2, err := x509.CreateCertificate(rand.Reader, tmpl2, tmpl2, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert2, err := x509.ParseCertificate(der2)
	if err != nil {
		t.Fatal(err)
	}

	if SpkiPin(cert1) != SpkiPin(cert2) {
		t.Fatal("same key across re-issuance must produce an identical SPKI pin")
	}
}

// TestNormalizePinsMergesCollidingKeys: two keys that normalize to the same
// host ("ipinfo.io" and "IPINFO.IO:443") used to overwrite each other, so
// which pin set survived depended on map iteration order -- nondeterministic,
// and a dropped pin means the probe fails closed against the legitimate host
// after a rotation the surviving entry does not cover.
func TestNormalizePinsMergesCollidingKeys(t *testing.T) {
	leaf, _ := selfSigned(t, "pinned.example")
	rotated, _ := selfSigned(t, "pinned.example")

	normalized := normalizePins(map[string][]string{
		"pinned.example":     {SpkiPin(leaf)},
		"PINNED.EXAMPLE:443": {SpkiPin(rotated)},
	})

	allowed := normalized["pinned.example"]
	if len(allowed) != 2 {
		t.Fatalf("normalized pins = %v, want both colliding keys' pins merged", allowed)
	}
	// Both certificates must now verify under the single normalized key.
	for name, cert := range map[string]*x509.Certificate{"leaf": leaf, "rotated": rotated} {
		cfg := PinnedTlsConfigForHost(map[string][]string{
			"pinned.example":     {SpkiPin(leaf)},
			"PINNED.EXAMPLE:443": {SpkiPin(rotated)},
		}, "pinned.example")
		if err := cfg.VerifyPeerCertificate([][]byte{cert.Raw}, chainOf(cert)); err != nil {
			t.Errorf("%s certificate rejected after key collision: %v", name, err)
		}
	}
}
