package main

import (
	"fmt"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"

	mmdb "github.com/oschwald/maxminddb-golang/v2"
)

const testAllocationScopeRules = `version: 1
quality_policy_version: 2
rules:
  - name: reviewed-subscriber-pool
    allocation_scopes:
      - {net_handle: TEST-POOL, org_handle: TEST-MIXED, prefix: 192.0.2.0/24}
      - {net_handle: TEST-REVIEWED-CHILD, org_handle: TEST-CHILD, prefix: 192.0.2.160/27}
      - {net_handle: TEST-CONFLICT, org_handle: TEST-MIXED, prefix: 192.0.2.192/26}
    non_quality: false
    reason: reviewed operator publication identifies this exact subscriber pool
    source: https://evidence.example/subscriber-pools
  - name: reviewed-proxy
    org_handles: [TEST-PROXY]
    non_quality: true
    risk_category: virtual_isp
    reason: reviewed proxy service
    source: https://evidence.example/proxy
`

// A real mixed-use ISP has subscriber pools and infrastructure. Reproduces the
// missing affirmative supply without turning the owner's other networks, new
// reassignments or an equally specific conflicting registration into access.
func TestSubscriberAllocationScopesBuildPreservesDelegationAndRisk(t *testing.T) {
	dir := t.TempDir()
	org := func(handle, parent string) string {
		return fmt.Sprintf(`<org><handle>%s</handle><name>Synthetic Operator</name><parentOrgHandle>%s</parentOrgHandle><iso3166-1><code2>US</code2></iso3166-1></org>`, handle, parent)
	}
	net := func(handle, owner, parent, address string, bits int) string {
		return fmt.Sprintf(`<net><handle>%s</handle><orgHandle>%s</orgHandle><parentNetHandle>%s</parentNetHandle><netBlocks><netBlock><type>S</type><cidrLength>%d</cidrLength><startAddress>%s</startAddress></netBlock></netBlocks></net>`, handle, owner, parent, bits, address)
	}
	xml := "<bulkwhois>" + org("TEST-MIXED", "") + org("TEST-UNKNOWN", "") + org("TEST-PROXY", "") + org("TEST-CHILD", "TEST-PROXY") +
		net("TEST-POOL", "TEST-MIXED", "", "192.0.2.0", 24) +
		net("TEST-UNKNOWN-CHILD", "TEST-UNKNOWN", "TEST-POOL", "192.0.2.64", 27) +
		net("TEST-SAME-OWNER-CHILD", "TEST-MIXED", "TEST-POOL", "192.0.2.96", 27) +
		net("TEST-PROXY-CHILD", "TEST-PROXY", "TEST-POOL", "192.0.2.128", 27) +
		net("TEST-REVIEWED-CHILD", "TEST-CHILD", "TEST-POOL", "192.0.2.160", 27) +
		net("TEST-CONFLICT", "TEST-MIXED", "TEST-POOL", "192.0.2.192", 26) +
		net("TEST-INCOMPARABLE", "TEST-UNKNOWN", "TEST-POOL", "192.0.2.192", 26) +
		net("TEST-OTHER-USE", "TEST-MIXED", "", "198.51.100.0", 24) + "</bulkwhois>"
	source := writeTestInput(t, filepath.Join(dir, "source.xml"), []byte(xml))
	geo := writeTestInput(t, filepath.Join(dir, "geo.mmdb"), testGeoDatabase(t))
	for _, scoped := range []bool{false, true} {
		rules := testAllocationScopeRules
		if !scoped {
			// Original policy has no positive use evidence for this mixed owner.
			rules = testSubscriberRules
		}
		rulesPath := writeTestInput(t, filepath.Join(dir, fmt.Sprintf("rules-%t.yml", scoped)), []byte(rules))
		output := filepath.Join(dir, fmt.Sprintf("out-%t", scoped))
		if err := publishDirectory(output, func(stage string) error { return buildArinDatabase(t.Context(), source, geo, rulesPath, stage) }); err != nil {
			t.Fatal(err)
		}
		db, err := mmdb.Open(filepath.Join(output, "arin.mmdb"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		for n := 0; n < 256; n++ {
			var record struct {
				State             string `maxminddb:"quality_state"`
				NonQuality        bool   `maxminddb:"non_quality"`
				Risk              bool   `maxminddb:"risk"`
				GeographicRisk    bool   `maxminddb:"geographic_risk"`
				NetworkRisk       bool   `maxminddb:"network_risk"`
				ClassifiedNetwork string `maxminddb:"classification_network_handle"`
			}
			if err := db.Lookup(netip.MustParseAddr(fmt.Sprintf("192.0.2.%d", n))).Decode(&record); err != nil {
				t.Fatal(err)
			}
			state := "unknown"
			if 128 <= n && n < 192 {
				state = "excluded"
			}
			if scoped {
				if n < 64 || 160 <= n && n < 192 {
					state = "subscriber"
				}
				if n >= 192 {
					state = "ambiguous"
				}
			}
			if record.State != state || record.NonQuality != (state != "subscriber") || record.GeographicRisk != (n >= 128) || record.NetworkRisk != (128 <= n && n < 192) || record.Risk != (n >= 128) {
				t.Fatalf("scoped=%t offset=%d: record=%+v want state=%s with unchanged risk", scoped, n, record, state)
			}
			if scoped && n < 64 && record.ClassifiedNetwork != "TEST-POOL" {
				t.Fatal("subscriber approval lost its exact network provenance")
			}
		}
		var outside struct {
			State string `maxminddb:"quality_state"`
		}
		if err := db.Lookup(netip.MustParseAddr("198.51.100.1")).Decode(&outside); err != nil || outside.State != "unknown" {
			t.Fatal("subscriber pool approved the owner's unrelated allocation")
		}
	}
}

func TestSubscriberAllocationScopesRejectInvalidOrChangedAuthority(t *testing.T) {
	base := `version: 1
quality_policy_version: 2
rules:
  - name: access
    allocation_scopes:
      - {net_handle: TEST-POOL, org_handle: TEST-MIXED, prefix: 192.0.2.0/24}
    non_quality: false
    reason: reviewed service pool
    source: https://evidence.example/subscribers
`
	for _, invalid := range []string{
		strings.Replace(base, "quality_policy_version: 2", "quality_policy_version: 0", 1),
		strings.Replace(base, "non_quality: false", "non_quality: true", 1),
		strings.Replace(base, "non_quality: false", "non_quality: false\n    org_handles: [TEST-MIXED]", 1),
		strings.Replace(base, "non_quality: false", "non_quality: false\n    prefixes: [192.0.2.0/24]", 1),
		strings.Replace(base, "non_quality: false", "non_quality: false\n    org_name_pattern: '^Synthetic$'", 1),
		strings.Replace(base, "TEST-POOL", "''", 1),
		strings.Replace(base, "192.0.2.0/24", "192.0.2.1/24", 1),
		strings.Replace(base, "192.0.2.0/24", "::ffff:192.0.2.0/120", 1),
	} {
		path := writeTestInput(t, filepath.Join(t.TempDir(), "rules.yml"), []byte(invalid))
		if _, err := loadClassificationRules(path); err == nil {
			t.Fatal("invalid subscriber scope was accepted")
		}
	}
	path := writeTestInput(t, filepath.Join(t.TempDir(), "rules.yml"), []byte(base))
	rules, err := loadClassificationRules(path)
	if err != nil {
		t.Fatal(err)
	}
	allocation := arinAllocation{network: "TEST-POOL", organization: "TEST-MIXED", prefix: netip.MustParsePrefix("192.0.2.0/24"), blockType: "S"}
	if err := rules.validateAllocationScopes([]arinAllocation{allocation}); err != nil {
		t.Fatal(err)
	}
	for _, changed := range []arinAllocation{
		{network: "TEST-CHILD", organization: allocation.organization, prefix: allocation.prefix, blockType: "S"},
		{network: allocation.network, organization: "TEST-TRANSFER", prefix: allocation.prefix, blockType: "S"},
		{network: allocation.network, organization: allocation.organization, prefix: netip.MustParsePrefix("192.0.2.0/25"), blockType: "S"},
		{network: allocation.network, organization: allocation.organization, prefix: allocation.prefix, blockType: "AP"},
		{network: allocation.network, organization: allocation.organization, prefix: allocation.prefix, blockType: "AR"},
		{network: allocation.network, organization: allocation.organization, prefix: allocation.prefix},
	} {
		if err := rules.validateAllocationScopes([]arinAllocation{changed}); err == nil {
			t.Fatal("changed or nonauthoritative scope remained publishable")
		}
	}
	if err := rules.validateAllocationScopes(nil); err == nil {
		t.Fatal("absent allocation remained publishable")
	}
	prior := arinClassification{nonQuality: true, qualityState: "excluded", ruleName: "direct-proxy", source: "https://evidence.example/proxy", orgHandle: allocation.organization}
	if got := rules.classifyAllocationScope(allocation, prior); got.qualityState != "ambiguous" || !got.nonQuality || !strings.Contains(got.source, prior.source) {
		t.Fatal("scoped approval cleared a conflicting direct-use exclusion")
	}
}
