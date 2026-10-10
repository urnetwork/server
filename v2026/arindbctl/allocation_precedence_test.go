// Equal-prefix registrations resolve by authoritative network ancestry, not order.
package main

import (
	"context"
	"fmt"
	"net/netip"
	"path/filepath"
	"testing"

	mmdb "github.com/oschwald/maxminddb-golang/v2"
)

// A reassigned complete prefix is simultaneously present under its parent and
// direct owner in bulk Whois. A verified access child overrides hosting flags.
func TestArinBuildEqualPrefixChildOverridesParent(t *testing.T) {
	parent := `<net><handle>TEST-NET-PARENT</handle><orgHandle>TEST-HOSTING</orgHandle><netBlocks><netBlock><type>DA</type><cidrLength>24</cidrLength><startAddress>192.0.2.0</startAddress><endAddress>192.0.2.255</endAddress></netBlock></netBlocks></net>`
	child := `<net><handle>TEST-NET-CHILD</handle><parentNetHandle>TEST-NET-PARENT</parentNetHandle><orgHandle>TEST-ACCESS</orgHandle><netBlocks><netBlock><type>S</type><cidrLength>24</cidrLength><startAddress>192.0.2.0</startAddress><endAddress>192.0.2.255</endAddress></netBlock></netBlocks></net>`
	for _, networks := range []string{parent + child, child + parent} {
		dir := t.TempDir()
		source := writeTestInput(t, filepath.Join(dir, "source.xml"), []byte(`<bulkwhois>
<org><handle>TEST-HOSTING</handle><name>Synthetic Hosting</name><iso3166-1><code2>US</code2></iso3166-1></org>
<org><handle>TEST-ACCESS</handle><name>Synthetic Access</name><iso3166-1><code2>CA</code2></iso3166-1></org>`+networks+`</bulkwhois>`))
		rules := writeTestInput(t, filepath.Join(dir, "rules.yml"), []byte(testArinRules+`  - name: synthetic-direct-access
    org_handles: [TEST-ACCESS]
    non_quality: false
    reason: verified direct access owner
    source: https://evidence.example/direct-access
`))
		geo := writeTestInput(t, filepath.Join(dir, "geolite2.mmdb"), testGeoDatabase(t))
		output := filepath.Join(dir, "release")
		if err := publishDirectory(output, func(stage string) error {
			return buildArinDatabase(context.Background(), source, geo, rules, stage)
		}); err != nil {
			t.Fatalf("a verified equal-prefix child was rejected: %v", err)
		}
		db, err := mmdb.Open(filepath.Join(output, "arin.mmdb"))
		if err != nil {
			t.Fatal(err)
		}
		var record struct {
			Owner      string `maxminddb:"org_handle"`
			Risk       bool   `maxminddb:"risk"`
			NonQuality bool   `maxminddb:"non_quality"`
		}
		err = db.Lookup(netip.MustParseAddr("192.0.2.1")).Decode(&record)
		db.Close()
		if err != nil || record.Owner != "TEST-ACCESS" || !record.Risk || record.NonQuality {
			t.Fatalf("direct network-owner facts were not retained: %+v error=%v", record, err)
		}
	}
}

// Contradictory facts in one network and cyclic ancestry are malformed rather
// than legitimate multiple-owner registrations.
func TestArinAllocationPrecedenceRejectsAmbiguousInputs(t *testing.T) {
	prefix := netip.MustParsePrefix("192.0.2.0/24")
	for _, test := range []struct {
		name        string
		allocations []arinAllocation
		parents     map[string]string
	}{
		{name: "contradictory-same-network-scope", allocations: []arinAllocation{
			{prefix: prefix, organization: "TEST-A", blockType: "DA", network: "TEST-A"},
			{prefix: prefix, organization: "TEST-A", blockType: "RN", network: "TEST-A"},
		}, parents: map[string]string{"TEST-A": ""}},
		{name: "network-cycle", allocations: []arinAllocation{
			{prefix: prefix, organization: "TEST-A", blockType: "S", network: "TEST-A"},
		}, parents: map[string]string{"TEST-A": "TEST-B", "TEST-B": "TEST-A"}},
	} {
		if _, err := selectArinAllocations(t.Context(), test.allocations, test.parents); err == nil {
			t.Errorf("%s: ambiguous source was silently selected", test.name)
		}
	}
}

// A valid grandchild supersedes every known ancestor even when input order is
// reversed; redundant same-owner/type registrations do not alter serving facts.
func TestArinAllocationPrecedenceUsesAncestryAndEquivalentFacts(t *testing.T) {
	prefix := netip.MustParsePrefix("192.0.2.0/24")
	parent := arinAllocation{prefix: prefix, organization: "TEST-PARENT", blockType: "DA", network: "TEST-PARENT"}
	child := arinAllocation{prefix: prefix, organization: "TEST-CHILD", blockType: "A", network: "TEST-CHILD"}
	grandchild := arinAllocation{prefix: prefix, organization: "TEST-DIRECT", blockType: "S", network: "TEST-DIRECT"}
	parents := map[string]string{"TEST-PARENT": "", "TEST-CHILD": "TEST-PARENT", "TEST-DIRECT": "TEST-CHILD"}
	for _, allocations := range [][]arinAllocation{{parent, grandchild, child}, {grandchild, child, parent}} {
		selected, err := selectArinAllocations(t.Context(), allocations, parents)
		if err != nil || len(selected) != 1 || len(selected[0].owners) != 1 || selected[0].owners[0] != grandchild {
			t.Fatalf("direct descendant was not selected: error=%v", err)
		}
	}
	equivalent := grandchild
	equivalent.network = "TEST-EQUIVALENT"
	parents[equivalent.network] = ""
	for _, allocations := range [][]arinAllocation{{grandchild, equivalent}, {equivalent, grandchild}} {
		selected, err := selectArinAllocations(t.Context(), allocations, parents)
		if err != nil || len(selected) != 1 || len(selected[0].owners) != 2 {
			t.Fatalf("equivalent facts changed classification: error=%v", err)
		}
	}
}

// Sibling registrations cannot be ordered by source position, timestamps, or
// handle spelling. Their evidence survives until facts are compared explicitly.
func TestArinAllocationPrecedenceRetainsIncomparableOwners(t *testing.T) {
	prefix := netip.MustParsePrefix("192.0.2.0/24")
	a := arinAllocation{prefix: prefix, organization: "TEST-A", blockType: "S", network: "TEST-A"}
	b := arinAllocation{prefix: prefix, organization: "TEST-B", blockType: "S", network: "TEST-B"}
	for _, input := range [][]arinAllocation{{a, b}, {b, a}} {
		selected, err := selectArinAllocations(t.Context(), input, map[string]string{"TEST-A": "TEST-PARENT", "TEST-B": "TEST-PARENT"})
		if err != nil || len(selected) != 1 || len(selected[0].owners) != 2 || selected[0].owners[0] != a || selected[0].owners[1] != b {
			t.Fatalf("incomparable registration evidence was discarded: error=%v", err)
		}
	}
}

// Unknown ownership never fabricates a geographic or hosting exception. Known
// unanimous facts remain usable, and each conflicting source survives readback.
func TestArinBuildIncomparableOwnersRequireFactConsensus(t *testing.T) {
	for _, test := range []struct {
		name             string
		secondCountry    string
		secondHosting    bool
		wantRisk         bool
		wantNonQuality   bool
		countryAmbiguous bool
		qualityAmbiguous bool
	}{
		{name: "all-facts-agree", secondCountry: "ca", secondHosting: true, wantRisk: true, wantNonQuality: true},
		{name: "country-conflict", secondCountry: "us", secondHosting: true, wantNonQuality: true, countryAmbiguous: true},
		{name: "quality-conflict", secondCountry: "ca", wantRisk: true, qualityAmbiguous: true},
		{name: "both-conflict", secondCountry: "us", countryAmbiguous: true, qualityAmbiguous: true},
		{name: "missing-country", secondHosting: true, wantNonQuality: true, countryAmbiguous: true},
	} {
		dir := t.TempDir()
		source := writeTestInput(t, filepath.Join(dir, "source.xml"), []byte(fmt.Sprintf(`<bulkwhois>
<org><handle>TEST-A</handle><name>Synthetic A</name><iso3166-1><code2>CA</code2></iso3166-1></org>
<org><handle>TEST-B</handle><name>Synthetic B</name><iso3166-1><code2>%s</code2></iso3166-1></org>
<net><handle>TEST-NET-A</handle><parentNetHandle>TEST-SHARED-PARENT</parentNetHandle><orgHandle>TEST-A</orgHandle><netBlocks><netBlock><type>S</type><cidrLength>24</cidrLength><startAddress>192.0.2.0</startAddress><endAddress>192.0.2.255</endAddress></netBlock></netBlocks></net>
<net><handle>TEST-NET-B</handle><parentNetHandle>TEST-SHARED-PARENT</parentNetHandle><orgHandle>TEST-B</orgHandle><netBlocks><netBlock><type>S</type><cidrLength>24</cidrLength><startAddress>192.0.2.0</startAddress><endAddress>192.0.2.255</endAddress></netBlock></netBlocks></net>
</bulkwhois>`, test.secondCountry)))
		rules := writeTestInput(t, filepath.Join(dir, "rules.yml"), []byte(fmt.Sprintf(`version: 1
rules:
  - name: synthetic-first
    org_handles: [TEST-A]
    non_quality: true
    reason: reviewed first owner
    source: https://evidence.example/first
  - name: synthetic-second
    org_handles: [TEST-B]
    non_quality: %t
    reason: reviewed second owner
    source: https://evidence.example/second
`, test.secondHosting)))
		geo := writeTestInput(t, filepath.Join(dir, "geolite2.mmdb"), testGeoDatabase(t))
		output := filepath.Join(dir, "release")
		if err := publishDirectory(output, func(stage string) error { return buildArinDatabase(t.Context(), source, geo, rules, stage) }); err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		db, err := mmdb.Open(filepath.Join(output, "arin.mmdb"))
		if err != nil {
			t.Fatal(err)
		}
		var record struct {
			Owner            string `maxminddb:"org_handle"`
			Risk             bool   `maxminddb:"risk"`
			NonQuality       bool   `maxminddb:"non_quality"`
			CountryAmbiguous bool   `maxminddb:"country_ambiguous"`
			QualityAmbiguous bool   `maxminddb:"non_quality_ambiguous"`
			Owners           []struct {
				Owner string `maxminddb:"org_handle"`
			} `maxminddb:"owner_evidence"`
		}
		err = db.Lookup(netip.MustParseAddr("192.0.2.1")).Decode(&record)
		db.Close()
		if err != nil || record.Owner != "" || len(record.Owners) != 2 || record.Owners[0].Owner != "TEST-A" || record.Owners[1].Owner != "TEST-B" ||
			record.Risk != test.wantRisk || record.NonQuality != test.wantNonQuality || record.CountryAmbiguous != test.countryAmbiguous || record.QualityAmbiguous != test.qualityAmbiguous {
			t.Fatalf("%s: consensus/evidence mismatch: %+v error=%v", test.name, record, err)
		}
	}
}
