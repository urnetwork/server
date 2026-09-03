package monitor

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestActivePublicLBAndManagerHostnameUseOnlyCurrentConfiguredTargets(t *testing.T) {
	services := servicesYaml{
		Domain: "bringyour.com",
		Versions: []servicesVersionYaml{
			{
				LB: servicesLBYaml{Interfaces: map[string]map[string]servicesLBInterfaceYaml{
					"by-us-fmt-5-edge-3.bringyour.com": {
						"eno3": {IPv4: "192.0.2.84", IPv6: "2001:db8:5880::382"},
						"eno4": {IPv4: "192.0.2.85"},
					},
					"fireside.bringyour.com": {
						"eno1": {IPv4: "192.0.2.90", IPv6: "2001:db8::90", Transparent: true},
					},
				}},
				Services: map[string]servicesServiceYaml{
					"app": {ExposeAliases: []string{"app.bringyour.com", "manager.bringyour.com"}},
				},
			},
			{
				LB: servicesLBYaml{Interfaces: map[string]map[string]servicesLBInterfaceYaml{
					"by-us-fmt-5-edge-3.bringyour.com": {
						"old0": {IPv4: "192.0.2.200", IPv6: "2001:db8::200"},
					},
				}},
			},
		},
	}

	if got := activeManagerHostnameFromServices(services); got != "manager.bringyour.com" {
		t.Fatalf("manager hostname = %q", got)
	}
	byHost, err := activePublicLBFromServices(services)
	if err != nil {
		t.Fatal(err)
	}
	targets := byHost["by-us-fmt-5-edge-3"]
	if len(targets) != 2 {
		t.Fatalf("public LB targets = %+v, want two", targets)
	}
	if targets[0] != (PublicLBInterfaceSettings{Interface: "eno3", IPv4Address: "192.0.2.84", IPv6Address: "2001:db8:5880::382"}) ||
		targets[1] != (PublicLBInterfaceSettings{Interface: "eno4", IPv4Address: "192.0.2.85"}) {
		t.Fatalf("public LB targets = %+v", targets)
	}
	if _, ok := byHost["fireside"]; ok {
		t.Fatalf("transparent proxy became a public edge TLS target: %+v", byHost)
	}
	for _, target := range targets {
		if target.Interface == "old0" || target.IPv4Address == "192.0.2.200" {
			t.Fatalf("historical version leaked into public TLS targets: %+v", targets)
		}
	}
}

func TestTLSExpirySignalSyntheticExpiredExactEdge(t *testing.T) {
	now := time.Date(2026, 9, 3, 7, 20, 0, 0, time.UTC)
	hostname := "manager.bringyour.com"
	expired := syntheticTLSCertificate(t, []string{"*.bringyour.com"}, time.Date(2024, 4, 16, 0, 0, 0, 0, time.UTC), time.Date(2025, 5, 17, 23, 59, 59, 0, time.UTC))
	healthy := syntheticTLSCertificate(t, []string{hostname}, now.Add(-time.Hour), now.Add(90*24*time.Hour))

	var lock sync.Mutex
	calls := []string{}
	source := &syntheticSource{tlsFn: func(network, address, serverName string) (TLSCertificateObservation, error) {
		if serverName != hostname {
			return TLSCertificateObservation{}, fmt.Errorf("SNI = %q", serverName)
		}
		lock.Lock()
		calls = append(calls, network+" "+address)
		lock.Unlock()
		switch address {
		case "192.0.2.82:443":
			return TLSCertificateObservation{Certificates: [][]byte{expired}}, nil
		case "[2001:db8:5870::2bca]:443":
			return TLSCertificateObservation{Certificates: [][]byte{healthy}}, nil
		default:
			return TLSCertificateObservation{}, fmt.Errorf("unexpected address %s", address)
		}
	}}
	settings := syntheticSettings(source)
	settings.Now = func() time.Time { return now }
	settings.ManagerHostname = hostname
	settings.Hosts = []HostSettings{{
		Name: "by-us-fmt-5-edge-4",
		PublicLB: []PublicLBInterfaceSettings{{
			Interface: "eno3", IPv4Address: "192.0.2.82", IPv6Address: "2001:db8:5870::2bca",
		}},
	}}

	alerts, err := NewTLSExpirySignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 {
		t.Fatalf("alerts = %d, want only expired IPv4 endpoint: %+v", len(alerts), alerts)
	}
	alert := requireAlertClass(t, alerts, "tls-certificate-expired")
	if alert.SignalNumber != "18.2" || alert.SignalKey != "tls-expiry" ||
		alert.Target != "by-us-fmt-5-edge-4/eno3" || alert.Frame != "ipv4" {
		t.Fatalf("wrong TLS alert identity: %+v", alert)
	}
	for _, want := range []string{
		"2025-05-17T23:59:59Z",
		"hostname_covered=true",
		"warpctl certs issue <env>",
		"controlled LB drain",
		"SIGNALS.md §18.2",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("expired TLS alert missing %q:\n%s", want, alert.Markdown())
		}
	}
	lock.Lock()
	sort.Strings(calls)
	gotCalls := append([]string(nil), calls...)
	lock.Unlock()
	wantCalls := []string{"tcp4 192.0.2.82:443", "tcp6 [2001:db8:5870::2bca]:443"}
	if strings.Join(gotCalls, "|") != strings.Join(wantCalls, "|") {
		t.Fatalf("TLS calls = %v, want %v", gotCalls, wantCalls)
	}
}

func TestTLSExpirySignalSyntheticFailureClasses(t *testing.T) {
	now := time.Date(2026, 9, 3, 7, 20, 0, 0, time.UTC)
	hostname := "manager.bringyour.com"
	certificates := map[string][]byte{
		"192.0.2.1:443": syntheticTLSCertificate(t, []string{hostname}, now.Add(time.Hour), now.Add(90*24*time.Hour)),
		"192.0.2.2:443": syntheticTLSCertificate(t, []string{"api.bringyour.com"}, now.Add(-time.Hour), now.Add(90*24*time.Hour)),
		"192.0.2.3:443": syntheticTLSCertificate(t, []string{hostname}, now.Add(-time.Hour), now.Add(90*24*time.Hour)),
		"192.0.2.4:443": syntheticTLSCertificate(t, []string{hostname}, now.Add(-time.Hour), now.Add(tlsExpiryWarningWindow)),
	}
	source := &syntheticSource{tlsFn: func(_ string, address, _ string) (TLSCertificateObservation, error) {
		if address == "192.0.2.5:443" {
			return TLSCertificateObservation{}, errors.New("synthetic handshake timeout")
		}
		observation := TLSCertificateObservation{Certificates: [][]byte{certificates[address]}}
		if address == "192.0.2.3:443" {
			observation.VerifyError = errors.New("x509: certificate signed by unknown authority")
		}
		return observation, nil
	}}
	settings := syntheticSettings(source)
	settings.Now = func() time.Time { return now }
	settings.ManagerHostname = hostname
	settings.Hosts = []HostSettings{{Name: "edge-test", PublicLB: []PublicLBInterfaceSettings{
		{Interface: "future", IPv4Address: "192.0.2.1"},
		{Interface: "mismatch", IPv4Address: "192.0.2.2"},
		{Interface: "untrusted", IPv4Address: "192.0.2.3"},
		{Interface: "expiring", IPv4Address: "192.0.2.4"},
		{Interface: "missing", IPv4Address: "192.0.2.5"},
	}}}

	alerts, err := NewTLSExpirySignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 5 {
		t.Fatalf("alerts = %d, want every distinct failure class: %+v", len(alerts), alerts)
	}
	for _, class := range []string{
		"tls-certificate-not-yet-valid",
		"tls-certificate-hostname",
		"tls-certificate-untrusted",
		"tls-certificate-expiring",
		"tls-certificate-unobservable",
	} {
		alert := requireAlertClass(t, alerts, class)
		if (class == "tls-certificate-expiring" || class == "tls-certificate-unobservable") && alert.Severity != SeverityWarn {
			t.Fatalf("%s severity = %s, want warn", class, alert.Severity)
		}
	}
}

func TestTLSExpirySignalSyntheticHealthyAndGracefulNoop(t *testing.T) {
	now := time.Date(2026, 9, 3, 7, 20, 0, 0, time.UTC)
	hostname := "manager.bringyour.com"
	healthy := syntheticTLSCertificate(t, []string{hostname}, now.Add(-time.Hour), now.Add(tlsExpiryWarningWindow+time.Hour))
	calls := 0
	source := &syntheticSource{tlsFn: func(_, _, _ string) (TLSCertificateObservation, error) {
		calls++
		return TLSCertificateObservation{Certificates: [][]byte{healthy}}, nil
	}}
	settings := syntheticSettings(source)
	settings.Now = func() time.Time { return now }
	settings.Hosts = []HostSettings{{Name: "edge-test", PublicLB: []PublicLBInterfaceSettings{{Interface: "eno1", IPv4Address: "192.0.2.10"}}}}

	alerts, err := NewTLSExpirySignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 || calls != 0 {
		t.Fatalf("unconfigured manager probe did not noop: calls=%d alerts=%+v", calls, alerts)
	}

	settings.ManagerHostname = hostname
	alerts, err = NewTLSExpirySignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 || calls != 1 {
		t.Fatalf("healthy certificate result: calls=%d alerts=%+v", calls, alerts)
	}
}

func syntheticTLSCertificate(t *testing.T, dnsNames []string, notBefore, notAfter time.Time) []byte {
	t.Helper()
	seedMaterial := strings.Join(dnsNames, ",") + notBefore.UTC().Format(time.RFC3339Nano) + notAfter.UTC().Format(time.RFC3339Nano)
	seed := sha256.Sum256([]byte(seedMaterial))
	privateKey := ed25519.NewKeyFromSeed(seed[:])
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               syntheticCertificateSubject("synthetic monitor leaf"),
		DNSNames:              append([]string(nil), dnsNames...),
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(bytes.NewReader(make([]byte, 1024)), template, template, privateKey.Public(), privateKey)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

func syntheticCertificateSubject(commonName string) pkix.Name {
	return pkix.Name{CommonName: commonName}
}
