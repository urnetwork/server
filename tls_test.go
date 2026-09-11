package server

import (
	"bytes"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// Holds one hermetic certificate generation and its decoded leaf identity.
type testTlsPair struct {
	certPemBytes        []byte
	keyPemBytes         []byte
	certificateDerBytes []byte
}

// Creates a valid pair whose leaf identity lets tests distinguish rotations
// without depending on repository or operator certificates.
func newTestTlsPair(t testing.TB, hostName string) testTlsPair {
	t.Helper()
	certPemBytes, keyPemBytes, err := selfSign(
		[]string{hostName},
		hostName,
		time.Minute,
		2*time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	certificateBlock, _ := pem.Decode(certPemBytes)
	if certificateBlock == nil || certificateBlock.Type != "CERTIFICATE" {
		t.Fatal("self-signed test pair did not contain a certificate")
	}
	return testTlsPair{
		certPemBytes:        certPemBytes,
		keyPemBytes:         keyPemBytes,
		certificateDerBytes: append([]byte(nil), certificateBlock.Bytes...),
	}
}

// Places either or both leaves at one resolver location so partial rotations
// can be assembled without timing or filesystem mutation races.
func writeTestTlsPairLocation(
	t testing.TB,
	directory string,
	resourceName string,
	pair testTlsPair,
	includeCert bool,
	includeKey bool,
) {
	t.Helper()
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if includeCert {
		if err := os.WriteFile(filepath.Join(directory, resourceName+".crt"), pair.certPemBytes, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if includeKey {
		if err := os.WriteFile(filepath.Join(directory, resourceName+".key"), pair.keyPemBytes, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// Requires the loaded leaf to come from the expected complete generation.
func requireTestTlsLeaf(
	t testing.TB,
	transportTls *TransportTls,
	hostName string,
	expectedDerBytes []byte,
) {
	t.Helper()
	tlsConfig, err := transportTls.GetTlsConfig(hostName)
	if err != nil {
		t.Fatal(err)
	}
	if len(tlsConfig.Certificates) != 1 || len(tlsConfig.Certificates[0].Certificate) == 0 {
		t.Fatalf("TLS config certificate chain is incomplete: %+v", tlsConfig.Certificates)
	}
	if !bytes.Equal(tlsConfig.Certificates[0].Certificate[0], expectedDerBytes) {
		t.Fatal("TLS config selected the wrong certificate generation")
	}
}

// Partial direct and newer rotations must not outrank the first location that
// contains both leaves.
func TestTransportTlsFallsBackFromPartialExplicitRotations(t *testing.T) {
	vaultRoot := t.TempDir()
	t.Setenv("WARP_ENV", "local")
	t.Setenv("WARP_VAULT_HOME", vaultRoot)
	hostName := "rotation.example"
	completePair := newTestTlsPair(t, hostName)
	newerPair := newTestTlsPair(t, hostName)
	writeTestTlsPairLocation(
		t,
		filepath.Join(vaultRoot, "all", "tls", "1.0.0", hostName),
		hostName,
		completePair,
		true,
		true,
	)
	writeTestTlsPairLocation(
		t,
		filepath.Join(vaultRoot, "all", "tls", "2.0.0", hostName),
		hostName,
		newerPair,
		true,
		false,
	)
	writeTestTlsPairLocation(
		t,
		filepath.Join(vaultRoot, "tls", hostName),
		hostName,
		completePair,
		false,
		true,
	)
	transportTls := NewTransportTls(
		map[string]bool{hostName: true},
		DefaultTransportTlsSettings(),
	)
	requireTestTlsLeaf(t, transportTls, hostName, completePair.certificateDerBytes)
}

// Leaves that only match cryptographically are not one atomic resolver
// generation and therefore must report a lookup miss.
func TestTransportTlsRejectsLeavesWithoutCommonResolverLocation(t *testing.T) {
	vaultRoot := t.TempDir()
	t.Setenv("WARP_ENV", "local")
	t.Setenv("WARP_VAULT_HOME", vaultRoot)
	hostName := "localhost"
	pair := newTestTlsPair(t, hostName)
	writeTestTlsPairLocation(
		t,
		filepath.Join(vaultRoot, "tls", hostName),
		hostName,
		pair,
		true,
		false,
	)
	writeTestTlsPairLocation(
		t,
		filepath.Join(vaultRoot, "all", "tls", "1.0.0", hostName),
		hostName,
		pair,
		false,
		true,
	)
	transportTls := NewTransportTls(
		map[string]bool{hostName: true},
		DefaultTransportTlsSettings(),
	)
	tlsConfig, err := transportTls.GetTlsConfig(hostName)
	if err == nil || !strings.Contains(err.Error(), "Missing lookup key") {
		t.Fatalf("TLS lookup without a common pair = %v; want lookup miss", err)
	}
	if tlsConfig != nil {
		t.Fatal("TLS lookup without a common pair returned a config")
	}
}

// Wildcard lookup shares the same pair selection and falls back past partial
// direct and newer locations without combining them.
func TestTransportTlsWildcardFallsBackFromPartialRotations(t *testing.T) {
	vaultRoot := t.TempDir()
	t.Setenv("WARP_ENV", "local")
	t.Setenv("WARP_VAULT_HOME", vaultRoot)
	hostName := "api.example.com"
	resourceName := "star.example.com"
	completePair := newTestTlsPair(t, "*.example.com")
	newerPair := newTestTlsPair(t, "*.example.com")
	writeTestTlsPairLocation(
		t,
		filepath.Join(vaultRoot, "all", "tls", "1.0.0", resourceName),
		resourceName,
		completePair,
		true,
		true,
	)
	writeTestTlsPairLocation(
		t,
		filepath.Join(vaultRoot, "all", "tls", "2.0.0", resourceName),
		resourceName,
		newerPair,
		true,
		false,
	)
	writeTestTlsPairLocation(
		t,
		filepath.Join(vaultRoot, "local", "tls", resourceName),
		resourceName,
		completePair,
		false,
		true,
	)
	transportTls := NewTransportTls(
		map[string]bool{"*.example.com": true},
		DefaultTransportTlsSettings(),
	)
	requireTestTlsLeaf(t, transportTls, hostName, completePair.certificateDerBytes)
}

func TestTransportTls(t *testing.T) {
	if os.Getenv("WARP_TEST_ENV_USE_PORTABLE_RESOURCES") != "1" {
		t.Skip("requires the portable synthetic TLS fixture")
	}

	settings := &TransportTlsSettings{
		EnableSelfSign: false,
	}
	transportTls, err := NewTransportTlsFromConfig(settings)
	connect.AssertEqual(t, err, nil)

	tlsConfig, err := transportTls.GetTlsConfig("fixture.example")
	connect.AssertEqual(t, err, nil)
	connect.AssertNotEqual(t, tlsConfig, nil)

	tlsConfig, err = transportTls.GetTlsConfig("unlisted.fixture.example")
	connect.AssertNotEqual(t, err, nil)
	connect.AssertEqual(t, tlsConfig, nil)
}

func TestTransportTlsSelfSign(t *testing.T) {

	settings := &TransportTlsSettings{
		EnableSelfSign: true,
	}
	transportTls, err := NewTransportTlsFromConfig(settings)
	connect.AssertEqual(t, err, nil)

	for _, hostName := range []string{
		"fixture.example",
		"unlisted.fixture.example",
		"deep.unlisted.fixture.example",
	} {
		tlsConfig, err := transportTls.GetTlsConfig(hostName)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, tlsConfig, nil)
	}
}
