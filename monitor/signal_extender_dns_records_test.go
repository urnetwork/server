package monitor

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// §28.2 over synthetic TXT answers. Every record is signed with a key made
// here, and every address is an RFC 5737 or RFC 3849 documentation address.

type extenderDnsSyntheticSource struct {
	*syntheticSource
	names []string
	txtFn func(hostname string) ([]string, error)
}

func (self *extenderDnsSyntheticSource) DNSTxt(_ context.Context, hostname string) ([]string, error) {
	self.names = append(self.names, hostname)
	return self.txtFn(hostname)
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

// Runs the probe once against a fixed answer, and returns its findings'
// states and the names it resolved.
func extenderDnsTestStates(t *testing.T, settings *ExtenderDnsRecordsSettings, environment string, txtFn func(string) ([]string, error)) (map[string]string, []string) {
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
	return states, source.names
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

	answer := func(values []string, err error) func(string) ([]string, error) {
		return func(string) ([]string, error) { return values, err }
	}
	for _, test := range []struct {
		name  string
		txtFn func(string) ([]string, error)
		want  map[string]string
	}{
		{name: "fresh records", txtFn: answer(fresh, nil), want: map[string]string{stale: "healthy", visibility: "healthy"}},
		{name: "the records of 10-28", txtFn: answer(frozen, nil), want: map[string]string{stale: "warn", visibility: "healthy"}},
		{name: "valid but no longer refreshed", txtFn: answer(lagging, nil), want: map[string]string{stale: "warn"}},
		{name: "one fresh record", txtFn: answer(mixed, nil), want: map[string]string{stale: "healthy"}},
		{name: "no txt records", txtFn: answer(nil, &net.DNSError{Err: "no such host", Name: "extender.bringyour.com", IsNotFound: true}), want: map[string]string{stale: "warn", visibility: "healthy"}},
		{name: "values that are not records", txtFn: answer([]string{"v=spf1 -all", revocation}, nil), want: map[string]string{stale: "warn"}},
		{name: "the lookup timed out", txtFn: answer(nil, &net.DNSError{Err: "i/o timeout", IsTimeout: true}), want: map[string]string{stale: "hold", visibility: "unobservable"}},
	} {
		states, names := extenderDnsTestStates(t, DefaultExtenderDnsRecordsSettings(), "main", test.txtFn)
		requireStormStates(t, test.name, states, test.want)
		if len(names) != 1 || names[0] != "extender.bringyour.com" {
			t.Fatalf("%s: resolved %v, want the extender record name", test.name, names)
		}
	}
}

// The stale alert carries the counts and times that explain it, needs two
// runs, and carries no record content.
func TestExtenderDnsRecordsAlertNamesTheServedTimes(t *testing.T) {
	frozenIssue := time.Date(2026, 10, 28, 18, 12, 0, 0, time.UTC)
	values := []string{extenderDnsTestValue(t, frozenIssue), extenderDnsTestValue(t, frozenIssue)}
	source := &extenderDnsSyntheticSource{syntheticSource: &syntheticSource{}, txtFn: func(string) ([]string, error) { return values, nil }}
	alerts, err := NewExtenderDnsRecordsSignal().Run(context.Background(), stormMainSettings(extenderDnsTestNow, source))
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
		"name=extender.bringyour.com", "served=2", "records=2", "valid=0", "expired=2",
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

// An environment with no record name resolves nothing, and a record body that
// does not decode is counted, never trusted.
func TestExtenderDnsRecordsSkipsAndCounts(t *testing.T) {
	states, names := extenderDnsTestStates(t, DefaultExtenderDnsRecordsSettings(), "beta", func(string) ([]string, error) {
		return nil, errors.New("must not resolve")
	})
	if len(states) != 0 || len(names) != 0 {
		t.Fatalf("an environment with no record name: states %v, names %v", states, names)
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

	invalid, _ := extenderDnsTestStates(t, &ExtenderDnsRecordsSettings{}, "main", func(string) ([]string, error) {
		t.Fatal("resolved under an invalid policy")
		return nil, nil
	})
	if invalid["extender-dns-records-unobservable main/extender-dns-records"] != "unobservable" {
		t.Fatalf("invalid policy: %v", invalid)
	}
}
