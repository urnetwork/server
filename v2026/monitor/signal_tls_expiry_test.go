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
		"build and deploy the LB service image",
		"run-edges.sh` alone",
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

func TestTLSExpirySignalSyntheticObserverNoRouteAggregatesIPv6AndKeepsIPv4(t *testing.T) {
	now := time.Date(2026, 9, 4, 15, 24, 0, 0, time.UTC)
	hostname := "manager.bringyour.com"
	expired := syntheticTLSCertificate(
		t,
		[]string{hostname},
		now.Add(-90*24*time.Hour),
		now.Add(-time.Hour),
	)
	ipv6Addresses := []string{"[2001:db8:1::10]:443", "[2001:db8:2::20]:443"}
	routeCalls := 0
	source := &syntheticSource{
		localFn: func(name string, args ...string) (string, error) {
			if name != "/sbin/route" || strings.Join(args, " ") != "-n get -inet6 "+ipv6ObserverRouteProbeAddress {
				return "", fmt.Errorf("unexpected local command %s %s", name, strings.Join(args, " "))
			}
			routeCalls++
			return "route: writing to routing socket: not in table\n", errors.New("exit status 1")
		},
		tlsFn: func(network, address, _ string) (TLSCertificateObservation, error) {
			if network == "tcp4" {
				return TLSCertificateObservation{Certificates: [][]byte{expired}}, nil
			}
			return TLSCertificateObservation{}, fmt.Errorf(
				"dial tcp6 %s: connect: no route to host",
				address,
			)
		},
	}
	settings := syntheticSettings(source)
	settings.Now = func() time.Time { return now }
	settings.ManagerHostname = hostname
	settings.Hosts = []HostSettings{
		{Name: "edge-a", PublicLB: []PublicLBInterfaceSettings{{
			Interface: "public0", IPv4Address: "192.0.2.10", IPv6Address: "2001:db8:1::10",
		}}},
		{Name: "edge-b", PublicLB: []PublicLBInterfaceSettings{{
			Interface: "public0", IPv6Address: "2001:db8:2::20",
		}}},
	}

	env, err := newProbeEnv(settings.withDefaults())
	if err != nil {
		t.Fatal(err)
	}
	findings, err := (tlsExpiryProbe{}).check(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	if routeCalls != 2 {
		t.Fatalf("route calls = %d, want before/after lookups", routeCalls)
	}
	observer := findingByClass(t, findings, "ipv6-observer-route-unavailable")
	if observer.healthy || observer.target != "monitor-host/tls-expiry" {
		t.Fatalf("observer finding = %+v", observer)
	}
	for _, want := range []string{
		"observer_route=absent",
		"configured_ipv6_targets=2",
		"no_route_failures=2",
	} {
		if !strings.Contains(observer.observed, want) {
			t.Fatalf("observer finding missing %q: %+v", want, observer)
		}
	}
	for _, forbidden := range append(ipv6Addresses, "not in table", "no route to host") {
		if strings.Contains(observer.observed+observer.evidence+observer.mechanism, forbidden) {
			t.Fatalf("observer finding leaked %q: %+v", forbidden, observer)
		}
	}
	resolved := 0
	expiry := 0
	for _, finding := range findings {
		switch finding.class {
		case "tls-certificate-unobservable":
			if !finding.healthy {
				t.Fatalf("common route loss emitted per-edge TLS unknown: %+v", finding)
			}
			resolved++
		case "tls-certificate-expired":
			if finding.healthy || finding.frame != "ipv4" {
				t.Fatalf("IPv4 expiry changed during IPv6 observer loss: %+v", finding)
			}
			expiry++
		}
	}
	if resolved != 2 || expiry != 1 {
		t.Fatalf("resolved=%d expiry=%d findings=%+v", resolved, expiry, findings)
	}
}

func TestTLSExpirySignalSyntheticNoRouteNeedsObserverRouteProof(t *testing.T) {
	tests := []struct {
		name        string
		routeOutput string
		routeErr    error
	}{
		{
			name:        "route available",
			routeOutput: "route to: 2606:4700:4700::1111\ninterface: en0\n",
		},
		{
			name:        "route unobservable",
			routeOutput: "synthetic private route diagnostic",
			routeErr:    errors.New("synthetic route command failure"),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := &syntheticSource{
				localFn: func(string, ...string) (string, error) {
					return test.routeOutput, test.routeErr
				},
				tlsFn: func(_ string, address string, _ string) (TLSCertificateObservation, error) {
					return TLSCertificateObservation{}, fmt.Errorf(
						"dial tcp6 %s: connect: no route to host",
						address,
					)
				},
			}
			settings := syntheticSettings(source)
			settings.ManagerHostname = "manager.bringyour.com"
			settings.Hosts = []HostSettings{{Name: "edge-a", PublicLB: []PublicLBInterfaceSettings{
				{Interface: "public0", IPv6Address: "2001:db8:3::30"},
				{Interface: "public1", IPv6Address: "2001:db8:3::31"},
			}}}

			alerts, err := NewTLSExpirySignal().Run(context.Background(), settings)
			if err != nil {
				t.Fatal(err)
			}
			if len(alerts) != 2 {
				t.Fatalf("alerts = %d, want per-target unknown without observer proof: %+v", len(alerts), alerts)
			}
			for _, alert := range alerts {
				if alert.Class != "tls-certificate-unobservable" || !strings.HasPrefix(alert.Target, "edge-a/") {
					t.Fatalf("TLS alert = %+v", alert)
				}
			}
		})
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
