package work

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Names the directory holding the local root for the child half of
// TestRefreshGeolocationSourcePinsAgainstLocalRoots.
const geolocationSourcePinLocalRootsEnv = "URNETWORK_GEOLOCATION_SOURCE_PIN_LOCAL_ROOTS"

// The real-source tests observe ipinfo.io's live certificate, so they run only
// with URNETWORK_LIVE_THIRD_PARTY_TESTS=1. This covers the same successful
// observations against a local chain without any seam: a child process trusts
// a throwaway root through SSL_CERT_FILE, the standard way to choose a
// process's system roots, so sourceTLSConfig and observeGeolocationSourcePin
// run exactly as in production.
func TestRefreshGeolocationSourcePinsAgainstLocalRoots(t *testing.T) {
	if directory := os.Getenv(geolocationSourcePinLocalRootsEnv); directory != "" {
		refreshGeolocationSourcePinsAgainstLocalRoots(t, directory)
		return
	}

	directory := t.TempDir()
	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	root := createLocalSourceCertificate(t, "synthetic source root", "", rootKey, nil, nil)
	keyDer, err := x509.MarshalECPrivateKey(rootKey)
	if err != nil {
		t.Fatal(err)
	}
	writeLocalSourcePem(t, filepath.Join(directory, "root.pem"), "CERTIFICATE", root.Raw)
	writeLocalSourcePem(t, filepath.Join(directory, "root-key.pem"), "EC PRIVATE KEY", keyDer)
	emptyCertDir := t.TempDir()

	child := exec.Command(os.Args[0], "-test.run=^TestRefreshGeolocationSourcePinsAgainstLocalRoots$", "-test.count=1", "-test.v")
	child.Env = append(
		os.Environ(),
		geolocationSourcePinLocalRootsEnv+"="+directory,
		"SSL_CERT_FILE="+filepath.Join(directory, "root.pem"),
		"SSL_CERT_DIR="+emptyCertDir,
	)
	output, err := child.CombinedOutput()
	if err != nil || !bytes.Contains(output, []byte("--- PASS: TestRefreshGeolocationSourcePinsAgainstLocalRoots")) {
		t.Fatalf("local roots child did not pass: %v\n%s", err, output)
	}
}

// Runs in the child, whose system roots are only the local root.
func refreshGeolocationSourcePinsAgainstLocalRoots(t *testing.T, directory string) {
	rootBlock, _ := pem.Decode(readLocalSourceFile(t, filepath.Join(directory, "root.pem")))
	keyBlock, _ := pem.Decode(readLocalSourceFile(t, filepath.Join(directory, "root-key.pem")))
	if rootBlock == nil || keyBlock == nil {
		t.Fatal("local root files are not PEM")
	}
	root, err := x509.ParseCertificate(rootBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	rootKey, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	intermediateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	intermediate := createLocalSourceCertificate(t, "synthetic source intermediate", "", intermediateKey, root, rootKey)
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	host := "ipinfo.io"
	leaf := createLocalSourceCertificate(t, host, host, leafKey, intermediate, intermediateKey)

	source := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	source.TLS = &tls.Config{Certificates: []tls.Certificate{{
		Certificate: [][]byte{leaf.Raw, intermediate.Raw},
		PrivateKey:  leafKey,
	}}}
	source.StartTLS()
	defer source.Close()
	sourceUrl, err := url.Parse(source.URL)
	if err != nil {
		t.Fatal(err)
	}
	addr := sourceUrl.Host
	badAddr, stop := selfSignedTLSServer(t)
	defer stop()

	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		// one bad host does not cost the good host its first observation
		changes, errs := refreshGeolocationSourcePins(ctx, []geolocationSourceTarget{
			{Host: "free.freeipapi.com", Addr: badAddr},
			{Host: host, Addr: addr},
		})
		connect.AssertEqual(t, len(errs), 1)
		connect.AssertEqual(t, changes, []GeolocationSourcePinChange{{
			Host:             host,
			NewLeaf:          spkiPin(leaf),
			NewIntermediate:  spkiPin(intermediate),
			FirstObservation: true,
		}})
		if model.GetGeolocationSourcePin(ctx, "free.freeipapi.com") != nil {
			t.Fatal("a failed observation wrote a row")
		}

		// a rotation reports the old and new pins and stores the new ones
		model.SetGeolocationSourcePin(ctx, &model.GeolocationSourcePin{
			Host:             host,
			LeafSpki:         "stale-leaf",
			IntermediateSpki: "stale-intermediate",
			ObservedAt:       server.NowUtc().Add(-24 * time.Hour),
		})
		changes, errs = refreshGeolocationSourcePins(ctx, []geolocationSourceTarget{{Host: host, Addr: addr}})
		connect.AssertEqual(t, len(errs), 0)
		connect.AssertEqual(t, changes, []GeolocationSourcePinChange{{
			Host:            host,
			OldLeaf:         "stale-leaf",
			NewLeaf:         spkiPin(leaf),
			OldIntermediate: "stale-intermediate",
			NewIntermediate: spkiPin(intermediate),
		}})
		stored := model.GetGeolocationSourcePin(ctx, host)
		connect.AssertEqual(t, stored.LeafSpki, spkiPin(leaf))
		connect.AssertEqual(t, stored.IntermediateSpki, spkiPin(intermediate))
		// an unchanged certificate is not a rotation
		changes, errs = refreshGeolocationSourcePins(ctx, []geolocationSourceTarget{{Host: host, Addr: addr}})
		connect.AssertEqual(t, len(errs), 0)
		connect.AssertEqual(t, len(changes), 0)

		// a trusted chain for another name leaves that name's row unchanged
		observedAt := server.NowUtc().Add(-time.Hour).Truncate(time.Millisecond)
		model.SetGeolocationSourcePin(ctx, &model.GeolocationSourcePin{
			Host:             "free.freeipapi.com",
			LeafSpki:         "good-leaf",
			IntermediateSpki: "good-intermediate",
			ObservedAt:       observedAt,
		})
		changes, errs = refreshGeolocationSourcePins(ctx, []geolocationSourceTarget{{Host: "free.freeipapi.com", Addr: addr}})
		connect.AssertEqual(t, len(errs), 1)
		connect.AssertEqual(t, len(changes), 0)
		after := model.GetGeolocationSourcePin(ctx, "free.freeipapi.com")
		connect.AssertEqual(t, after.LeafSpki, "good-leaf")
		connect.AssertEqual(t, after.IntermediateSpki, "good-intermediate")
		connect.AssertEqual(t, after.ObservedAt.UTC().Equal(observedAt.UTC()), true)
	})
}

// A CA when dnsName is empty, else a leaf for dnsName. A nil issuer self-signs.
func createLocalSourceCertificate(
	t *testing.T,
	commonName string,
	dnsName string,
	key *ecdsa.PrivateKey,
	issuer *x509.Certificate,
	issuerKey *ecdsa.PrivateKey,
) *x509.Certificate {
	t.Helper()
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	if dnsName == "" {
		template.IsCA = true
		template.BasicConstraintsValid = true
		template.KeyUsage = x509.KeyUsageCertSign
	} else {
		template.DNSNames = []string{dnsName}
		template.KeyUsage = x509.KeyUsageDigitalSignature
		template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	}
	if issuer == nil {
		issuer, issuerKey = template, key
	}
	der, err := x509.CreateCertificate(rand.Reader, template, issuer, &key.PublicKey, issuerKey)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return certificate
}

func writeLocalSourcePem(t *testing.T, path string, blockType string, der []byte) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readLocalSourceFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
