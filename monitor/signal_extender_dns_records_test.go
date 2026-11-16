package monitor

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// §28.2 over synthetic resolver answers. Every record is signed with a key
// made here, and every address is an RFC 5737 or RFC 3849 documentation
// address.

type extenderDnsSyntheticSource struct {
	*syntheticSource
	stateLock sync.Mutex
	asked     []string
	txtFn     func(resolver string, hostname string) (DNSResponseObservation, error)
}

func (self *extenderDnsSyntheticSource) DNSTxt(_ context.Context, resolver, hostname string, _ netip.Prefix) (DNSResponseObservation, error) {
	self.stateLock.Lock()
	self.asked = append(self.asked, resolver+" "+hostname)
	self.stateLock.Unlock()
	return self.txtFn(resolver, hostname)
}

var extenderDnsTestNow = time.Date(2026, 11, 11, 21, 30, 0, 0, time.UTC)

// One TXT value as the publisher writes it: a record signed under a test root
// key, issued at issue, alive for a day.
func extenderDnsTestValue(t *testing.T, issue time.Time) string {
	t.Helper()
	seed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	rootPrivateKey, err := connect.ExtenderPrivateKeyFromSeed(seed)
	if err != nil {
		t.Fatal(err)
	}
	record, err := connect.SignExtenderRecord(rootPrivateKey, &protocol.ExtenderRecordBody{
		PublicKey: []byte("extender-dns-test-key-0123456789"),
		Addresses: []*protocol.ExtenderAddress{
			{Ip: "192.0.2.50", IpVersion: 4, Carriers: []string{connect.ExtenderCarrierTcp}},
			{Ip: "2001:db8::50", IpVersion: 6, Carriers: []string{connect.ExtenderCarrierTcp}},
		},
		TcpPort:       443,
		UdpPort:       443,
		DnsPort:       53,
		DnsTld:        connect.DefaultExtenderDnsTld,
		CountryCode:   "us",
		ContinentCode: "NA",
		IssueTimeMs:   uint64(issue.UnixMilli()),
		ExpireTimeMs:  uint64(issue.Add(24 * time.Hour).UnixMilli()),
		NetworkHost:   "example.org",
	})
	if err != nil {
		t.Fatal(err)
	}
	value, err := connect.EncodeExtenderDnsRecord(&protocol.ExtenderGossipMessage{
		Message: &protocol.ExtenderGossipMessage_Record{Record: record},
	})
	if err != nil {
		t.Fatal(err)
	}
	return value
}

// A resolver's successful answer of these values.
func extenderDnsTestAnswer(values []string) DNSResponseObservation {
	return DNSResponseObservation{ResponseCode: DNSResponseSuccess, Texts: values}
}

// The default settings with the whole-zone half alone: no region's path is
// asked (extender_dns_countries_test.go judges those).
func extenderDnsZoneSettings() *ExtenderDnsRecordsSettings {
	settings := DefaultExtenderDnsRecordsSettings()
	settings.Countries = nil
	return settings
}

// Runs the probe once, and returns its findings' states and what it asked.
func extenderDnsTestStates(t *testing.T, settings *ExtenderDnsRecordsSettings, environment string, txtFn func(string, string) (DNSResponseObservation, error)) (map[string]string, []string) {
	t.Helper()
	source := &extenderDnsSyntheticSource{syntheticSource: &syntheticSource{}, txtFn: txtFn}
	signalSettings := stormMainSettings(extenderDnsTestNow, source)
	signalSettings.Environment = environment
	env, err := newProbeEnv(signalSettings.withDefaults())
	if err != nil {
		t.Fatal(err)
	}
	findings, err := extenderDnsRecordsProbe{settings: settings}.check(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]string{}
	for _, one := range findings {
		state := one.tier
		switch {
		case one.clearHold:
			state = "hold"
		case one.healthy:
			state = "healthy"
		case one.isUnobservable():
			state = "unobservable"
		}
		states[one.class+" "+one.target] = state
	}
	sort.Strings(source.asked)
	return states, source.asked
}

func TestExtenderDnsRecordsJudgesWhatClientsAreServed(t *testing.T) {
	const (
		stale      = "extender-dns-records-stale main/extender-dns-records"
		visibility = "extender-dns-records-unobservable main/extender-dns-records"
	)
	fresh := []string{
		extenderDnsTestValue(t, extenderDnsTestNow.Add(-4*time.Minute)),
		extenderDnsTestValue(t, extenderDnsTestNow.Add(-4*time.Minute)),
		extenderDnsTestValue(t, extenderDnsTestNow.Add(-6*time.Minute)),
	}
	// 2026-11-11: thirteen records issued 10-28 18:12Z, expired 10-29 18:12Z
	frozenIssue := time.Date(2026, 10, 28, 18, 12, 0, 0, time.UTC)
	frozen := []string{}
	for range 13 {
		frozen = append(frozen, extenderDnsTestValue(t, frozenIssue))
	}
	// the tick stopped landing three hours ago: still valid, no longer fresh
	lagging := []string{
		extenderDnsTestValue(t, extenderDnsTestNow.Add(-3*time.Hour)),
		extenderDnsTestValue(t, extenderDnsTestNow.Add(-3*time.Hour)),
	}
	// one fresh record among stale ones is a set that lands
	mixed := append(append([]string{}, frozen...), extenderDnsTestValue(t, extenderDnsTestNow.Add(-2*time.Minute)))
	revocation, err := connect.EncodeExtenderDnsRecord(&protocol.ExtenderGossipMessage{
		Message: &protocol.ExtenderGossipMessage_Revocation{Revocation: &protocol.ExtenderRevocation{Body: []byte("x")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	timeout := &net.DNSError{Err: "i/o timeout", IsTimeout: true}

	every := func(observation DNSResponseObservation, err error) func(string, string) (DNSResponseObservation, error) {
		return func(string, string) (DNSResponseObservation, error) { return observation, err }
	}
	for _, test := range []struct {
		name  string
		txtFn func(string, string) (DNSResponseObservation, error)
		want  map[string]string
	}{
		{name: "fresh records", txtFn: every(extenderDnsTestAnswer(fresh), nil), want: map[string]string{stale: "healthy", visibility: "healthy"}},
		{name: "the records of 10-28", txtFn: every(extenderDnsTestAnswer(frozen), nil), want: map[string]string{stale: "warn", visibility: "healthy"}},
		{name: "valid but no longer refreshed", txtFn: every(extenderDnsTestAnswer(lagging), nil), want: map[string]string{stale: "warn"}},
		{name: "one fresh record", txtFn: every(extenderDnsTestAnswer(mixed), nil), want: map[string]string{stale: "healthy"}},
		{name: "no txt records", txtFn: every(extenderDnsTestAnswer(nil), nil), want: map[string]string{stale: "warn", visibility: "healthy"}},
		{name: "no such name", txtFn: every(DNSResponseObservation{ResponseCode: DNSResponseNameError}, nil), want: map[string]string{stale: "warn"}},
		{name: "values that are not records", txtFn: every(extenderDnsTestAnswer([]string{"v=spf1 -all", revocation}), nil), want: map[string]string{stale: "warn"}},
		{name: "every resolver timed out", txtFn: every(DNSResponseObservation{}, timeout), want: map[string]string{stale: "hold", visibility: "unobservable"}},
		{name: "every resolver failed the name", txtFn: every(DNSResponseObservation{ResponseCode: DNSResponseServerFailure}, nil), want: map[string]string{stale: "hold", visibility: "unobservable"}},
		{name: "one resolver answers", txtFn: func(resolver string, _ string) (DNSResponseObservation, error) {
			if resolver == "8.8.8.8:53" {
				return extenderDnsTestAnswer(fresh), nil
			}
			return DNSResponseObservation{}, timeout
		}, want: map[string]string{stale: "healthy", visibility: "healthy"}},
		{name: "one resolver's stale cache", txtFn: func(resolver string, _ string) (DNSResponseObservation, error) {
			if resolver == "9.9.9.9:53" {
				return extenderDnsTestAnswer(frozen), nil
			}
			return extenderDnsTestAnswer(fresh), nil
		}, want: map[string]string{stale: "healthy"}},
	} {
		states, asked := extenderDnsTestStates(t, extenderDnsZoneSettings(), "main", test.txtFn)
		requireStormStates(t, test.name, states, test.want)
		if strings.Join(asked, ",") != "1.1.1.1:53 extender.bringyour.com,8.8.8.8:53 extender.bringyour.com,9.9.9.9:53 extender.bringyour.com" {
			t.Fatalf("%s: asked %v, want every resolver for the extender record name", test.name, asked)
		}
	}
}

// The stale alert carries the counts and times that explain it, needs two
// runs, and carries no record content.
func TestExtenderDnsRecordsAlertNamesTheServedTimes(t *testing.T) {
	frozenIssue := time.Date(2026, 10, 28, 18, 12, 0, 0, time.UTC)
	values := []string{extenderDnsTestValue(t, frozenIssue), extenderDnsTestValue(t, frozenIssue)}
	source := &extenderDnsSyntheticSource{syntheticSource: &syntheticSource{}, txtFn: func(resolver string, _ string) (DNSResponseObservation, error) {
		if resolver == "1.1.1.1:53" {
			return DNSResponseObservation{}, errors.New("synthetic unreachable")
		}
		return extenderDnsTestAnswer(values), nil
	}}
	if got := (extenderDnsRecordsProbe{}).cadence(); got != 10*time.Minute {
		t.Fatalf("cadence = %s", got)
	}
	alerts, err := NewExtenderDnsRecordsSignalWithSettings(extenderDnsZoneSettings()).Run(context.Background(), stormMainSettings(extenderDnsTestNow, source))
	if err != nil {
		t.Fatal(err)
	}
	var stale *Alert
	for index := range alerts {
		requireAlertOmits(t, alerts[index], "192.0.2.50", "2001:db8", values[0][:16])
		if alerts[index].Class == "extender-dns-records-stale" {
			stale = &alerts[index]
		}
	}
	if stale == nil {
		t.Fatalf("no stale alert in %+v", alerts)
	}
	for _, want := range []string{
		"name=extender.bringyour.com", "resolvers_answered=2/3", "served=2", "records=2", "valid=0", "expired=2",
		fmt.Sprintf("newest_issued_unix=%d", frozenIssue.Unix()),
		fmt.Sprintf("newest_issued_age_seconds=%.0f", extenderDnsTestNow.Sub(frozenIssue).Seconds()),
		fmt.Sprintf("latest_expires_unix=%d", frozenIssue.Add(24*time.Hour).Unix()),
		"reasons=all_expired,newest_too_old",
	} {
		if !strings.Contains(stale.Observed, want) {
			t.Fatalf("stale alert lost %q: %s", want, stale.Observed)
		}
	}
	if stale.SignalNumber != "28.2" || stale.Severity != SeverityWarn {
		t.Fatalf("stale alert = %+v", stale)
	}
}

// An environment with no record name asks nothing, a record body that does
// not decode is counted and never trusted, and an invalid policy asks nothing.
func TestExtenderDnsRecordsSkipsAndCounts(t *testing.T) {
	states, asked := extenderDnsTestStates(t, DefaultExtenderDnsRecordsSettings(), "beta", func(string, string) (DNSResponseObservation, error) {
		return DNSResponseObservation{}, errors.New("must not resolve")
	})
	if len(states) != 0 || len(asked) != 0 {
		t.Fatalf("an environment with no record name: states %v, asked %v", states, asked)
	}

	broken, err := proto.Marshal(&protocol.ExtenderGossipMessage{
		Message: &protocol.ExtenderGossipMessage_Record{Record: &protocol.ExtenderRecord{Body: []byte{0xff, 0xff, 0xff}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	observation := observeExtenderDnsRecords([]string{
		base64.StdEncoding.EncodeToString(broken),
		"not base64 at all",
		extenderDnsTestValue(t, extenderDnsTestNow.Add(-time.Minute)),
		extenderDnsTestValue(t, extenderDnsTestNow.Add(-time.Minute)),
	}, extenderDnsTestNow, 3)
	if observation.served != 4 || observation.records != 1 || observation.undecodable != 3 || observation.valid != 1 {
		t.Fatalf("observation = %+v", observation)
	}

	invalid, asked := extenderDnsTestStates(t, &ExtenderDnsRecordsSettings{}, "main", func(string, string) (DNSResponseObservation, error) {
		return DNSResponseObservation{}, errors.New("must not resolve")
	})
	if invalid["extender-dns-records-unobservable main/extender-dns-records"] != "unobservable" || len(asked) != 0 {
		t.Fatalf("invalid policy: %v, asked %v", invalid, asked)
	}
}

// The wire parser takes a TXT answer's records as one value each, its
// character strings joined as a client joins them, and nothing of another
// type or class.
func TestParseDNSResponseJoinsTxtCharacterStrings(t *testing.T) {
	name := dnsmessage.MustNewName("extender.example.test.")
	long := strings.Repeat("A", 255)
	message := dnsmessage.Message{
		Header:    dnsmessage.Header{ID: 29, Response: true, RCode: dnsmessage.RCodeSuccess},
		Questions: []dnsmessage.Question{{Name: name, Type: dnsmessage.TypeTXT, Class: dnsmessage.ClassINET}},
		Answers: []dnsmessage.Resource{
			{
				Header: dnsmessage.ResourceHeader{Name: name, Type: dnsmessage.TypeTXT, Class: dnsmessage.ClassINET},
				Body:   &dnsmessage.TXTResource{TXT: []string{long, "BBB"}},
			},
			{
				Header: dnsmessage.ResourceHeader{Name: name, Type: dnsmessage.TypeTXT, Class: dnsmessage.ClassINET},
				Body:   &dnsmessage.TXTResource{TXT: []string{"second"}},
			},
			{
				Header: dnsmessage.ResourceHeader{Name: name, Type: dnsmessage.TypeTXT, Class: dnsmessage.ClassCHAOS},
				Body:   &dnsmessage.TXTResource{TXT: []string{"not internet"}},
			},
		},
	}
	packet, err := message.Pack()
	if err != nil {
		t.Fatal(err)
	}
	observation, truncated, err := parseDNSResponse(packet, 29, name, dnsmessage.TypeTXT)
	if err != nil {
		t.Fatal(err)
	}
	if truncated || observation.ResponseCode != DNSResponseSuccess || len(observation.Addresses) != 0 ||
		len(observation.Texts) != 2 || observation.Texts[0] != long+"BBB" || observation.Texts[1] != "second" {
		t.Fatalf("parsed txt response = %+v truncated=%t", observation, truncated)
	}
	if recordType, err := dnsMessageType(DNSRecordTXT); err != nil || recordType != dnsmessage.TypeTXT {
		t.Fatalf("txt record type = %v %v", recordType, err)
	}
}

// A public resolver asked without recursion desired answers SERVFAIL or
// REFUSED (1.1.1.1, 8.8.8.8 and 9.9.9.9 on 2026-11-11), so the TXT question
// sets it; an authoritative nameserver is still asked without it.
func TestDNSQuestionSetsRecursionDesiredOnlyForResolvers(t *testing.T) {
	for _, recursionDesired := range []bool{false, true} {
		packet, queryID, _, questionType, err := buildDNSQuestion("extender.example.test", DNSRecordTXT, recursionDesired)
		if err != nil {
			t.Fatal(err)
		}
		var query dnsmessage.Message
		if err := query.Unpack(packet); err != nil {
			t.Fatal(err)
		}
		if query.Header.ID != queryID || query.Header.RecursionDesired != recursionDesired || questionType != dnsmessage.TypeTXT ||
			len(query.Questions) != 1 || query.Questions[0].Name.String() != "extender.example.test." {
			t.Fatalf("question with recursion desired %t = %+v", recursionDesired, query)
		}
	}
}
