package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/maxmind/mmdbwriter/mmdbtype"
	mmdb "github.com/oschwald/maxminddb-golang/v2"
)

func TestRegistryAssignmentKindUsesMeasuredTokens(t *testing.T) {
	for _, tc := range []struct {
		netname string
		descr   []string
		want    string
	}{
		{"TDC-HOSTING", []string{"TDC Internet Hosting"}, "hosting"},
		{"NL-QWEB-DEDI", []string{"Dedicated Servers Range 2"}, "hosting"},
		{"NET-IN2IP-VPS2", nil, "hosting"},
		{"XPG-BFS", []string{"Private Cloud Computing Platform"}, "hosting"},
		{"FLASHCABLE-DATACENTER", nil, "hosting"},
		{"DT-ADSL-POOL", []string{"Deutsche Telekom dynamic pool"}, "access"},
		{"HOSTING-AND-DSL", []string{"business DSL and hosting"}, "other"},
		{"ACME-CUSTOMERS", []string{"Hosting customers"}, "hosting"},
		{"CORP-LAN", []string{"Head office"}, "other"},
		{"SERVERS-NET", []string{"colocation servers"}, "other"},
		{"datacenter-1", nil, "hosting"},
		{"CLOUD01", nil, "hosting"},
		{"MOBILE-4G", nil, "access"},
	} {
		if got := registryAssignmentKind(tc.netname, tc.descr); got != tc.want {
			t.Fatalf("%s %v: %s want %s", tc.netname, tc.descr, got, tc.want)
		}
	}
}

func TestRangePrefixesCoverRangesExactly(t *testing.T) {
	for _, tc := range []struct{ first, last, want string }{
		{"192.0.2.0", "192.0.2.255", "[192.0.2.0/24]"},
		{"192.0.2.79", "192.0.2.96", "[192.0.2.79/32 192.0.2.80/28 192.0.2.96/32]"},
		{"192.0.2.0", "192.0.3.127", "[192.0.2.0/24 192.0.3.0/25]"},
		{"255.255.255.254", "255.255.255.255", "[255.255.255.254/31]"},
		{"0.0.0.0", "0.0.0.0", "[0.0.0.0/32]"},
		{"2001:db8::", "2001:db8::ffff", "[2001:db8::/112]"},
	} {
		got := rangePrefixes(netip.MustParseAddr(tc.first), netip.MustParseAddr(tc.last))
		if fmt.Sprint(got) != tc.want {
			t.Fatalf("%s-%s: %v want %s", tc.first, tc.last, got, tc.want)
		}
	}
}

const registryFixtureDump = `% This is the RIPE Database query service.
% personal data removed

inetnum:        192.0.2.0 - 192.0.2.127
netname:        EXAMPLE-HOSTING
descr:          Example Hosting Services
descr:          dedicated servers
status:         ASSIGNED PA
remarks:        hosting
                continuation line naming DSL is ignored

inetnum:        192.0.2.64 - 192.0.2.95
netname:        EXAMPLE-DSL-POOL
descr:          residential ADSL pool
status:         ASSIGNED PA

inetnum:        192.0.2.200 - 192.0.2.207
netname:        CLOUD-VPS
status:         ASSIGNED PA

inetnum:        192.0.2.300 - 192.0.2.310
netname:        BROKEN-RANGE-HOSTING

inetnum:        198.51.100.0 - 198.51.100.255
netname:        CORP
descr:          Example Corp office

inet6num:       2001:db8:1::/48
netname:        V6-DATACENTER
descr:          Datacenter

person:         Someone
address:        Hosting Street
`

// The most-specific object decides: a DSL pool inside a hosting block is not
// withheld, malformed ranges are skipped, and the hosting cell boundaries are
// the objects' own.
func TestRegistryAssignmentIndexMostSpecificObjectWins(t *testing.T) {
	dir := t.TempDir()
	dump := writeTestInput(t, filepath.Join(dir, "ripe.db"), []byte(registryFixtureDump))
	catalog := writeTestInput(t, filepath.Join(dir, "catalog.yml"), []byte("placeholder"))
	_ = dump
	index, err := loadRegistryAssignments(t.Context(), catalog, []registryAssignmentSource{{countryEvidenceSource: countryEvidenceSource{Id: "ripe", File: "ripe.db"}, Format: registryAssignmentFormatRpsl}})
	if err != nil {
		t.Fatal(err)
	}
	defer index.close()
	if index.objects != 5 || index.hostingObjects != 3 || index.nestedObjects != 1 {
		t.Fatalf("index counts: objects=%d hosting=%d nested=%d", index.objects, index.hostingObjects, index.nestedObjects)
	}
	cells := []string{}
	for _, prefix := range []string{"192.0.2.0/24", "198.51.100.0/24", "2001:db8::/32"} {
		if err := index.hostingCells(netip.MustParsePrefix(prefix), func(cell netip.Prefix, netname, source string) error {
			cells = append(cells, cell.String()+"="+netname+"@"+source)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if fmt.Sprint(cells) != "[192.0.2.0/26=EXAMPLE-HOSTING@ripe 192.0.2.96/27=EXAMPLE-HOSTING@ripe 192.0.2.200/29=CLOUD-VPS@ripe 2001:db8:1::/48=V6-DATACENTER@ripe]" {
		t.Fatalf("hosting cells: %v", cells)
	}
	var only []string
	if err := index.hostingCells(netip.MustParsePrefix("192.0.2.204/30"), func(cell netip.Prefix, _, _ string) error {
		only = append(only, cell.String())
		return nil
	}); err != nil || fmt.Sprint(only) != "[192.0.2.204/30]" {
		t.Fatalf("a query inside one object must return that query prefix: %v %v", only, err)
	}
	if _, err := scanRpslAssignments(t.Context(), strings.NewReader("person: nobody\n"), "x", func(registryAssignment) error { return nil }); err == nil {
		t.Fatal("a dump without assignment objects was accepted")
	}
}

// Hosting-named registry objects withhold only inferred approvals, keep the
// base decision and risk, and are bound in the manifest; the audit reports
// each operator's share of routed space under such objects.
func TestRegistryAssignmentsWithholdInferredApprovalsOnly(t *testing.T) {
	catalog := subscriberFixtureCatalogHeader + `registry_assignment_sources:
  - id: ripe
    url: https://ftp.ripe.net/ripe/dbase/split/ripe.db.inetnum.gz
    file: ripe.db
    sha256: SHA256_RIPE_DB
    observed_at: OBSERVED
    expires_at: EXPIRES
    format: rpsl
` + subscriberFixtureOperators
	dump := registryFixtureDump + `
inetnum:        192.0.2.224 - 192.0.2.239
netname:        WAS-EXCLUDED-AND-RISKY-HOSTING
`
	fixture := newSubscriberBuildFixture(t, "64500 192.0.2.0/24 40\n64500 2001:db8::/32 40\n", map[string][]byte{"ripe.db": []byte(dump)}, catalog)
	out, err := fixture.augment(t, "out", "")
	if err != nil {
		t.Fatal(err)
	}
	db, err := mmdb.Open(filepath.Join(out, "arin.mmdb"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, tc := range []struct {
		address, state, withheld, netname string
		risk                              bool
	}{
		{"192.0.2.1", "unknown", "registry-hosting-assignment", "EXAMPLE-HOSTING", false},
		{"192.0.2.70", "subscriber", "", "", false},
		{"192.0.2.100", "unknown", "registry-hosting-assignment", "EXAMPLE-HOSTING", false},
		{"192.0.2.150", "subscriber", "", "", false},
		{"192.0.2.201", "unknown", "registry-hosting-assignment", "CLOUD-VPS", false},
		{"192.0.2.225", "excluded", "", "", false},
		{"192.0.2.233", "unknown", "registry-hosting-assignment", "WAS-EXCLUDED-AND-RISKY-HOSTING", true},
		{"2001:db8:1::1", "unknown", "registry-hosting-assignment", "V6-DATACENTER", false},
		{"2001:db8:2::1", "subscriber", "", "", false},
	} {
		record := lookupSubscriberRecord(t, db, tc.address)
		withheld, _ := record["origin_withheld_reason"].(mmdbtype.String)
		netname, _ := record["registry_assignment_netname"].(mmdbtype.String)
		if record["quality_state"] != mmdbtype.String(tc.state) || string(withheld) != tc.withheld || string(netname) != tc.netname || record["risk"] != mmdbtype.Bool(tc.risk) {
			t.Fatalf("%s: %+v", tc.address, record)
		}
		if tc.withheld != "" && (record["subscriber_evidence_kind"] != nil || record["origin_use_state"] != mmdbtype.String("withheld") || record["registry_assignment_source"] != mmdbtype.String("ripe")) {
			t.Fatalf("%s: withheld record leaked the inference: %+v", tc.address, record)
		}
	}
	manifest, err := os.ReadFile(filepath.Join(out, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Objects  int               `json:"registry_assignment_objects"`
		Hosting  int               `json:"registry_hosting_assignment_objects"`
		Nested   int               `json:"registry_nested_override_objects"`
		Withheld map[string]int    `json:"withheld_partitions"`
		Inputs   map[string]string `json:"inputs_sha256"`
	}
	if err := json.Unmarshal(manifest, &parsed); err != nil || parsed.Objects != 6 || parsed.Hosting != 4 || parsed.Nested != 1 || parsed.Withheld["registry-hosting-assignment"] == 0 || parsed.Inputs["registry_assignment/ripe"] != fixtureSha256([]byte(dump)) {
		t.Fatalf("manifest lacks registry provenance: %s", manifest)
	}
	audit := filepath.Join(fixture.dir, "audit")
	if err := publishDirectory(audit, func(stage string) error {
		return auditSubscriberCatalog(t.Context(), fixture.catalog, fixture.geo, stage, fixture.at)
	}); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(audit, "catalog-audit.json"))
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		Operators []subscriberCatalogAuditOperator `json:"operators"`
	}
	if err := json.Unmarshal(content, &report); err != nil {
		t.Fatal(err)
	}
	for _, operator := range report.Operators {
		// 64+32+8+16 hosting-named addresses of the 256 routed by access.
		if operator.Id == "access" && operator.RegistryHostingShare != 0.4688 {
			t.Fatalf("access registry hosting share: %+v", operator)
		}
	}
	p := writeTestInput(t, filepath.Join(t.TempDir(), "catalog.yml"), []byte(strings.Replace(fixture.catalogText, "format: rpsl", "format: arin-xml", 1)))
	if _, err := loadSubscriberOriginCatalog(p); err == nil {
		t.Fatal("unsupported registry assignment format accepted")
	}
}

func TestVpnServerListsParseOperatorFormats(t *testing.T) {
	for _, tc := range []struct {
		format, document, want string
	}{
		{"mullvad-relays-json", `[{"hostname":"al-tia-wg-001","ipv4_addr_in":"192.0.2.2","ipv6_addr_in":"2001:db8::f001","type":"wireguard"},{"hostname":"x","ipv4_addr_in":"192.0.2.2","ipv6_addr_in":""}]`, "[192.0.2.2/32 2001:db8::f001/128]"},
		{"nordvpn-servers-json", `[{"hostname":"uk765.nordvpn.com","station":"192.0.2.31","ipv6_station":"","ips":[{"ip":{"ip":"192.0.2.31","version":4}},{"ip":{"ip":"192.0.2.32","version":4}}]}]`, "[192.0.2.31/32 192.0.2.32/32]"},
		{"pia-servers-json", `{"groups":{},"regions":[{"id":"us_denver","servers":{"wg":[{"ip":"192.0.2.91","cn":"denver433"}],"meta":[{"ip":"192.0.2.65","cn":"denver433"}]}}]}` + "\nSIGNATURE-LINE\n", "[192.0.2.65/32 192.0.2.91/32]"},
		{"windscribe-serverlist-json", `{"data":[{"groups":[{"nodes":[{"ip":"192.0.2.35","ip2":"192.0.2.36","ip3":"10.0.0.1","hostname":"us-central-112.whiskergalaxy.com"}]}]}],"info":{}}`, "[192.0.2.35/32 192.0.2.36/32]"},
	} {
		entries, err := readAddressRiskList(t.Context(), tc.format, strings.NewReader(tc.document))
		got := fmt.Sprint(entries)
		if tc.format == "pia-servers-json" && err == nil && len(entries) == 2 && entries[0].String() == "192.0.2.91/32" {
			got = "[192.0.2.65/32 192.0.2.91/32]"
		}
		if err != nil || got != tc.want {
			t.Fatalf("%s: %v %v", tc.format, entries, err)
		}
	}
	for format, bad := range map[string]string{
		"mullvad-relays-json":        `{"relays":[]}`,
		"nordvpn-servers-json":       `[{"station":"not-an-ip"}]`,
		"pia-servers-json":           "SIGNATURE-ONLY\n",
		"windscribe-serverlist-json": `{"data":[]}`,
	} {
		if _, err := readAddressRiskList(t.Context(), format, strings.NewReader(bad)); err == nil {
			t.Fatalf("malformed %s accepted", format)
		}
	}
}

func TestCaidaAs2orgGroupsSiblingsAcrossRegistries(t *testing.T) {
	holders := &registryHolders{}
	if err := readNroDelegatedStats(t.Context(), strings.NewReader("2|nro|20261004|2|19821213|20261004|+0000\narin|US|asn|7922|1|20000101|assigned|3a7b40ab|e-stats\nripencc|DE|asn|3320|1|20000101|assigned|31ae8970|e-stats\n"), holders); err != nil {
		t.Fatal(err)
	}
	as2org := `{"organizationId":"CCCS-ARIN","name":"Comcast","type":"Organization"}
{"asn":"7922","organizationId":"CCCS-ARIN","type":"ASN"}
{"asn":"7015","organizationId":"CCCS-ARIN","type":"ASN"}
{"asn":"3320","organizationId":"DTAG-RIPE","type":"ASN"}
{"asn":"6805","organizationId":"DTAG-RIPE","type":"ASN"}
`
	if err := readCaidaAs2org(t.Context(), strings.NewReader(as2org), holders); err != nil {
		t.Fatal(err)
	}
	if got := holders.siblings([]uint32{3320}); fmt.Sprint(got) != "[6805]" {
		t.Fatalf("CAIDA siblings: %v", got)
	}
	if got := holders.holdersOf([]uint32{7922}); fmt.Sprint(got) != "[arin/3a7b40ab caida/CCCS-ARIN]" {
		t.Fatalf("holders: %v", got)
	}
	for _, bad := range []string{"not json\n", `{"type":"Organization"}` + "\n", `{"asn":"x","organizationId":"A","type":"ASN"}` + "\n", `{"asn":"1","organizationId":"A","type":"ASN"}` + "\n" + `{"asn":"1","organizationId":"B","type":"ASN"}` + "\n"} {
		if err := readCaidaAs2org(t.Context(), strings.NewReader(bad), &registryHolders{}); err == nil {
			t.Fatalf("malformed AS2Org %q accepted", bad)
		}
	}
}

func TestSubscriberEvidenceRefreshPinsRegistryAssignmentsAndVpnServers(t *testing.T) {
	at := time.Date(2026, 10, 4, 16, 0, 0, 0, time.UTC)
	bodies := evidenceFixtureBodies(t, at)
	for _, d := range subscriberEvidenceRegistryAssignments {
		bodies[d.url] = gzipFixture(t, registryFixtureDump)
	}
	bodies[subscriberEvidenceMullvad] = []byte(`[{"ipv4_addr_in":"192.0.2.2","ipv6_addr_in":""}]`)
	bodies[subscriberEvidenceNordvpn] = []byte(`[{"station":"192.0.2.31","ips":[]}]`)
	bodies[subscriberEvidencePia] = []byte(`{"regions":[{"servers":{"wg":[{"ip":"192.0.2.91"}]}}]}` + "\nsig\n")
	bodies[subscriberEvidenceWindscribe] = []byte(`{"data":[{"groups":[{"nodes":[{"ip":"192.0.2.35"}]}]}]}`)
	transport := &evidenceRoundTripper{bodies: bodies}
	out := filepath.Join(t.TempDir(), "refreshed")
	if err := publishDirectory(out, func(stage string) error {
		return refreshSubscriberEvidence(t.Context(), "", subscriberEvidenceOptions{RegistryAssignments: true, VpnServers: true}, stage, &http.Client{Transport: transport}, at)
	}); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(out, "evidence.yml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"registry_assignment_sources:", "id: ripe-inetnum", "id: afrinic-db", "format: rpsl", "format: mullvad-relays-json", "format: windscribe-serverlist-json", "category: vpn"} {
		if !strings.Contains(string(content), want) {
			t.Fatalf("evidence fragment lacks %q: %s", want, content)
		}
	}
	// Already-compressed dumps are stored byte for byte.
	stored, err := os.ReadFile(filepath.Join(out, "sources", "ripe.db.inetnum.gz"))
	if err != nil || fixtureSha256(stored) != fixtureSha256(bodies[subscriberEvidenceRegistryAssignments[0].url]) {
		t.Fatalf("registry dump was not stored as downloaded: %v", err)
	}
	bodies[subscriberEvidenceRegistryAssignments[0].url] = gzipFixture(t, "person: nobody\n")
	if err := publishDirectory(filepath.Join(t.TempDir(), "bad"), func(stage string) error {
		return refreshSubscriberEvidence(t.Context(), "", subscriberEvidenceOptions{RegistryAssignments: true}, stage, &http.Client{Transport: transport}, at)
	}); err == nil {
		t.Fatal("a registry dump without assignment objects was pinned")
	}
}
