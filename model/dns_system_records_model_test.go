package model

import (
	"slices"
	"strings"
	"testing"

	"github.com/miekg/dns"

	"github.com/urnetwork/sidecar/dnsservice"
)

// Each row as owner, type, TTL, kind and its rdata joined, sorted.
func dnsSystemRecordLines(sets []dnsservice.DnsRecordSet) []string {
	lines := []string{}
	for _, set := range sets {
		lines = append(lines, strings.Join([]string{set.Owner, dns.TypeToString[set.Type], string(set.Kind), strings.Join(set.Rdata, " | ")}, " "))
	}
	slices.Sort(lines)
	return lines
}

// The golden sets of §12.7's <net>.<root> at M5: mail off, with and without the edge grant and the apex.
func TestDnsSystemRecordsGoldenSets(t *testing.T) {
	ttl := dnsservice.DefaultDnsSettings().Ttl
	apex := "abcdefghij.zone.example."
	mailOff := []string{
		"*.abcdefghij.zone.example. MX system 0 .",
		"_dmarc.abcdefghij.zone.example. TXT system \"v=DMARC1; p=reject; sp=reject; np=reject; adkim=s; aspf=s\"",
		"abcdefghij.zone.example. MX system 0 .",
		"abcdefghij.zone.example. TXT system \"v=spf1 -all\"",
	}
	cases := []struct {
		name  string
		state DnsZoneState
		want  []string
	}{
		{name: "mail off", state: DnsZoneState{Apex: apex}, want: mailOff},
		{name: "edge on", state: DnsZoneState{Apex: apex, EdgeWildcard: true}, want: append(slices.Clone(mailOff),
			"*.abcdefghij.zone.example. A edge ",
			"*.abcdefghij.zone.example. AAAA edge ",
		)},
		{name: "apex on", state: DnsZoneState{Apex: apex, EdgeApex: true}, want: append(slices.Clone(mailOff),
			"abcdefghij.zone.example. A edge ",
			"abcdefghij.zone.example. AAAA edge ",
		)},
	}
	for _, c := range cases {
		sets := DnsSystemRecords(&c.state, &ttl)
		want := slices.Clone(c.want)
		slices.Sort(want)
		if got := dnsSystemRecordLines(sets); !slices.Equal(got, want) {
			t.Errorf("%s:\n%s\nwant\n%s", c.name, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
		// every golden passes the zone lint and the wire's rules, and holds no user row
		if err := dnsservice.LintZoneWrite("zone.example.", nil, sets); err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
		for index := range sets {
			set := &sets[index]
			if err := set.Validate("zone.example."); err != nil {
				t.Errorf("%s: %v", c.name, err)
			}
			if set.Kind != dnsservice.DnsRecordKindSystem && set.Kind != dnsservice.DnsRecordKindEdge {
				t.Errorf("%s: %+v would count as an entry", c.name, set)
			}
			if set.Type == dns.TypeCNAME {
				t.Errorf("%s: a CNAME %+v", c.name, set)
			}
		}
	}
}

// The same state gives the same rows whatever order its toggles came in.
func TestDnsSystemRecordsRecomputeFromState(t *testing.T) {
	ttl := dnsservice.DefaultDnsSettings().Ttl
	apex := "abcdefghij.zone.example."
	first := DnsSystemRecords(&DnsZoneState{Apex: apex, EdgeWildcard: true, EdgeApex: true}, &ttl)
	state := &DnsZoneState{Apex: apex}
	state.EdgeApex = true
	state.EdgeWildcard = true
	state.EdgeApex = false
	state.EdgeApex = true
	second := DnsSystemRecords(state, &ttl)
	if !slices.Equal(dnsSystemRecordLines(first), dnsSystemRecordLines(second)) {
		t.Fatal("two orders of the same toggles gave two sets")
	}
}

// A dns.yml's root_records for zone.example.
func testDnsRootSettings(t testing.TB, rootRecords string) *dnsservice.DnsSettings {
	t.Helper()
	settings, err := dnsservice.LoadDnsSettings([]byte(`
roots: [zone.example]
signatures_per_second: 1
tcp: {connections_per_source: 1}
rrl: {responses_per_second: 1}
` + rootRecords))
	if err != nil {
		t.Fatal(err)
	}
	return settings
}

// <root>'s set (reading 19) from dns.yml: SOA, NS and glue, CAA with iodef, SPF, the report MX, the apex
// edge, strict DMARC whose reports leave the root, _psl and _spf, all passing the lint.
func TestDnsRootRecordsFromSettings(t *testing.T) {
	settings := testDnsRootSettings(t, `
root_records:
  zone.example:
    caa: ['0 issue "ca.example; accounturi=https://ca.example/acct/1"']
    iodef: mailto:caa@report.example
    report_mx: rpt.report.example
    apex_edge: true
    company_mail_left: true
    psl: psl-proof-token
    glue: {ns1: [192.0.2.53, "2001:db8::53"]}
`)
	sets, err := DnsRootRecords("zone.example.", settings)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"_dmarc.zone.example. TXT system \"v=DMARC1; p=reject; sp=reject; np=reject; adkim=s; aspf=s; rua=mailto:root@rpt.report.example; psd=y\"",
		"_psl.zone.example. TXT system \"psl-proof-token\"",
		"_spf.zone.example. TXT system \"v=spf1 -all\"",
		"ns1.zone.example. A system 192.0.2.53",
		"ns1.zone.example. AAAA system 2001:db8::53",
		"zone.example. A edge ",
		"zone.example. AAAA edge ",
		"zone.example. CAA system 0 issue \"ca.example; accounturi=https://ca.example/acct/1\" | 0 iodef \"mailto:caa@report.example\"",
		"zone.example. MX system 10 rpt.report.example.",
		"zone.example. NS system ns1.zone.example. | ns2.zone.example.",
		"zone.example. SOA system ns1.zone.example. hostmaster.zone.example. 1 7200 3600 1209600 60",
		"zone.example. TXT system \"v=spf1 -all\"",
	}
	slices.Sort(want)
	if got := dnsSystemRecordLines(sets); !slices.Equal(got, want) {
		t.Fatalf("\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if err := dnsservice.LintZoneWrite("zone.example.", nil, sets); err != nil {
		t.Fatal(err)
	}
}

// With no CAA set configured no CA may issue under the root, and with no report host mail to the root is
// refused by a null MX, so an unset value never opens anything.
func TestDnsRootRecordsUnsetValuesClose(t *testing.T) {
	settings := testDnsRootSettings(t, `
root_records:
  zone.example: {}
`)
	sets, err := DnsRootRecords("zone.example.", settings)
	if err != nil {
		t.Fatal(err)
	}
	lines := dnsSystemRecordLines(sets)
	for _, want := range []string{
		"zone.example. CAA system 0 issue \";\" | 0 issuewild \";\"",
		"zone.example. MX system 0 .",
		"_dmarc.zone.example. TXT system \"v=DMARC1; p=reject; sp=reject; np=reject; adkim=s; aspf=s\"",
	} {
		if !slices.Contains(lines, want) {
			t.Errorf("no %s in\n%s", want, strings.Join(lines, "\n"))
		}
	}
	none, err := DnsRootRecords("other.example.", settings)
	if err != nil || none != nil {
		t.Fatalf("a root with no root_records: %v, %v", none, err)
	}
}

// §22.7: no synthesized DMARC carries ruf, fo or a rua into <root>.
func TestDnsSystemDmarcNeverReportsIntoTheRoot(t *testing.T) {
	ttl := dnsservice.DefaultDnsSettings().Ttl
	for _, set := range DnsSystemRecords(&DnsZoneState{Apex: "abcdefghij.zone.example."}, &ttl) {
		if strings.HasPrefix(set.Owner, "_dmarc.") {
			if err := dnsservice.ValidateDmarcRecord("zone.example.", strings.Trim(set.Rdata[0], `"`)); err != nil {
				t.Fatal(err)
			}
		}
	}
	settings := testDnsRootSettings(t, `
root_records:
  zone.example: {report_mx: mail.zone.example}
`)
	if _, err := DnsRootRecords("zone.example.", settings); err == nil {
		t.Fatal("a report host inside the root passed")
	}
}
