// pebble is the local tier's ACME server (sidecar DESIGN §19.11's local row, §22.9's "Pebble with 5% bad nonces in T2
// and T3"; TLS-M5-26): Let's Encrypt's Pebble, from the Go module the sidecar test leg pins, behind an ACME listener on
// TLS and a plain management port that hands out what a client needs to trust it.
//
//	pebble -acme :14000 -management :15000 -dns 127.0.0.1:53 -nonce-reject 5
//
//	GET /roots/0    Pebble's issuing root, PEM, which its chains verify under
//	GET /tls-cert   the ACME listener's own certificate, PEM, a throwaway made at start
//	GET /status     {"directory": "/dir"}
//
// Its validation authority asks -dns for every DNS-01 challenge and dials a challenge's host for TLS-ALPN-01, as
// Pebble's own command does; -always-valid takes every challenge as passed, for a run that has no resolver the
// container can reach. Pebble never checks CAA or a customer's accounturi (§22.9), so those rows stay LE staging's.
//
// It is a test fixture: it keeps every account, order and key in memory, issues certificates nothing trusts, and nothing
// in it is fit to face a network. The compose file publishes its ports on 127.0.0.1 alone.
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/letsencrypt/pebble/v2/ca"
	"github.com/letsencrypt/pebble/v2/db"
	"github.com/letsencrypt/pebble/v2/va"
	"github.com/letsencrypt/pebble/v2/wfe"
)

// LE's tlsserver profile, 45 days (sidecar §13.8), beside Pebble's default.
const pebbleProfileTlsServer = "tlsserver"

type PebbleSettings struct {
	// The share of good nonces Pebble refuses as bad, in percent: §22.9's 5.
	NonceRejectPercent int
	// The share of orders whose authorizations Pebble reuses from an earlier valid one, in percent.
	AuthzReusePercent int
	// Every challenge reads as passed, with no lookup or dial.
	AlwaysValid bool
	// What resolves DNS-01 challenges, host:port. Empty takes the system resolver.
	DnsServer string
	// The names the ACME listener's throwaway certificate covers.
	Hostnames []string
	// The throwaway certificate's life from start.
	CertificateLife time.Duration
	// The tlsserver profile's validity: 45 days.
	TlsServerValidity time.Duration
	// How long a stop waits for open requests.
	ShutdownTimeout time.Duration
}

func DefaultPebbleSettings() *PebbleSettings {
	return &PebbleSettings{
		NonceRejectPercent: 5,
		AuthzReusePercent:  0,
		Hostnames:          []string{"localhost", "sidecar-test-pebble", "127.0.0.1"},
		CertificateLife:    30 * 24 * time.Hour,
		TlsServerValidity:  45 * 24 * time.Hour,
		ShutdownTimeout:    10 * time.Second,
	}
}

// One Pebble: its ACME handler and what its management port answers.
type pebbleServer struct {
	acme        http.Handler
	management  http.Handler
	certificate tls.Certificate
}

// A throwaway certificate for hostnames, made at start, on a P-256 key nothing else holds.
func pebbleListenerCertificate(hostnames []string, life time.Duration) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(now.UnixNano()),
		Subject:               pkix.Name{CommonName: hostnames[0]},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(life),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	for _, hostname := range hostnames {
		if ip := net.ParseIP(hostname); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
		} else {
			template.DNSNames = append(template.DNSNames, hostname)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, nil
}

// Pebble as settings configure it. Pebble reads its knobs from the environment as it is built, so they are set here
// first, as its own command reads them.
func newPebbleServer(settings *PebbleSettings, logger *log.Logger) (*pebbleServer, error) {
	if len(settings.Hostnames) == 0 || settings.CertificateLife <= 0 || settings.TlsServerValidity <= 0 {
		return nil, errors.New("pebble needs hostnames and positive lives")
	}
	environment := map[string]string{
		"PEBBLE_VA_NOSLEEP":      "1",
		"PEBBLE_WFE_NONCEREJECT": strconv.Itoa(settings.NonceRejectPercent),
		"PEBBLE_AUTHZREUSE":      strconv.Itoa(settings.AuthzReusePercent),
	}
	if settings.AlwaysValid {
		environment["PEBBLE_VA_ALWAYS_VALID"] = "1"
	}
	for name, value := range environment {
		if err := os.Setenv(name, value); err != nil {
			return nil, err
		}
	}
	memoryStore := db.NewMemoryStore()
	authority := ca.New(logger, memoryStore, "", "ecdsa", 0, 1, map[string]ca.Profile{
		pebbleProfileTlsServer: {Description: "45 days, as LE's tlsserver", ValidityPeriod: uint64(settings.TlsServerValidity.Seconds())},
		"default":              {Description: "Pebble's default"},
	})
	validator := va.New(logger, 0, 0, false, settings.DnsServer, memoryStore)
	frontEnd := wfe.New(logger, memoryStore, validator, authority, []string{"pebble.example"}, false, false, 0, 0)
	certificate, err := pebbleListenerCertificate(settings.Hostnames, settings.CertificateLife)
	if err != nil {
		return nil, err
	}
	rootPem := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: authority.GetRootCert(0).Cert.Raw})
	certificatePem := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Certificate[0]})
	management := http.NewServeMux()
	management.HandleFunc("GET /roots/0", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/x-pem-file")
		writer.Write(rootPem)
	})
	management.HandleFunc("GET /tls-cert", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/x-pem-file")
		writer.Write(certificatePem)
	})
	management.HandleFunc("GET /status", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		json.NewEncoder(writer).Encode(map[string]string{"directory": wfe.DirectoryPath})
	})
	return &pebbleServer{acme: frontEnd.Handler(), management: management, certificate: certificate}, nil
}

// Serves the ACME port on TLS and the management port in the clear until ctx ends, then stops both.
func (self *pebbleServer) serve(ctx context.Context, acmeListener net.Listener, managementListener net.Listener, shutdownTimeout time.Duration) error {
	acmeServer := &http.Server{Handler: self.acme, TLSConfig: &tls.Config{Certificates: []tls.Certificate{self.certificate}, MinVersion: tls.VersionTLS12}}
	managementServer := &http.Server{Handler: self.management}
	served := make(chan error, 2)
	go func() { served <- acmeServer.ServeTLS(acmeListener, "", "") }()
	go func() { served <- managementServer.Serve(managementListener) }()
	var err error
	select {
	case <-ctx.Done():
	case err = <-served:
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	acmeServer.Shutdown(shutdownCtx)
	managementServer.Shutdown(shutdownCtx)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func main() {
	settings := DefaultPebbleSettings()
	acmeAddress := flag.String("acme", ":14000", "the ACME listener, on TLS")
	managementAddress := flag.String("management", ":15000", "the management listener: roots, the listener's certificate and status")
	flag.StringVar(&settings.DnsServer, "dns", "", "what resolves DNS-01 challenges, host:port; empty is the system resolver")
	flag.IntVar(&settings.NonceRejectPercent, "nonce-reject", settings.NonceRejectPercent, "good nonces refused as bad, in percent")
	flag.IntVar(&settings.AuthzReusePercent, "authz-reuse", settings.AuthzReusePercent, "orders whose authorizations are reused, in percent")
	flag.BoolVar(&settings.AlwaysValid, "always-valid", false, "every challenge passes with no lookup or dial")
	flag.Parse()

	logger := log.New(io.Discard, "", 0)
	if os.Getenv("PEBBLE_LOG") == "1" {
		logger = log.New(os.Stderr, "pebble ", log.LstdFlags)
	}
	server, err := newPebbleServer(settings, logger)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	acmeListener, err := net.Listen("tcp", *acmeAddress)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	managementListener, err := net.Listen("tcp", *managementAddress)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := server.serve(ctx, acmeListener, managementListener, settings.ShutdownTimeout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
