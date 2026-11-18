package model

import (
	"errors"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/miekg/dns"

	"github.com/urnetwork/sidecar"
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

// DnsSystemRecords for a state that the test knows is valid.
func testDnsSystemRecords(t testing.TB, state *DnsZoneState, ttl *dnsservice.DnsTtlSettings) []dnsservice.DnsRecordSet {
	t.Helper()
	sets, err := DnsSystemRecords(state, ttl)
	if err != nil {
		t.Fatal(err)
	}
	return sets
}

// §12.7's <net>.<root> with mail off, as M5 has it, for the apex abcdefghij.zone.example.
var dnsTestMailOffLines = []string{
	"*.abcdefghij.zone.example. MX system 0 .",
	"_dmarc.abcdefghij.zone.example. TXT system \"v=DMARC1; p=reject; sp=reject; np=reject; adkim=s; aspf=s\"",
	"abcdefghij.zone.example. MX system 0 .",
	"abcdefghij.zone.example. TXT system \"v=spf1 -all\"",
}

// The same with mail on (M7): MX 10 mx at @ and *, mx's router rows, and b's MX and SPF.
var dnsTestMailOnLines = []string{
	"*.abcdefghij.zone.example. MX system 10 mx.abcdefghij.zone.example.",
	"_dmarc.abcdefghij.zone.example. TXT system \"v=DMARC1; p=reject; sp=reject; np=reject; adkim=s; aspf=s\"",
	"abcdefghij.zone.example. MX system 10 mx.abcdefghij.zone.example.",
	"abcdefghij.zone.example. TXT system \"v=spf1 -all\"",
	"b.abcdefghij.zone.example. MX system 10 mx.abcdefghij.zone.example.",
	"b.abcdefghij.zone.example. TXT system \"v=spf1 include:_spf.zone.example -all\"",
	"mx.abcdefghij.zone.example. A router ",
	"mx.abcdefghij.zone.example. AAAA router ",
}

// The golden sets of §12.7's <net>.<root>: mail off (M5) and on (M7), each with and without the edge
// grant and the apex.
func TestDnsSystemRecordsGoldenSets(t *testing.T) {
	ttl := dnsservice.DefaultDnsSettings().Ttl
	root := "zone.example."
	apex := "abcdefghij.zone.example."
	wildcardEdge := []string{"*.abcdefghij.zone.example. A edge ", "*.abcdefghij.zone.example. AAAA edge "}
	apexEdge := []string{"abcdefghij.zone.example. A edge ", "abcdefghij.zone.example. AAAA edge "}
	cases := []struct {
		name  string
		state DnsZoneState
		want  []string
	}{
		{name: "mail off", state: DnsZoneState{Root: root, Apex: apex}, want: dnsTestMailOffLines},
		{name: "edge on", state: DnsZoneState{Root: root, Apex: apex, EdgeWildcard: true}, want: slices.Concat(dnsTestMailOffLines, wildcardEdge)},
		{name: "apex on", state: DnsZoneState{Root: root, Apex: apex, EdgeApex: true}, want: slices.Concat(dnsTestMailOffLines, apexEdge)},
		{name: "mail on", state: DnsZoneState{Root: root, Apex: apex, Mail: true}, want: dnsTestMailOnLines},
		{
			name:  "mail on, edge and apex on",
			state: DnsZoneState{Root: root, Apex: apex, Mail: true, EdgeWildcard: true, EdgeApex: true},
			want:  slices.Concat(dnsTestMailOnLines, wildcardEdge, apexEdge),
		},
	}
	for _, c := range cases {
		sets := testDnsSystemRecords(t, &c.state, &ttl)
		want := slices.Clone(c.want)
		slices.Sort(want)
		if got := dnsSystemRecordLines(sets); !slices.Equal(got, want) {
			t.Errorf("%s:\n%s\nwant\n%s", c.name, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
		// every golden passes the zone lint and the wire's rules, and holds no row that is an entry
		if err := dnsservice.LintZoneWrite(root, nil, sets); err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
		for index := range sets {
			set := &sets[index]
			if err := set.Validate(root); err != nil {
				t.Errorf("%s: %v", c.name, err)
			}
			switch set.Kind {
			case dnsservice.DnsRecordKindSystem, dnsservice.DnsRecordKindEdge, dnsservice.DnsRecordKindRouter:
			default:
				t.Errorf("%s: %+v would count as an entry", c.name, set)
			}
			if set.Type == dns.TypeCNAME {
				t.Errorf("%s: a CNAME %+v", c.name, set)
			}
		}
	}
}

// Mail on's rows take §12.6's TTLs: mx and mx_host for the MX and the mail host, mail_policy for SPF.
func TestDnsSystemMailRecordsTakeTheirTtls(t *testing.T) {
	ttl := dnsservice.DefaultDnsSettings().Ttl
	ttl.Mx = 301
	ttl.MxHost = 302
	ttl.MailPolicy = 3601
	sets := testDnsSystemRecords(t, &DnsZoneState{Root: "zone.example.", Apex: "abcdefghij.zone.example.", Mail: true}, &ttl)
	want := map[dnsservice.DnsRecordKey]uint32{
		{Owner: "abcdefghij.zone.example.", Type: dns.TypeMX}:      301,
		{Owner: "*.abcdefghij.zone.example.", Type: dns.TypeMX}:    301,
		{Owner: "b.abcdefghij.zone.example.", Type: dns.TypeMX}:    301,
		{Owner: "mx.abcdefghij.zone.example.", Type: dns.TypeA}:    302,
		{Owner: "mx.abcdefghij.zone.example.", Type: dns.TypeAAAA}: 302,
		{Owner: "b.abcdefghij.zone.example.", Type: dns.TypeTXT}:   3601,
	}
	for _, set := range sets {
		if ttl, ok := want[set.Key()]; ok && set.Ttl != ttl {
			t.Errorf("%s %s TTL %d, want %d", set.Owner, dns.TypeToString[set.Type], set.Ttl, ttl)
		}
	}
}

// The same state gives the same rows whatever order its toggles came in, mail's included, and mail off
// after mail on is M5's set again, every mail key managed so a reconcile deletes it.
func TestDnsSystemRecordsRecomputeFromState(t *testing.T) {
	ttl := dnsservice.DefaultDnsSettings().Ttl
	root := "zone.example."
	apex := "abcdefghij.zone.example."
	first := testDnsSystemRecords(t, &DnsZoneState{Root: root, Apex: apex, EdgeWildcard: true, EdgeApex: true, Mail: true}, &ttl)
	state := &DnsZoneState{Root: root, Apex: apex}
	state.EdgeApex = true
	state.Mail = true
	state.EdgeWildcard = true
	state.EdgeApex = false
	state.Mail = false
	state.EdgeApex = true
	state.Mail = true
	second := testDnsSystemRecords(t, state, &ttl)
	if !slices.Equal(dnsSystemRecordLines(first), dnsSystemRecordLines(second)) {
		t.Fatal("two orders of the same toggles gave two sets")
	}
	mailOff := testDnsSystemRecords(t, &DnsZoneState{Root: root, Apex: apex}, &ttl)
	state = &DnsZoneState{Root: root, Apex: apex, Mail: true}
	state.Mail = false
	if !slices.Equal(dnsSystemRecordLines(mailOff), dnsSystemRecordLines(testDnsSystemRecords(t, state, &ttl))) {
		t.Fatal("mail off after mail on is not M5's set")
	}
	managed := dnsZoneSystemKeys(apex)
	for _, set := range first {
		if !slices.Contains(managed, set.Key()) {
			t.Errorf("%s %s is written and not managed, so mail off would leave it", set.Owner, dns.TypeToString[set.Type])
		}
	}
}

// A state whose apex is not a zone below its root has no set, since mail's SPF would name the wrong
// root.
func TestDnsSystemRecordsRefuseAnApexOutsideTheRoot(t *testing.T) {
	ttl := dnsservice.DefaultDnsSettings().Ttl
	for _, state := range []DnsZoneState{
		{Root: "zone.example.", Apex: "abcdefghij.other.example."},
		{Root: "zone.example.", Apex: "zone.example."},
		{Root: "", Apex: "abcdefghij.zone.example.", Mail: true},
	} {
		if _, err := DnsSystemRecords(&state, &ttl); !errors.Is(err, ErrDnsOwnerOutsideZone) {
			t.Errorf("%+v: %v", state, err)
		}
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
	for _, mail := range []bool{false, true} {
		state := &DnsZoneState{Root: "zone.example.", Apex: "abcdefghij.zone.example.", Mail: mail}
		for _, set := range testDnsSystemRecords(t, state, &ttl) {
			if strings.HasPrefix(set.Owner, "_dmarc.") {
				if err := dnsservice.ValidateDmarcRecord("zone.example.", strings.Trim(set.Rdata[0], `"`)); err != nil {
					t.Fatal(err)
				}
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

// A report token and host the M8 tests name: visibly synthetic, the host outside every test root.
const (
	dnsTestReportToken = "abcdefghijklmnopqrstuvwxyz"
	dnsTestReportHost  = "rpt.report.example"
)

func dnsTestMailPolicy() *DnsZoneMailPolicy {
	return &DnsZoneMailPolicy{ReportToken: dnsTestReportToken, ReportHost: dnsTestReportHost, MtaSts: sidecar.DefaultMtaStsSettings()}
}

// M8's golden sets (reading 15): with mail on, _mta-sts names the policy's id and _smtp._tls the network's
// report address, which _dmarc's rua names too; with mail off only _dmarc's rua is added. Each passes the lint.
func TestDnsSystemRecordsMailPolicyGoldenSets(t *testing.T) {
	ttl := dnsservice.DefaultDnsSettings().Ttl
	root := "zone.example."
	apex := "abcdefghij.zone.example."
	id := sidecar.MtaStsPolicyId(sidecar.MtaStsPolicy("mx."+apex, nil), nil)
	rua := "mailto:" + dnsTestReportToken + "@" + dnsTestReportHost
	dmarc := "_dmarc.abcdefghij.zone.example. TXT system \"v=DMARC1; p=reject; sp=reject; np=reject; adkim=s; aspf=s; rua=" + rua + "\""
	replaceDmarc := func(lines []string) []string {
		replaced := []string{}
		for _, line := range lines {
			if strings.HasPrefix(line, "_dmarc.") {
				line = dmarc
			}
			replaced = append(replaced, line)
		}
		return replaced
	}
	mailOn := slices.Concat(replaceDmarc(dnsTestMailOnLines), []string{
		"_mta-sts.abcdefghij.zone.example. TXT system \"v=STSv1; id=" + id + "\"",
		"_smtp._tls.abcdefghij.zone.example. TXT system \"v=TLSRPTv1; rua=" + rua + "\"",
	})
	cases := []struct {
		name  string
		state DnsZoneState
		want  []string
	}{
		{name: "mail off", state: DnsZoneState{Root: root, Apex: apex, MailPolicy: dnsTestMailPolicy()}, want: replaceDmarc(dnsTestMailOffLines)},
		{name: "mail on", state: DnsZoneState{Root: root, Apex: apex, Mail: true, MailPolicy: dnsTestMailPolicy()}, want: mailOn},
		// no token: _dmarc keeps p=reject with no rua, and no _smtp._tls, whose rua RFC 8460 requires
		{
			name:  "mail on, no token",
			state: DnsZoneState{Root: root, Apex: apex, Mail: true, MailPolicy: &DnsZoneMailPolicy{ReportHost: dnsTestReportHost, MtaSts: sidecar.DefaultMtaStsSettings()}},
			want:  slices.Concat(dnsTestMailOnLines, []string{"_mta-sts.abcdefghij.zone.example. TXT system \"v=STSv1; id=" + id + "\""}),
		},
	}
	for _, c := range cases {
		sets := testDnsSystemRecords(t, &c.state, &ttl)
		want := slices.Clone(c.want)
		slices.Sort(want)
		if got := dnsSystemRecordLines(sets); !slices.Equal(got, want) {
			t.Errorf("%s:\n%s\nwant\n%s", c.name, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
		if err := dnsservice.LintZoneWrite(root, nil, sets); err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
		for _, set := range sets {
			if strings.HasPrefix(set.Owner, "_mta-sts.") || strings.HasPrefix(set.Owner, "_smtp._tls.") {
				if set.Ttl != ttl.MailPolicy {
					t.Errorf("%s: %s TTL %d", c.name, set.Owner, set.Ttl)
				}
			}
		}
	}
	// mail off's keys hold the policy records, so turning mail off deletes them
	managed := dnsZoneSystemKeys(apex)
	for _, set := range testDnsSystemRecords(t, &cases[1].state, &ttl) {
		if !slices.Contains(managed, set.Key()) {
			t.Errorf("%s %s is written and not managed", set.Owner, dns.TypeToString[set.Type])
		}
	}
}

// _mta-sts's id changes when the policy does, its mode, its max_age or its mail host, and stays when it does
// not; a scope's policy is its own mail host's.
func TestDnsMtaStsIdChangesWithThePolicy(t *testing.T) {
	ttl := dnsservice.DefaultDnsSettings().Ttl
	root := "zone.example."
	apex := "abcdefghij.zone.example."
	idOf := func(state *DnsZoneState) string {
		for _, set := range testDnsSystemRecords(t, state, &ttl) {
			if set.Owner == "_mta-sts."+state.Apex {
				return set.Rdata[0]
			}
		}
		t.Fatalf("no _mta-sts at %s", state.Apex)
		return ""
	}
	base := idOf(&DnsZoneState{Root: root, Apex: apex, Mail: true, MailPolicy: dnsTestMailPolicy()})
	if again := idOf(&DnsZoneState{Root: root, Apex: apex, Mail: true, EdgeWildcard: true, MailPolicy: dnsTestMailPolicy()}); again != base {
		t.Errorf("an unrelated toggle moved the id: %s %s", base, again)
	}
	longer := dnsTestMailPolicy()
	longer.MtaSts.MaxAgeSeconds += 1
	testing := dnsTestMailPolicy()
	testing.MtaSts.Mode = "testing"
	for name, policy := range map[string]*DnsZoneMailPolicy{"max_age": longer, "mode": testing} {
		if moved := idOf(&DnsZoneState{Root: root, Apex: apex, Mail: true, MailPolicy: policy}); moved == base {
			t.Errorf("%s changed and the id did not", name)
		}
	}
	scopeId := idOf(&DnsZoneState{Root: root, Apex: "eng." + apex, Mail: true, MailPolicy: dnsTestMailPolicy()})
	if scopeId == base {
		t.Error("a scope's policy names the network's mail host")
	}
	// the id is the receivers' policy's, which names the zone's mail host
	want := `"v=STSv1; id=` + sidecar.MtaStsPolicyId(sidecar.MtaStsPolicy("mx."+apex, nil), nil) + `"`
	if base != want {
		t.Errorf("id %s, want %s", base, want)
	}
}

// A report address in the root is refused: as the set's report host, which would send a zone's reports to a
// user's name, and in a row the lint judges.
func TestDnsSystemRecordsRefuseReportsIntoTheRoot(t *testing.T) {
	ttl := dnsservice.DefaultDnsSettings().Ttl
	policy := dnsTestMailPolicy()
	policy.ReportHost = "rpt.zone.example"
	if _, err := DnsSystemRecords(&DnsZoneState{Root: "zone.example.", Apex: "abcdefghij.zone.example.", Mail: true, MailPolicy: policy}, &ttl); !errors.Is(err, dnsservice.ErrDnsDmarcRefused) && !errors.Is(err, dnsservice.ErrDnsTlsRptRefused) {
		t.Errorf("a report host in the root: %v", err)
	}
	row := dnsSystemTxt("_smtp._tls.abcdefghij.zone.example.", "v=TLSRPTv1; rua=mailto:x@abc.zone.example", ttl.MailPolicy)
	if err := dnsservice.LintZoneWrite("zone.example.", nil, []dnsservice.DnsRecordSet{row}); !errors.Is(err, dnsservice.ErrDnsZoneLint) {
		t.Errorf("a TLS-RPT row into the root: %v", err)
	}
	for _, text := range []string{"v=TLSRPTv1", "v=TLSRPTv1; rua=https://report.example/x", "v=TLSRPTv2; rua=mailto:x@report.example"} {
		if err := dnsservice.ValidateTlsRptRecord("zone.example.", text); !errors.Is(err, dnsservice.ErrDnsTlsRptRefused) {
			t.Errorf("%q: %v", text, err)
		}
	}
	if err := dnsservice.ValidateTlsRptRecord("zone.example.", "v=TLSRPTv1; rua=mailto:"+dnsTestReportToken+"@"+dnsTestReportHost); err != nil {
		t.Error(err)
	}
}

// A Team scope's set is the network's function at the scope's apex, for every state, its mail set included.
func TestDnsScopeRecordsAreTheNetworksFunction(t *testing.T) {
	ttl := dnsservice.DefaultDnsSettings().Ttl
	root := "zone.example."
	apex := "abcdefghij.zone.example."
	for _, flags := range [][3]bool{{false, false, false}, {true, false, false}, {false, true, false}, {false, false, true}, {true, true, true}} {
		scope := &DnsScopeState{Root: root, Apex: apex, Scope: "eng", EdgeWildcard: flags[0], EdgeApex: flags[1], Mail: flags[2], MailPolicy: dnsTestMailPolicy()}
		got, err := DnsScopeRecords(scope, &ttl)
		if err != nil {
			t.Fatal(err)
		}
		want := testDnsSystemRecords(t, &DnsZoneState{Root: root, Apex: "eng." + apex, EdgeWildcard: flags[0], EdgeApex: flags[1], Mail: flags[2], MailPolicy: dnsTestMailPolicy()}, &ttl)
		if !slices.Equal(dnsSystemRecordLines(got), dnsSystemRecordLines(want)) {
			t.Errorf("%v: the scope's set is not the network's at its apex", flags)
		}
		if flags[2] && !slices.Contains(dnsSystemRecordLines(got), "mx.eng.abcdefghij.zone.example. A router ") {
			t.Errorf("%v: no mx.<scope> host", flags)
		}
		if err := dnsservice.LintZoneWrite(root, nil, got); err != nil {
			t.Errorf("%v: %v", flags, err)
		}
	}
	if _, err := DnsScopeRecords(&DnsScopeState{Root: root, Apex: apex, Scope: "a.b"}, &ttl); !errors.Is(err, dnsservice.ErrDnsScopeLabelRefused) {
		t.Errorf("a scope of two labels: %v", err)
	}
}

// _spf.<root> lists the sending addresses the host sets beside dns.yml (DNS-M9-02), in address order, and
// sends from nowhere with none; the SOA takes dns.yml's timers.
func TestDnsRootSpfFollowsTheSendingAddresses(t *testing.T) {
	settings := testDnsRootSettings(t, `
root_records:
  zone.example: {}
soa: {refresh_s: 7201, retry_s: 3601, expire_s: 1209601}
`)
	spfLine := func() string {
		t.Helper()
		sets, err := DnsRootRecords("zone.example.", settings)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range dnsSystemRecordLines(sets) {
			if strings.HasPrefix(line, "zone.example. SOA ") && !strings.HasSuffix(line, " 1 7201 3601 1209601 60") {
				t.Errorf("soa %s", line)
			}
			if strings.HasPrefix(line, "_spf.") {
				return line
			}
		}
		t.Fatal("no _spf")
		return ""
	}
	if got := spfLine(); got != `_spf.zone.example. TXT system "v=spf1 -all"` {
		t.Errorf("none: %s", got)
	}
	if err := settings.SetSendingAddresses([]dnsservice.DnsSendingAddress{
		{Address: netip.MustParseAddr("192.0.2.11"), Name: "o2.out.report.example"},
		{Address: netip.MustParseAddr("192.0.2.10"), Name: "o1.out.report.example"},
	}); err != nil {
		t.Fatal(err)
	}
	if got := spfLine(); got != `_spf.zone.example. TXT system "v=spf1 ip4:192.0.2.10 ip4:192.0.2.11 -all"` {
		t.Errorf("two: %s", got)
	}
}
