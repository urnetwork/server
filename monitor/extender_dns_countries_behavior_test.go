package monitor

import (
	"context"
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// §28.2's per-country half over synthetic answers, through the default
// settings, whose paths come from the sdk's per-country DNS. This file uses
// only what the signal exposed before the half existed, so it also runs
// against that code, where it fails: nothing asks a region's resolvers.

// A source that answers every transport the signal asks: plain DNS by
// resolver (the TXT question, and the A question asked of a resolver that
// left it unanswered), DoH by url, and the authoritative nameservers by
// client subnet.
type extenderDnsCountrySource struct {
	*syntheticSource
	stateLock     sync.Mutex
	asked         []string
	txt           func(resolver string) (DNSResponseObservation, error)
	address       func(resolver string) (DNSResponseObservation, error)
	doh           func(dohUrl string, maxBytes int) (int, []byte, error)
	authoritative func(clientSubnet netip.Prefix) (DNSResponseObservation, error)
}

func (self *extenderDnsCountrySource) DNSAddress(_ context.Context, resolver, _ string) (DNSResponseObservation, error) {
	self.record("address " + resolver)
	if self.address == nil {
		return DNSResponseObservation{}, fmt.Errorf("synthetic %s: no answer", resolver)
	}
	return self.address(resolver)
}

func (self *extenderDnsCountrySource) record(question string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.asked = append(self.asked, question)
}

func (self *extenderDnsCountrySource) askedSorted() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	asked := append([]string(nil), self.asked...)
	sort.Strings(asked)
	return asked
}

func (self *extenderDnsCountrySource) DNSTxt(_ context.Context, resolver, _ string, _ netip.Prefix) (DNSResponseObservation, error) {
	self.record("dns " + resolver)
	return self.txt(resolver)
}

func (self *extenderDnsCountrySource) DNSDohTxt(_ context.Context, dohUrl, _ string, maxBytes int, _ netip.Prefix) (int, []byte, error) {
	self.record("doh " + dohUrl)
	return self.doh(dohUrl, maxBytes)
}

func (self *extenderDnsCountrySource) DNSNameservers(_ context.Context, _ string) ([]string, error) {
	self.record("nameservers")
	return []string{"192.0.2.53:53"}, nil
}

func (self *extenderDnsCountrySource) DNSTxtAuthoritative(_ context.Context, _ string, servers []string, clientSubnet netip.Prefix) (DNSResponseObservation, error) {
	if clientSubnet.IsValid() {
		self.record("authoritative " + clientSubnet.String())
	} else {
		self.record("authoritative")
	}
	if strings.Join(servers, ",") != "192.0.2.53:53" {
		return DNSResponseObservation{}, fmt.Errorf("asked nameservers %v", servers)
	}
	return self.authoritative(clientSubnet)
}

// One extender's TXT value, as the publisher writes it: a record of its own
// key, signed under a test root key, issued at issue, alive for a day.
func extenderDnsCountryTestValue(t testing.TB, index int, issue time.Time) string {
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
		PublicKey:     []byte(fmt.Sprintf("extender-dns-country-key-%07d", index)),
		Addresses:     []*protocol.ExtenderAddress{{Ip: "192.0.2.60", IpVersion: 4, Carriers: []string{connect.ExtenderCarrierTcp}}},
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

// The values of extenders first..first+count-1, issued at issue.
func extenderDnsCountryTestValues(t testing.TB, first int, count int, issue time.Time) []string {
	values := []string{}
	for index := first; index < first+count; index++ {
		values = append(values, extenderDnsCountryTestValue(t, index, issue))
	}
	return values
}

// A DoH answer's body: the values as TXT records, each split into character
// strings of 255 bytes as Route 53 serves them, after edit adjusts the
// message.
func extenderDnsCountryDohBody(t testing.TB, values []string, edit func(message *dnsmessage.Message)) []byte {
	t.Helper()
	name := dnsmessage.MustNewName("extender.bringyour.com.")
	message := dnsmessage.Message{
		Header:    dnsmessage.Header{Response: true, RecursionDesired: true, RecursionAvailable: true, RCode: dnsmessage.RCodeSuccess},
		Questions: []dnsmessage.Question{{Name: name, Type: dnsmessage.TypeTXT, Class: dnsmessage.ClassINET}},
	}
	for _, value := range values {
		strs := []string{}
		for 255 < len(value) {
			strs = append(strs, value[:255])
			value = value[255:]
		}
		strs = append(strs, value)
		message.Answers = append(message.Answers, dnsmessage.Resource{
			Header: dnsmessage.ResourceHeader{Name: name, Type: dnsmessage.TypeTXT, Class: dnsmessage.ClassINET, TTL: 60},
			Body:   &dnsmessage.TXTResource{TXT: strs},
		})
	}
	if edit != nil {
		edit(&message)
	}
	body, err := message.Pack()
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// Runs the probe once over source and returns its findings' states by "class
// target": hold, healthy, unobservable, or the tier of a detection.
func extenderDnsCountryStates(t *testing.T, settings *ExtenderDnsRecordsSettings, source SignalSource) (map[string]string, []finding) {
	t.Helper()
	env, err := newProbeEnv(stormMainSettings(extenderDnsTestNow, source).withDefaults())
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
		identity := one.class + " " + one.target
		if previous, ok := states[identity]; ok {
			t.Fatalf("%s reported twice (%s, %s)", identity, previous, state)
		}
		states[identity] = state
	}
	return states, findings
}

// The answers of 2026-11-12, as the monitor host read them, with a failure
// of each kind on the regions' paths: Alidns's DoH serves 3 of the 15
// records, DNSPod's DoH an answer marked truncated with none, Electro refuses
// foreign questions; and here Shecan's cache serves expired records, Yandex
// drops the TXT, one NSDI address answers that the name does not exist, a
// DNSPod DoH address is unreachable, and the default 1.1.1.1 DoH answer runs
// past what a client reads.
func TestExtenderDnsRecordsJudgesEachCountryResolver(t *testing.T) {
	const zone = "main/extender-dns-records"
	fresh := extenderDnsCountryTestValues(t, 0, 15, extenderDnsTestNow.Add(-4*time.Minute))
	expired := extenderDnsCountryTestValues(t, 0, 15, extenderDnsTestNow.Add(-50*time.Hour))
	oversize := append(append([]string{}, fresh...), extenderDnsCountryTestValues(t, 100, 75, extenderDnsTestNow.Add(-4*time.Minute))...)

	source := &extenderDnsCountrySource{
		syntheticSource: &syntheticSource{},
		txt: func(resolver string) (DNSResponseObservation, error) {
			switch resolver {
			case "1.1.1.1:53", "8.8.8.8:53", "9.9.9.9:53", "9.9.9.10:53", "195.208.4.1:53":
				return extenderDnsTestAnswer(fresh), nil
			case "178.22.122.100:53":
				return extenderDnsTestAnswer(expired), nil
			case "78.157.42.100:53":
				return DNSResponseObservation{ResponseCode: DNSResponseRefused}, nil
			case "77.88.8.8:53":
				return extenderDnsTestAnswer(nil), nil
			case "195.208.5.1:53":
				return DNSResponseObservation{ResponseCode: DNSResponseNameError}, nil
			}
			return DNSResponseObservation{}, fmt.Errorf("asked %s, which no client of its region asks", resolver)
		},
		doh: func(dohUrl string, maxBytes int) (int, []byte, error) {
			switch dohUrl {
			case "https://223.5.5.5/dns-query", "https://223.6.6.6/dns-query":
				return 200, extenderDnsCountryDohBody(t, fresh[:3], nil), nil
			case "https://1.12.12.12/dns-query":
				return 200, extenderDnsCountryDohBody(t, nil, func(message *dnsmessage.Message) { message.Header.Truncated = true }), nil
			case "https://120.53.53.53/dns-query":
				return 0, nil, fmt.Errorf("doh query: %w", syscall.ECONNREFUSED)
			case "https://1.1.1.1/dns-query":
				body := extenderDnsCountryDohBody(t, oversize, nil)
				if len(body) <= 16*1024 || maxBytes < len(body) {
					return 0, nil, fmt.Errorf("oversize body of %d bytes for a read of %d", len(body), maxBytes)
				}
				return 200, body, nil
			}
			return 200, extenderDnsCountryDohBody(t, fresh, nil), nil
		},
		authoritative: func(netip.Prefix) (DNSResponseObservation, error) {
			return extenderDnsTestAnswer(fresh), nil
		},
	}
	states, findings := extenderDnsCountryStates(t, DefaultExtenderDnsRecordsSettings(), source)

	stale := func(path string) string { return "extender-dns-country-stale " + zone + "/" + path }
	partial := func(path string) string { return "extender-dns-country-partial " + zone + "/" + path }
	requireStormStates(t, "the regions' paths", states, map[string]string{
		"extender-dns-records-stale " + zone:                       "healthy",
		"extender-dns-country-unobservable " + zone + "/countries": "healthy",
		// the undisclosed path: the default DoH servers
		partial("default/DoH 1.1.1.1"):  "warn",
		stale("default/DoH 1.1.1.1"):    "healthy",
		partial("default/DoH 9.9.9.10"): "healthy",
		stale("default/DoH 9.9.9.10"):   "healthy",
		// cn: its DoH presets yield records, so its plain resolvers are not asked
		partial("cn/DoH 223.5.5.5"):    "warn",
		stale("cn/DoH 223.5.5.5"):      "healthy",
		partial("cn/DoH 223.6.6.6"):    "warn",
		stale("cn/DoH 1.12.12.12"):     "warn",
		partial("cn/DoH 1.12.12.12"):   "healthy",
		stale("cn/DoH 120.53.53.53"):   "hold",
		partial("cn/DoH 120.53.53.53"): "hold",
		// ir: Shecan, then Electro
		stale("ir/Shecan"):    "warn",
		partial("ir/Shecan"):  "healthy",
		stale("ir/Electro"):   "hold",
		partial("ir/Electro"): "hold",
		// ru
		stale("ru/Yandex.DNS"):                        "warn",
		stale("ru/National DNS (NSDI) 195.208.4.1"):   "healthy",
		partial("ru/National DNS (NSDI) 195.208.4.1"): "healthy",
		stale("ru/National DNS (NSDI) 195.208.5.1"):   "warn",
		// kz
		stale("kz/Cloudflare"): "healthy",
		stale("kz/Quad9"):      "healthy",
	})
	for identity := range states {
		if strings.Contains(identity, "/cn/Alidns") || strings.Contains(identity, "/cn/DNSPod") {
			t.Fatalf("cn's DoH yielded records, yet its plain resolver was judged: %s", identity)
		}
	}
	for _, question := range source.askedSorted() {
		if question == "dns 223.5.5.5:53" || question == "dns 119.29.29.29:53" {
			t.Fatalf("asked %s although cn's DoH yielded records", question)
		}
	}

	byIdentity := map[string]finding{}
	for _, one := range findings {
		byIdentity[one.class+" "+one.target] = one
	}
	for identity, want := range map[string][]string{
		partial("cn/DoH 223.5.5.5"):    {"country=cn", "transport=doh", "kept=3", "authoritative=15", "missing=12", "authority=vantage", "reasons=fewer_than_authoritative", "path_unreachable=DoH 120.53.53.53"},
		stale("cn/DoH 1.12.12.12"):     {"truncated_flag=true", "kept=0", "reasons=truncated_empty"},
		stale("ir/Shecan"):             {"transport=dns", "endpoint=178.22.122.100:53", "valid=0", "expired=15", "reasons=all_expired,newest_too_old", "path_unreachable=Electro"},
		stale("ru/Yandex.DNS"):         {"served=0", "reasons=none_served"},
		partial("default/DoH 1.1.1.1"): {"served=90", "reasons=client_cap"},
	} {
		one, ok := byIdentity[identity]
		if !ok {
			t.Fatalf("no finding %s", identity)
		}
		for _, field := range want {
			if !strings.Contains(one.observed, field) {
				t.Fatalf("%s lost %q: %s", identity, field, one.observed)
			}
		}
		if one.sustain != 2 || one.tier != tierWarn || one.playbook != "SIGNALS.md §28.2" {
			t.Fatalf("%s = sustain %d tier %s playbook %s", identity, one.sustain, one.tier, one.playbook)
		}
	}
	cut := byIdentity[partial("default/DoH 1.1.1.1")]
	if !strings.Contains(cut.symptom, "with an undisclosed connection region") || !strings.Contains(cut.symptom, "DoH 1.1.1.1") {
		t.Fatalf("cut symptom = %s", cut.symptom)
	}
	if kept := cut.observed[strings.Index(cut.observed, " kept=")+6:]; strings.HasPrefix(kept, "90 ") || strings.HasPrefix(kept, "0 ") {
		t.Fatalf("a client's 16 KiB read kept %s", cut.observed)
	}
}

// The alerts carry the country and the resolver's name and no record
// content, and an unreachable resolver raises nothing.
func TestExtenderDnsCountryAlertsNameTheCountryAndResolver(t *testing.T) {
	fresh := extenderDnsCountryTestValues(t, 0, 15, extenderDnsTestNow.Add(-4*time.Minute))
	source := &extenderDnsCountrySource{
		syntheticSource: &syntheticSource{},
		txt: func(resolver string) (DNSResponseObservation, error) {
			if resolver == "78.157.42.100:53" {
				return DNSResponseObservation{}, fmt.Errorf("txt query: %w", syscall.ECONNREFUSED)
			}
			if resolver == "178.22.122.100:53" {
				return extenderDnsTestAnswer(fresh[:5]), nil
			}
			return extenderDnsTestAnswer(fresh), nil
		},
		doh: func(string, int) (int, []byte, error) {
			return 200, extenderDnsCountryDohBody(t, fresh, nil), nil
		},
		authoritative: func(netip.Prefix) (DNSResponseObservation, error) {
			return extenderDnsTestAnswer(fresh), nil
		},
	}
	clearHolds := &signalClearHoldObservation{}
	ctx := context.WithValue(context.Background(), signalClearHoldContextKey{}, clearHolds)
	alerts, err := NewExtenderDnsRecordsSignal().Run(ctx, stormMainSettings(extenderDnsTestNow, source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 {
		t.Fatalf("alerts = %+v, want Shecan's partial set alone", alerts)
	}
	alert := alerts[0]
	if alert.Class != "extender-dns-country-partial" || alert.Target != "main/extender-dns-records/ir/Shecan" ||
		alert.Severity != SeverityWarn || alert.Sustain != 2 || alert.SignalNumber != "28.2" ||
		!strings.Contains(alert.Symptom, "Clients in ir that resolve the extender records through Shecan get a partial set: 5 of 15 records") {
		t.Fatalf("alert = %+v", alert)
	}
	requireAlertOmits(t, alert, "192.0.2.60", "extender-dns-country-key", fresh[0][:16], fresh[1][len(fresh[1])-16:])
	held := map[string]bool{}
	for _, hold := range clearHolds.snapshot(false) {
		held[hold.Class+" "+hold.Target] = true
	}
	for _, identity := range []string{
		"extender-dns-country-stale main/extender-dns-records/ir/Electro",
		"extender-dns-country-partial main/extender-dns-records/ir/Electro",
	} {
		if !held[identity] {
			t.Fatalf("unreachable Electro does not hold %s: %v", identity, held)
		}
	}
}
