package monitor

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"gopkg.in/yaml.v3"
)

var dnsFixtureA = []string{"192.0.2.10", "198.51.100.20"}
var dnsFixtureAAAA = []string{"2001:db8::10", "2001:db8:1::20"}

type dnsAliasSyntheticSource struct {
	*syntheticSource
	authoritativeFn func(context.Context, string, string, DNSRecordType) (DNSAuthoritativeObservation, error)
	recursiveFn     func(context.Context, string, DNSRecordType) (DNSResponseObservation, error)
}

func (s *dnsAliasSyntheticSource) DNSAuthoritative(ctx context.Context, zone, hostname string, recordType DNSRecordType) (DNSAuthoritativeObservation, error) {
	if s.authoritativeFn == nil {
		return DNSAuthoritativeObservation{}, fmt.Errorf("unexpected authoritative DNS observation")
	}
	return s.authoritativeFn(ctx, zone, hostname, recordType)
}

func (s *dnsAliasSyntheticSource) DNSRecursive(ctx context.Context, hostname string, recordType DNSRecordType) (DNSResponseObservation, error) {
	if s.recursiveFn == nil {
		return DNSResponseObservation{}, fmt.Errorf("unexpected recursive DNS observation")
	}
	return s.recursiveFn(ctx, hostname, recordType)
}

func healthyDNSAliasSource() *dnsAliasSyntheticSource {
	return &dnsAliasSyntheticSource{
		syntheticSource: &syntheticSource{},
		authoritativeFn: func(_ context.Context, _ string, hostname string, recordType DNSRecordType) (DNSAuthoritativeObservation, error) {
			expected := dnsFixtureExpected(hostname, recordType)
			return DNSAuthoritativeObservation{
				NameserverCount: 2,
				Responses: []DNSResponseObservation{
					{Addresses: append([]string(nil), expected...), ResponseCode: DNSResponseSuccess, Authoritative: true},
					{Addresses: append([]string(nil), expected...), ResponseCode: DNSResponseSuccess, Authoritative: true},
				},
			}, nil
		},
		recursiveFn: func(_ context.Context, hostname string, recordType DNSRecordType) (DNSResponseObservation, error) {
			return DNSResponseObservation{
				Addresses:    dnsFixtureExpected(hostname, recordType),
				ResponseCode: DNSResponseSuccess,
			}, nil
		},
	}
}

func dnsFixtureExpected(hostname string, recordType DNSRecordType) []string {
	label := strings.SplitN(hostname, ".", 2)[0]
	switch recordType {
	case DNSRecordA:
		if strings.HasSuffix(label, "-v6") {
			return nil
		}
		return append([]string(nil), dnsFixtureA...)
	case DNSRecordAAAA:
		if strings.HasSuffix(label, "-v4") {
			return nil
		}
		return append([]string(nil), dnsFixtureAAAA...)
	default:
		return nil
	}
}

func dnsAliasFixtureSettings(source SignalSource) SignalSettings {
	settings := syntheticSettings(source)
	settings.Environment = "stage"
	settings.DNSAliases = DNSAliasSettings{
		Enabled:        true,
		ManagedDomains: []string{"example.test"},
		ExpectedA:      append([]string(nil), dnsFixtureA...),
		ExpectedAAAA:   append([]string(nil), dnsFixtureAAAA...),
	}
	return settings
}

func TestDNSAliasesSignalSyntheticHealthyMatrix(t *testing.T) {
	source := healthyDNSAliasSource()
	var lock sync.Mutex
	calls := []string{}
	baseAuthoritative := source.authoritativeFn
	baseRecursive := source.recursiveFn
	source.authoritativeFn = func(ctx context.Context, zone, hostname string, recordType DNSRecordType) (DNSAuthoritativeObservation, error) {
		lock.Lock()
		calls = append(calls, "authority "+zone+" "+hostname+" "+string(recordType))
		lock.Unlock()
		return baseAuthoritative(ctx, zone, hostname, recordType)
	}
	source.recursiveFn = func(ctx context.Context, hostname string, recordType DNSRecordType) (DNSResponseObservation, error) {
		lock.Lock()
		calls = append(calls, "recursive "+hostname+" "+string(recordType))
		lock.Unlock()
		return baseRecursive(ctx, hostname, recordType)
	}
	settings := dnsAliasFixtureSettings(source)
	settings.DNSAliases.ManagedDomains = []string{"example.test", "secondary.example.test"}

	alerts, err := NewDNSAliasesSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("healthy DNS matrix alerted: %+v", alerts)
	}
	lock.Lock()
	gotCalls := append([]string(nil), calls...)
	lock.Unlock()
	if len(gotCalls) != 48 {
		t.Fatalf("DNS calls = %d, want 2 domains * 6 aliases * 2 families * 2 layers", len(gotCalls))
	}
	for _, want := range []string{
		"authority example.test alt.example.test A",
		"authority example.test stage-alt-v4.example.test AAAA",
		"recursive stage-alt-v6.secondary.example.test A",
		"recursive alt.secondary.example.test AAAA",
	} {
		if !containsString(gotCalls, want) {
			t.Fatalf("DNS matrix did not call %q", want)
		}
	}
}

func TestDNSAliasesSignalSyntheticAuthoritativeMismatchSuppressesRecursiveDuplicate(t *testing.T) {
	source := healthyDNSAliasSource()
	baseAuthoritative := source.authoritativeFn
	baseRecursive := source.recursiveFn
	source.authoritativeFn = func(ctx context.Context, zone, hostname string, recordType DNSRecordType) (DNSAuthoritativeObservation, error) {
		if hostname == "alt.example.test" && recordType == DNSRecordA {
			return DNSAuthoritativeObservation{
				NameserverCount: 2,
				Responses: []DNSResponseObservation{
					{Addresses: []string{"203.0.113.99"}, ResponseCode: DNSResponseSuccess, Authoritative: true},
					{Addresses: append([]string(nil), dnsFixtureA...), ResponseCode: DNSResponseSuccess, Authoritative: true, CNAMECount: 1},
				},
			}, nil
		}
		return baseAuthoritative(ctx, zone, hostname, recordType)
	}
	source.recursiveFn = func(ctx context.Context, hostname string, recordType DNSRecordType) (DNSResponseObservation, error) {
		if hostname == "alt.example.test" && recordType == DNSRecordA {
			return DNSResponseObservation{Addresses: []string{"203.0.113.98"}, ResponseCode: DNSResponseSuccess}, nil
		}
		return baseRecursive(ctx, hostname, recordType)
	}

	alerts, err := NewDNSAliasesSignal().Run(context.Background(), dnsAliasFixtureSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 {
		t.Fatalf("alerts = %d, want one authoritative root cause: %+v", len(alerts), alerts)
	}
	alert := requireAlertClass(t, alerts, "dns-authoritative-rrset")
	if alert.SignalNumber != "18.3" || alert.SignalKey != "dns-aliases" ||
		alert.Target != "alt.example.test" || alert.Frame != "A" {
		t.Fatalf("wrong DNS alert identity: %+v", alert)
	}
	for _, want := range []string{
		"expected_count=2", "mismatching_count=2", "cname_count=1",
		"missing_count=2", "unexpected_count=1", "SIGNALS.md §18.3",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("authoritative alert missing %q:\n%s", want, alert.Markdown())
		}
	}
	requireAlertOmits(t, alert, "203.0.113.99", "203.0.113.98")
}

func TestDNSRecursiveFindingCannotResolveWhileAuthorityIsNotExact(t *testing.T) {
	result := dnsAliasResult{
		target: dnsAliasTarget{
			zone: "example.test", hostname: "alt.example.test",
			recordType: DNSRecordA, expected: append([]string(nil), dnsFixtureA...),
		},
		recursive: DNSResponseObservation{
			Addresses: append([]string(nil), dnsFixtureA...), ResponseCode: DNSResponseSuccess,
		},
	}
	for _, state := range []dnsAuthorityState{dnsAuthorityUnknown, dnsAuthorityBroken} {
		findings := dnsRecursiveFindings(result, state)
		for _, finding := range findings {
			if finding.class == "dns-recursive-answer-family" {
				t.Fatalf("authority state %d emitted recursive RRset health: %+v", state, finding)
			}
		}
	}
}

func TestDNSAliasesSignalSyntheticAuthoritativeNXDomainIsNotNoData(t *testing.T) {
	source := healthyDNSAliasSource()
	baseAuthoritative := source.authoritativeFn
	source.authoritativeFn = func(ctx context.Context, zone, hostname string, recordType DNSRecordType) (DNSAuthoritativeObservation, error) {
		if hostname == "alt-v4.example.test" && recordType == DNSRecordAAAA {
			return DNSAuthoritativeObservation{
				NameserverCount: 2,
				Responses: []DNSResponseObservation{
					{ResponseCode: DNSResponseNameError, Authoritative: true},
					{ResponseCode: DNSResponseNameError, Authoritative: true},
				},
			}, nil
		}
		return baseAuthoritative(ctx, zone, hostname, recordType)
	}

	alerts, err := NewDNSAliasesSignal().Run(context.Background(), dnsAliasFixtureSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 {
		t.Fatalf("alerts = %d, want authoritative NXDOMAIN only: %+v", len(alerts), alerts)
	}
	alert := requireAlertClass(t, alerts, "dns-authoritative-rrset")
	if alert.Target != "alt-v4.example.test" || alert.Frame != "AAAA" ||
		!strings.Contains(alert.Observed, "non_success_count=2") ||
		!strings.Contains(alert.Observed, "response_codes=name-error:2") {
		t.Fatalf("authoritative NXDOMAIN was not distinguished from NODATA: %s", alert.Markdown())
	}
}

func TestDNSAliasesSignalSyntheticAuthoritativeForbiddenFamily(t *testing.T) {
	source := healthyDNSAliasSource()
	baseAuthoritative := source.authoritativeFn
	source.authoritativeFn = func(ctx context.Context, zone, hostname string, recordType DNSRecordType) (DNSAuthoritativeObservation, error) {
		if hostname == "alt-v6.example.test" && recordType == DNSRecordA {
			return DNSAuthoritativeObservation{
				NameserverCount: 2,
				Responses: []DNSResponseObservation{
					{Addresses: []string{"203.0.113.77"}, ResponseCode: DNSResponseSuccess, Authoritative: true},
					{ResponseCode: DNSResponseSuccess, Authoritative: true},
				},
			}, nil
		}
		return baseAuthoritative(ctx, zone, hostname, recordType)
	}

	alerts, err := NewDNSAliasesSignal().Run(context.Background(), dnsAliasFixtureSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 {
		t.Fatalf("alerts = %d, want forbidden-family authoritative drift: %+v", len(alerts), alerts)
	}
	alert := requireAlertClass(t, alerts, "dns-authoritative-rrset")
	if alert.Target != "alt-v6.example.test" || alert.Frame != "A" ||
		!strings.Contains(alert.Observed, "expected_count=0") ||
		!strings.Contains(alert.Observed, "unexpected_count=1") {
		t.Fatalf("forbidden-family drift lacks bounded evidence: %s", alert.Markdown())
	}
	requireAlertOmits(t, alert, "203.0.113.77")
}

func TestDNSAliasesSignalSyntheticRecursiveExactSetAndNoData(t *testing.T) {
	source := healthyDNSAliasSource()
	baseRecursive := source.recursiveFn
	source.recursiveFn = func(ctx context.Context, hostname string, recordType DNSRecordType) (DNSResponseObservation, error) {
		switch {
		case hostname == "alt.example.test" && recordType == DNSRecordA:
			return DNSResponseObservation{
				Addresses: []string{dnsFixtureA[0], "203.0.113.97"}, ResponseCode: DNSResponseSuccess,
			}, nil
		case hostname == "alt-v4.example.test" && recordType == DNSRecordAAAA:
			return DNSResponseObservation{ResponseCode: DNSResponseNameError}, nil
		default:
			return baseRecursive(ctx, hostname, recordType)
		}
	}

	alerts, err := NewDNSAliasesSignal().Run(context.Background(), dnsAliasFixtureSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 2 {
		t.Fatalf("alerts = %d, want stale same-family set and NXDOMAIN: %+v", len(alerts), alerts)
	}
	seen := map[string]Alert{}
	for _, alert := range alerts {
		if alert.Class != "dns-recursive-answer-family" {
			t.Fatalf("unexpected recursive alert: %+v", alert)
		}
		seen[alert.Target+"/"+alert.Frame] = alert
		if alert.Severity != SeverityWarn || alert.PageSustain != 3 {
			t.Fatalf("recursive severity/sustain = %s/%d, want warn/3: %+v", alert.Severity, alert.PageSustain, alert)
		}
	}
	stale := seen["alt.example.test/A"]
	if !strings.Contains(stale.Observed, "missing_count=1") || !strings.Contains(stale.Observed, "unexpected_count=1") {
		t.Fatalf("stale same-family set lacks bounded delta: %s", stale.Markdown())
	}
	nxdomain := seen["alt-v4.example.test/AAAA"]
	if !strings.Contains(nxdomain.Observed, "response_code=name-error") ||
		!strings.Contains(nxdomain.Mechanism, "NXDOMAIN instead of NOERROR/NODATA") {
		t.Fatalf("NXDOMAIN was not distinguished from NODATA: %s", nxdomain.Markdown())
	}
	requireAlertOmits(t, stale, "203.0.113.97", dnsFixtureA[0])
}

func TestDNSAliasesSignalSyntheticObservationFailuresAreExplicitAndRedacted(t *testing.T) {
	source := healthyDNSAliasSource()
	baseAuthoritative := source.authoritativeFn
	baseRecursive := source.recursiveFn
	source.authoritativeFn = func(ctx context.Context, zone, hostname string, recordType DNSRecordType) (DNSAuthoritativeObservation, error) {
		switch {
		case hostname == "alt.example.test" && recordType == DNSRecordA:
			return DNSAuthoritativeObservation{
				NameserverCount:   2,
				FailedNameservers: 1,
				Responses: []DNSResponseObservation{{
					Addresses: append([]string(nil), dnsFixtureA...), ResponseCode: DNSResponseSuccess, Authoritative: true,
				}},
			}, nil
		case hostname == "alt.example.test" && recordType == DNSRecordAAAA:
			return DNSAuthoritativeObservation{}, errors.New("fixture authority endpoint unavailable")
		default:
			return baseAuthoritative(ctx, zone, hostname, recordType)
		}
	}
	source.recursiveFn = func(ctx context.Context, hostname string, recordType DNSRecordType) (DNSResponseObservation, error) {
		if hostname == "alt-v4.example.test" && recordType == DNSRecordA {
			return DNSResponseObservation{}, errors.New("fixture recursive response unavailable")
		}
		return baseRecursive(ctx, hostname, recordType)
	}

	alerts, err := NewDNSAliasesSignal().Run(context.Background(), dnsAliasFixtureSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 3 {
		t.Fatalf("alerts = %d, want partial authority, failed authority, and recursive visibility: %+v", len(alerts), alerts)
	}
	classes := map[string]int{}
	for _, alert := range alerts {
		classes[alert.Class]++
		if alert.Severity != SeverityWarn || alert.Sustain != 2 {
			t.Fatalf("visibility severity/sustain = %s/%d, want warn/2: %+v", alert.Severity, alert.Sustain, alert)
		}
		requireAlertOmits(t, alert, "fixture authority endpoint unavailable", "fixture recursive response unavailable")
	}
	if classes["dns-authoritative-unobservable"] != 2 || classes["dns-recursive-unobservable"] != 1 {
		t.Fatalf("visibility classes = %+v", classes)
	}
	partial := alertByIdentity(t, alerts, "dns-authoritative-unobservable", "alt.example.test", "A")
	if !strings.Contains(partial.Observed, "authority_count=2") || !strings.Contains(partial.Observed, "failed_count=1") {
		t.Fatalf("partial authority counts missing: %s", partial.Markdown())
	}
}

func TestDNSAliasesSignalSyntheticOptionalConfigAndCancellation(t *testing.T) {
	t.Run("absent block noops", func(t *testing.T) {
		var calls atomic.Int32
		source := healthyDNSAliasSource()
		source.authoritativeFn = func(context.Context, string, string, DNSRecordType) (DNSAuthoritativeObservation, error) {
			calls.Add(1)
			return DNSAuthoritativeObservation{}, nil
		}
		alerts, err := NewDNSAliasesSignal().Run(context.Background(), syntheticSettings(source))
		if err != nil || len(alerts) != 0 || calls.Load() != 0 {
			t.Fatalf("absent dns_aliases block: alerts=%+v err=%v calls=%d", alerts, err, calls.Load())
		}
	})

	t.Run("present invalid block alerts before source", func(t *testing.T) {
		var calls atomic.Int32
		source := healthyDNSAliasSource()
		source.authoritativeFn = func(context.Context, string, string, DNSRecordType) (DNSAuthoritativeObservation, error) {
			calls.Add(1)
			return DNSAuthoritativeObservation{}, nil
		}
		settings := dnsAliasFixtureSettings(source)
		settings.DNSAliases.ExpectedA = nil
		alerts, err := NewDNSAliasesSignal().Run(context.Background(), settings)
		if err != nil {
			t.Fatal(err)
		}
		alert := requireAlertClass(t, alerts, "dns-alias-config-invalid")
		if calls.Load() != 0 || !strings.Contains(alert.Observed, "invalid_categories=expected_a") {
			t.Fatalf("invalid block did not fail before source: calls=%d alert=%s", calls.Load(), alert.Markdown())
		}
	})

	t.Run("cancellation returns context error without alert", func(t *testing.T) {
		started := make(chan struct{}, 1)
		source := healthyDNSAliasSource()
		source.authoritativeFn = func(ctx context.Context, _, _ string, _ DNSRecordType) (DNSAuthoritativeObservation, error) {
			select {
			case started <- struct{}{}:
			default:
			}
			<-ctx.Done()
			return DNSAuthoritativeObservation{}, ctx.Err()
		}
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan struct {
			alerts Alerts
			err    error
		}, 1)
		go func() {
			alerts, err := NewDNSAliasesSignal().Run(ctx, dnsAliasFixtureSettings(source))
			result <- struct {
				alerts Alerts
				err    error
			}{alerts: alerts, err: err}
		}()
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("DNS source did not start")
		}
		cancel()
		select {
		case got := <-result:
			if !errors.Is(got.err, context.Canceled) || len(got.alerts) != 0 {
				t.Fatalf("canceled run alerts=%+v err=%v", got.alerts, got.err)
			}
		case <-time.After(time.Second):
			t.Fatal("canceled DNS run did not return")
		}
	})
}

func TestDNSAliasSettingsLoaderCopiesOptionalDesiredState(t *testing.T) {
	if got := dnsAliasSettingsFromMonitorYaml(monitorYaml{}); got.Enabled {
		t.Fatalf("absent dns_aliases enabled probe: %+v", got)
	}
	var y monitorYaml
	if err := yaml.Unmarshal([]byte(`dns_aliases:
  managed_domains: [example.test]
  expected_a: [192.0.2.10]
  expected_aaaa: [2001:db8::10]
`), &y); err != nil {
		t.Fatal(err)
	}
	got := dnsAliasSettingsFromMonitorYaml(y)
	if !got.Enabled || strings.Join(got.ManagedDomains, ",") != "example.test" ||
		strings.Join(got.ExpectedA, ",") != "192.0.2.10" ||
		strings.Join(got.ExpectedAAAA, ",") != "2001:db8::10" {
		t.Fatalf("loaded DNS aliases = %+v", got)
	}
	y.DNSAliases.Content[1].Content[0].Value = "changed.example.test"
	if got.ManagedDomains[0] != "example.test" {
		t.Fatalf("loaded DNS aliases share yaml backing storage: %+v", got)
	}
}

func TestDNSAliasSettingsExplicitNullFailsClosed(t *testing.T) {
	var y monitorYaml
	if err := yaml.Unmarshal([]byte("dns_aliases: null\n"), &y); err != nil {
		t.Fatal(err)
	}
	settings := syntheticSettings(healthyDNSAliasSource())
	settings.DNSAliases = dnsAliasSettingsFromMonitorYaml(y)
	if !settings.DNSAliases.Enabled {
		t.Fatal("explicit dns_aliases null was treated as an absent block")
	}
	alerts, err := NewDNSAliasesSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "dns-alias-config-invalid")
	for _, category := range []string{"managed_domains", "expected_a", "expected_aaaa"} {
		if !strings.Contains(alert.Observed, category) {
			t.Errorf("explicit null alert omits %q: %s", category, alert.Markdown())
		}
	}
}

func TestDNSAliasTargetsRejectInvalidDesiredState(t *testing.T) {
	valid := DNSAliasSettings{
		Enabled:        true,
		ManagedDomains: []string{"example.test"},
		ExpectedA:      append([]string(nil), dnsFixtureA...),
		ExpectedAAAA:   append([]string(nil), dnsFixtureAAAA...),
	}
	for _, test := range []struct {
		name        string
		environment string
		mutate      func(*DNSAliasSettings)
		want        string
	}{
		{
			name: "duplicate domain", environment: "main", want: "managed_domains",
			mutate: func(settings *DNSAliasSettings) {
				settings.ManagedDomains = []string{"example.test", "EXAMPLE.TEST."}
			},
		},
		{
			name: "duplicate A address", environment: "main", want: "expected_a",
			mutate: func(settings *DNSAliasSettings) {
				settings.ExpectedA = []string{dnsFixtureA[0], dnsFixtureA[0]}
			},
		},
		{
			name: "wrong A family", environment: "main", want: "expected_a",
			mutate: func(settings *DNSAliasSettings) {
				settings.ExpectedA = []string{dnsFixtureAAAA[0]}
			},
		},
		{
			name: "wrong AAAA family", environment: "main", want: "expected_aaaa",
			mutate: func(settings *DNSAliasSettings) {
				settings.ExpectedAAAA = []string{dnsFixtureA[0]}
			},
		},
		{
			name: "invalid environment", environment: "main.invalid", want: "environment",
			mutate: func(*DNSAliasSettings) {},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			settings := valid
			settings.ManagedDomains = append([]string(nil), valid.ManagedDomains...)
			settings.ExpectedA = append([]string(nil), valid.ExpectedA...)
			settings.ExpectedAAAA = append([]string(nil), valid.ExpectedAAAA...)
			test.mutate(&settings)
			targets, invalid := dnsAliasTargets(settings, test.environment)
			if len(targets) != 0 || strings.Join(invalid, ",") != test.want {
				t.Fatalf("targets=%+v invalid=%v, want only %q", targets, invalid, test.want)
			}
		})
	}
}

func TestParseDNSResponseAcceptsCaseChangedQuestionAndRejectsCNAMEAsDirectRRset(t *testing.T) {
	queryName := dnsmessage.MustNewName("alt.example.test.")
	responseName := dnsmessage.MustNewName("ALT.Example.TEST.")
	targetName := dnsmessage.MustNewName("target.example.test.")
	message := dnsmessage.Message{
		Header:    dnsmessage.Header{ID: 17, Response: true, Authoritative: true, RCode: dnsmessage.RCodeSuccess},
		Questions: []dnsmessage.Question{{Name: responseName, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}},
		Answers: []dnsmessage.Resource{
			{
				Header: dnsmessage.ResourceHeader{Name: responseName, Type: dnsmessage.TypeCNAME, Class: dnsmessage.ClassINET},
				Body:   &dnsmessage.CNAMEResource{CNAME: targetName},
			},
			{
				Header: dnsmessage.ResourceHeader{Name: targetName, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET},
				Body:   &dnsmessage.AResource{A: [4]byte{192, 0, 2, 10}},
			},
		},
	}
	packet, err := message.Pack()
	if err != nil {
		t.Fatal(err)
	}
	observation, truncated, err := parseDNSResponse(packet, 17, queryName, dnsmessage.TypeA)
	if err != nil {
		t.Fatal(err)
	}
	if truncated || observation.CNAMECount != 1 || len(observation.Addresses) != 0 ||
		!observation.Authoritative || observation.ResponseCode != DNSResponseSuccess {
		t.Fatalf("parsed case-changed CNAME response = %+v truncated=%t", observation, truncated)
	}
}

func TestParseDNSResponseDoesNotUseNonINAnswerForINRRset(t *testing.T) {
	name := dnsmessage.MustNewName("alt.example.test.")
	message := dnsmessage.Message{
		Header:    dnsmessage.Header{ID: 23, Response: true, Authoritative: true, RCode: dnsmessage.RCodeSuccess},
		Questions: []dnsmessage.Question{{Name: name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}},
		Answers: []dnsmessage.Resource{{
			Header: dnsmessage.ResourceHeader{Name: name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassCHAOS},
			Body:   &dnsmessage.AResource{A: [4]byte{192, 0, 2, 10}},
		}},
	}
	packet, err := message.Pack()
	if err != nil {
		t.Fatal(err)
	}
	observation, truncated, err := parseDNSResponse(packet, 23, name, dnsmessage.TypeA)
	if err != nil {
		t.Fatal(err)
	}
	if truncated || len(observation.Addresses) != 0 || !observation.Authoritative ||
		observation.ResponseCode != DNSResponseSuccess {
		t.Fatalf("non-IN answer polluted IN RRset: observation=%+v truncated=%t", observation, truncated)
	}
}

func TestAbsoluteDNSNamePreventsSearchSuffixFallback(t *testing.T) {
	for _, input := range []string{"alt.example.test", "alt.example.test."} {
		if got := absoluteDNSName(input); got != "alt.example.test." {
			t.Fatalf("absoluteDNSName(%q) = %q, want rooted public name", input, got)
		}
	}
}

func TestDNSWireQueryRetriesTruncatedUDPOverTCP(t *testing.T) {
	tcpListener, err := net.Listen("tcp", net.JoinHostPort("localhost", "0"))
	if err != nil {
		t.Fatal(err)
	}
	defer tcpListener.Close()
	udpListener, err := net.ListenPacket("udp", tcpListener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer udpListener.Close()

	serverErrors := make(chan error, 2)
	var udpCalls atomic.Int32
	var tcpCalls atomic.Int32
	go func() {
		packet := make([]byte, 4096)
		count, peer, readErr := udpListener.ReadFrom(packet)
		if readErr != nil {
			serverErrors <- readErr
			return
		}
		udpCalls.Add(1)
		var query dnsmessage.Message
		if unpackErr := query.Unpack(packet[:count]); unpackErr != nil {
			serverErrors <- unpackErr
			return
		}
		message := dnsmessage.Message{
			Header: dnsmessage.Header{
				ID: query.Header.ID, Response: true, Authoritative: true,
				Truncated: true, RCode: dnsmessage.RCodeSuccess,
			},
			Questions: query.Questions,
		}
		response, packErr := message.Pack()
		if packErr == nil {
			_, packErr = udpListener.WriteTo(response, peer)
		}
		serverErrors <- packErr
	}()
	go func() {
		connection, acceptErr := tcpListener.Accept()
		if acceptErr != nil {
			serverErrors <- acceptErr
			return
		}
		defer connection.Close()
		tcpCalls.Add(1)
		var length [2]byte
		if _, readErr := io.ReadFull(connection, length[:]); readErr != nil {
			serverErrors <- readErr
			return
		}
		packet := make([]byte, int(binary.BigEndian.Uint16(length[:])))
		if _, readErr := io.ReadFull(connection, packet); readErr != nil {
			serverErrors <- readErr
			return
		}
		var query dnsmessage.Message
		if unpackErr := query.Unpack(packet); unpackErr != nil {
			serverErrors <- unpackErr
			return
		}
		message := dnsmessage.Message{
			Header:    dnsmessage.Header{ID: query.Header.ID, Response: true, Authoritative: true, RCode: dnsmessage.RCodeSuccess},
			Questions: query.Questions,
			Answers: []dnsmessage.Resource{{
				Header: dnsmessage.ResourceHeader{Name: query.Questions[0].Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET},
				Body:   &dnsmessage.AResource{A: [4]byte{192, 0, 2, 44}},
			}},
		}
		response, packErr := message.Pack()
		if packErr != nil {
			serverErrors <- packErr
			return
		}
		framed := make([]byte, 2+len(response))
		binary.BigEndian.PutUint16(framed[:2], uint16(len(response)))
		copy(framed[2:], response)
		serverErrors <- writeDNSPacket(connection, framed)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	observation, err := dnsWireQuery(ctx, "udp", tcpListener.Addr().String(), "alt.example.test", DNSRecordA)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if serverErr := <-serverErrors; serverErr != nil {
			t.Fatal(serverErr)
		}
	}
	if udpCalls.Load() != 1 || tcpCalls.Load() != 1 {
		t.Fatalf("DNS transport calls udp=%d tcp=%d, want one each", udpCalls.Load(), tcpCalls.Load())
	}
	if !observation.Authoritative || observation.ResponseCode != DNSResponseSuccess ||
		strings.Join(observation.Addresses, ",") != "192.0.2.44" {
		t.Fatalf("TCP fallback observation = %+v", observation)
	}
}

func alertByIdentity(t *testing.T, alerts []Alert, class, target, frame string) Alert {
	t.Helper()
	for _, alert := range alerts {
		if alert.Class == class && alert.Target == target && alert.Frame == frame {
			return alert
		}
	}
	t.Fatalf("no alert %s/%s/%s in %+v", class, target, frame, alerts)
	return Alert{}
}
