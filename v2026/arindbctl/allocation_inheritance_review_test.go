package main

import (
	"context"
	"fmt"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"

	mmdb "github.com/oschwald/maxminddb-golang/v2"
)

// Source-level control for the explicitly requested negative inheritance.
// The direct network child is independently registered: no parentOrgHandle
// makes the existing organization-only test accidentally satisfy this case.
func TestArinIndependentNetworkChildRetainsReviewedHostingParent(t *testing.T) {
	for _, policy := range []int{0, 2} {
		t.Run(fmt.Sprintf("policy_%d", policy), func(t *testing.T) {
			dir := t.TempDir()
			source := writeTestInput(t, filepath.Join(dir, "source.xml"), []byte(`<bulkwhois>
<org><handle>TEST-HOSTING</handle><name>Synthetic Hosting</name><iso3166-1><code2>US</code2></iso3166-1></org>
<org><handle>TEST-INDEPENDENT</handle><name>Synthetic Unreviewed Child</name><iso3166-1><code2>US</code2></iso3166-1></org>
<net><handle>TEST-NET-PARENT</handle><orgHandle>TEST-HOSTING</orgHandle><netBlocks><netBlock><type>DA</type><cidrLength>24</cidrLength><startAddress>192.0.2.0</startAddress><endAddress>192.0.2.255</endAddress></netBlock></netBlocks></net>
<net><handle>TEST-NET-CHILD</handle><parentNetHandle>TEST-NET-PARENT</parentNetHandle><orgHandle>TEST-INDEPENDENT</orgHandle><netBlocks><netBlock><type>S</type><cidrLength>25</cidrLength><startAddress>192.0.2.0</startAddress><endAddress>192.0.2.127</endAddress></netBlock></netBlocks></net>
</bulkwhois>`))
			rules := writeTestInput(t, filepath.Join(dir, "rules.yml"), []byte(fmt.Sprintf(`version: 1
quality_policy_version: %d
rules:
  - name: reviewed-hosting-parent
    org_handles: [TEST-HOSTING]
    non_quality: true
    reason: reviewed synthetic hosting use
    source: https://evidence.example/hosting
`, policy)))
			geo := writeTestInput(t, filepath.Join(dir, "geolite2.mmdb"), testGeoDatabase(t))
			output := filepath.Join(dir, "out")
			if err := publishDirectory(output, func(stage string) error { return buildArinDatabase(context.Background(), source, geo, rules, stage) }); err != nil {
				t.Fatal(err)
			}
			db, err := mmdb.Open(filepath.Join(output, "arin.mmdb"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var record struct {
				Owner             string `maxminddb:"org_handle"`
				Network           string `maxminddb:"net_handle"`
				ClassifiedOwner   string `maxminddb:"classification_org_handle"`
				ClassifiedNetwork string `maxminddb:"classification_network_handle"`
				Rule              string `maxminddb:"classification_rule"`
				State             string `maxminddb:"quality_state"`
				NonQuality        bool   `maxminddb:"non_quality"`
			}
			if err = db.Lookup(netip.MustParseAddr("192.0.2.1")).Decode(&record); err != nil {
				t.Fatal(err)
			}
			if record.Owner != "TEST-INDEPENDENT" || record.Network != "TEST-NET-CHILD" || !record.NonQuality || record.ClassifiedOwner != "TEST-HOSTING" || record.ClassifiedNetwork != "TEST-NET-PARENT" || record.Rule != "reviewed-hosting-parent" || policy == 2 && record.State != "excluded" {
				t.Fatalf("independent child lost reviewed hosting-parent provenance: %+v", record)
			}
		})
	}
}

// Full MMDB controls preserve the source owner, direct-owner geographic risk,
// independent proxy evidence and every reviewed child/prefix exception.
func TestArinAllocationParentQualityBoundaries(t *testing.T) {
	org := func(handle, country, parent string) string {
		return fmt.Sprintf(`<org><handle>%s</handle><name>%s</name><iso3166-1><code2>%s</code2></iso3166-1><parentOrgHandle>%s</parentOrgHandle></org>`, handle, handle, country, parent)
	}
	network := func(handle, owner, parent, kind, start, end string, bits int) string {
		return fmt.Sprintf(`<net><handle>%s</handle><parentNetHandle>%s</parentNetHandle><orgHandle>%s</orgHandle><netBlocks><netBlock><type>%s</type><cidrLength>%d</cidrLength><startAddress>%s</startAddress><endAddress>%s</endAddress></netBlock></netBlocks></net>`, handle, parent, owner, kind, bits, start, end)
	}
	rule := func(name, selector string, excluded bool, risk string) string {
		s := fmt.Sprintf("  - name: %s\n    %s\n    non_quality: %t\n    reason: reviewed synthetic use\n    source: https://evidence.example/%s\n", name, selector, excluded, name)
		if risk != "" {
			s += "    risk_category: " + risk + "\n"
		}
		return s
	}
	base := []string{org("HOST", "CA", ""), org("CHILD", "US", ""), org("MIDDLE", "US", ""), org("ACCESS", "US", ""),
		network("NET-HOST", "HOST", "", "DA", "192.0.2.0", "192.0.2.255", 24),
		network("NET-CHILD", "CHILD", "NET-HOST", "S", "192.0.2.0", "192.0.2.127", 25)}
	baseRules := rule("host", "org_handles: [HOST]", true, "proxy")
	type expectation struct {
		state, classifiedOwner, classifiedNet, rule string
		risk                                        bool
	}
	for _, tc := range []struct {
		name           string
		change         func([]string) []string
		rules, address string
		want           expectation
	}{
		{"independent_quality_only", nil, "", "", expectation{"excluded", "HOST", "NET-HOST", "host", false}},
		{"equal_prefix_direct_child", func(s []string) []string {
			s[5] = network("NET-CHILD", "CHILD", "NET-HOST", "S", "192.0.2.0", "192.0.2.255", 24)
			return s
		}, "", "", expectation{"excluded", "HOST", "NET-HOST", "host", false}},
		{"direct_access_wins", nil, rule("access", "org_handles: [CHILD]", false, ""), "", expectation{"subscriber", "CHILD", "", "access", false}},
		{"direct_exclusion_wins", nil, rule("child-host", "org_handles: [CHILD]", true, ""), "", expectation{"excluded", "CHILD", "", "child-host", false}},
		{"prefix_access_wins", nil, rule("prefix-access", "prefixes: [192.0.2.64/26]", false, ""), "192.0.2.65", expectation{"subscriber", "", "", "prefix-access", false}},
		{"prefix_access_does_not_clear_neighbor", nil, rule("prefix-access", "prefixes: [192.0.2.64/26]", false, ""), "192.0.2.1", expectation{"excluded", "HOST", "NET-HOST", "host", false}},
		{"independent_prefix_risk_retained", nil, rule("exact-proxy", "prefixes: [192.0.2.0/26]", true, "proxy"), "", expectation{"excluded", "", "", "exact-proxy", true}},
		{"missing_parent_unknown", func(s []string) []string { s[5] = strings.Replace(s[5], "NET-HOST", "NET-MISSING", 1); return s }, "", "", expectation{"unknown", "", "", "", false}},
		{"noncontaining_parent_unknown", func(s []string) []string {
			s[4] = network("NET-HOST", "HOST", "", "DA", "192.0.2.128", "192.0.2.255", 25)
			return s
		}, "", "", expectation{"unknown", "", "", "", false}},
		{"referral_parent_unknown", func(s []string) []string {
			s[4] = strings.Replace(s[4], "<type>DA</type>", "<type>AP</type>", 1)
			return s
		}, "", "", expectation{"unknown", "", "", "", false}},
		{"referral_child_unknown", func(s []string) []string {
			s[5] = strings.Replace(s[5], "<type>S</type>", "<type>AP</type>", 1)
			return s
		}, "", "", expectation{"unknown", "", "", "", false}},
		{"registry_parent_unknown", func(s []string) []string {
			s[4] = strings.Replace(s[4], "<type>DA</type>", "<type>AR</type>", 1)
			return s
		}, "", "", expectation{"unknown", "", "", "", false}},
		{"grandparent_quality", func(s []string) []string {
			s[5] = strings.Replace(s[5], "NET-HOST", "NET-MIDDLE", 1)
			return append(s, network("NET-MIDDLE", "MIDDLE", "NET-HOST", "DA", "192.0.2.0", "192.0.2.127", 25))
		}, "", "", expectation{"excluded", "HOST", "NET-HOST", "host", false}},
		{"intermediate_referral_stops_chain", func(s []string) []string {
			s[5] = strings.Replace(s[5], "NET-HOST", "NET-MIDDLE", 1)
			return append(s, network("NET-MIDDLE", "MIDDLE", "NET-HOST", "AP", "192.0.2.0", "192.0.2.127", 25))
		}, "", "", expectation{"unknown", "", "", "", false}},
		{"reviewed_access_parent_stops_chain", func(s []string) []string {
			s[5] = strings.Replace(s[5], "NET-HOST", "NET-MIDDLE", 1)
			return append(s, network("NET-MIDDLE", "MIDDLE", "NET-HOST", "DA", "192.0.2.0", "192.0.2.127", 25))
		}, rule("middle-access", "org_handles: [MIDDLE]", false, ""), "", expectation{"unknown", "", "", "", false}},
		{"reviewed_access_org_ancestor_stops_chain", func(s []string) []string { s[1] = org("CHILD", "US", "ACCESS"); return s }, rule("access-org", "org_handles: [ACCESS]", false, ""), "", expectation{"unknown", "", "", "", false}},
		{"reviewed_access_network_never_allows_unknown_child", func(s []string) []string {
			s[4] = strings.Replace(s[4], "<orgHandle>HOST</orgHandle>", "<orgHandle>ACCESS</orgHandle>", 1)
			return s
		}, rule("access-org", "org_handles: [ACCESS]", false, ""), "", expectation{"unknown", "", "", "", false}},
		{"direct_child_country_mismatch_retained", func(s []string) []string { s[1] = org("CHILD", "CA", ""); return s }, "", "", expectation{"excluded", "HOST", "NET-HOST", "host", true}},
		{"source_order_independent", func(s []string) []string {
			for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
				s[i], s[j] = s[j], s[i]
			}
			return s
		}, "", "", expectation{"excluded", "HOST", "NET-HOST", "host", false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			items := append([]string(nil), base...)
			if tc.change != nil {
				items = tc.change(items)
			}
			dir := t.TempDir()
			source := writeTestInput(t, filepath.Join(dir, "source.xml"), []byte("<bulkwhois>"+strings.Join(items, "\n")+"</bulkwhois>"))
			rules := writeTestInput(t, filepath.Join(dir, "rules.yml"), []byte("version: 1\nquality_policy_version: 2\nrules:\n"+baseRules+tc.rules))
			geo := writeTestInput(t, filepath.Join(dir, "geo.mmdb"), testGeoDatabase(t))
			out := filepath.Join(dir, "out")
			if err := publishDirectory(out, func(stage string) error { return buildArinDatabase(context.Background(), source, geo, rules, stage) }); err != nil {
				t.Fatal(err)
			}
			db, err := mmdb.Open(filepath.Join(out, "arin.mmdb"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var got struct {
				Owner             string `maxminddb:"org_handle"`
				State             string `maxminddb:"quality_state"`
				ClassifiedOwner   string `maxminddb:"classification_org_handle"`
				ClassifiedNetwork string `maxminddb:"classification_network_handle"`
				Rule              string `maxminddb:"classification_rule"`
				Risk              bool   `maxminddb:"risk"`
				NonQuality        bool   `maxminddb:"non_quality"`
			}
			address := tc.address
			if address == "" {
				address = "192.0.2.1"
			}
			if err := db.Lookup(netip.MustParseAddr(address)).Decode(&got); err != nil {
				t.Fatal(err)
			}
			want := tc.want
			if got.Owner != "CHILD" || got.State != want.state || got.ClassifiedOwner != want.classifiedOwner || got.ClassifiedNetwork != want.classifiedNet || got.Rule != want.rule || got.Risk != want.risk || got.NonQuality != (want.state != "subscriber") {
				t.Fatalf("allocation boundary mismatch: got %+v; want %+v", got, want)
			}
		})
	}
}
