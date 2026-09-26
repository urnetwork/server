package acceptance

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func diagnosticTLSCertificate(t *testing.T, issuer string) (tls.Certificate, *x509.Certificate) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	root := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: issuer},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	rootDER, err := x509.CreateCertificate(rand.Reader, root, root, public, private)
	if err != nil {
		t.Fatal(err)
	}
	root, err = x509.ParseCertificate(rootDER)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "validation.example"},
		DNSNames:     []string{"validation.example"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, root, public, private)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{leafDER, rootDER}, PrivateKey: private}, root
}

// Use the real net/http handshake boundary: its failure callback supplies an
// empty ConnectionState even though CertificateVerificationError holds every
// parsed certificate. A prior verified response must not hide a later failure.
func TestProbeHTTPSRetainsRejectedChainFromRealTLSHandshake(t *testing.T) {
	trusted, root := diagnosticTLSCertificate(t, "trusted validation root")
	rejected, _ := diagnosticTLSCertificate(t, "None")
	var handshakes, requests atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.TLS = &tls.Config{GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		if handshakes.Add(1) == 1 {
			return &trusted, nil
		}
		return &rejected, nil
	}}
	server.StartTLS()
	defer server.Close()
	pool := x509.NewCertPool()
	pool.AddCert(root)
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "validation.example"}}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		connection, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if err == nil {
			origin, _ := netip.ParseAddrPort(connection.RemoteAddr().String())
			recordHTTPSOrigin(ctx, origin)
		}
		return connection, err
	}
	defer transport.CloseIdleConnections()

	successes, err := probeHTTPSCampaign(
		context.Background(), "HTTP CONNECT", server.URL, transport,
		time.Second, 5*time.Second, time.Second,
		func(context.Context, time.Duration) error { return nil }, nil, nil,
	)
	if successes != 1 || requests.Load() != 1 || handshakes.Load() != 2 {
		t.Fatalf("successes=%d requests=%d handshakes=%d; want one success then one terminal rejection", successes, requests.Load(), handshakes.Load())
	}
	var verification *tls.CertificateVerificationError
	var authority x509.UnknownAuthorityError
	if !errors.As(err, &verification) || !errors.As(err, &authority) {
		t.Fatalf("lost normal TLS verification error: %v", err)
	}
	for _, evidence := range []string{
		"sustained request 1/5 failed after 1 successful requests",
		"phase tls_handshake_failed",
		"peer_certs=2 verified_chains=0",
		"certificate_source=verification_error",
		"validation.example>None/",
		"None>None/",
		"origin " + server.Listener.Addr().String(),
	} {
		if !strings.Contains(err.Error(), evidence) {
			t.Errorf("failure lacks %q: %v", evidence, err)
		}
	}
	var requestFailure *httpsRequestFailure
	if !errors.As(err, &requestFailure) || requestFailure.originAddress.String() != server.Listener.Addr().String() {
		t.Fatalf("failure lost the pre-TLS origin endpoint: %v", err)
	}
	for _, der := range rejected.Certificate {
		fingerprint := sha256.Sum256(der)
		if !strings.Contains(err.Error(), fmt.Sprintf("/%x", fingerprint[:6])) {
			t.Errorf("failure lost certificate fingerprint: %v", err)
		}
		if strings.Contains(err.Error(), string(der)) {
			t.Fatal("failure leaked certificate contents")
		}
	}
}

func TestTLSVerificationDiagnosticRetainsAdjacentRejections(t *testing.T) {
	certificate, _ := diagnosticTLSCertificate(t, "test issuer")
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	for name, rejection := range map[string]error{
		"hostname": x509.HostnameError{Certificate: leaf, Host: "other.example"},
		"expired":  x509.CertificateInvalidError{Cert: leaf, Reason: x509.Expired},
	} {
		t.Run(name, func(t *testing.T) {
			wrapped := fmt.Errorf("request: %w", &tls.CertificateVerificationError{
				UnverifiedCertificates: []*x509.Certificate{leaf}, Err: rejection,
			})
			detail := tlsVerificationErrorCertificateDiagnostic(wrapped)
			if !strings.Contains(detail, "peer_certs=1 verified_chains=0") ||
				!strings.Contains(detail, "validation.example>test_issuer/") {
				t.Fatalf("rejected chain unavailable: %q", detail)
			}
		})
	}
	if got := tlsVerificationErrorCertificateDiagnostic(io.EOF); got != "" {
		t.Fatalf("non-verification error reported a certificate: %q", got)
	}
}
