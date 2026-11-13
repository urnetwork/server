package main

import (
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mmdb "github.com/oschwald/maxminddb-golang/v2"
)

const testSubscriberRules = `version: 1
quality_policy_version: 2
rules:
  - name: synthetic-hosting
    org_handles: [TEST-HOSTING]
    non_quality: true
    reason: reviewed hosting infrastructure
    source: https://evidence.example/hosting
  - name: synthetic-subscriber
    org_handles: [TEST-ACCESS]
    non_quality: false
    reason: reviewed subscriber-access network
    source: https://evidence.example/access
  - name: synthetic-virtual-isp
    org_handles: [TEST-PROXY]
    non_quality: true
    risk_category: virtual_isp
    reason: reviewed ISP-branded proxy network
    source: https://evidence.example/proxy
`

func TestSubscriberPolicyRequiresPositiveDirectEvidence(t *testing.T) {
	path := writeTestInput(t, filepath.Join(t.TempDir(), "rules.yml"), []byte(testSubscriberRules))
	rules, err := loadClassificationRules(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		handles []string
		state   string
		risk    bool
	}{
		{handles: []string{"TEST-UNREVIEWED"}, state: "unknown"},
		{handles: []string{"TEST-ACCESS"}, state: "subscriber"},
		{handles: []string{"TEST-ACCESS", "TEST-UNREVIEWED"}, state: "unknown"},
		{handles: []string{"TEST-HOSTING", "TEST-UNREVIEWED"}, state: "excluded"},
		{handles: []string{"TEST-HOSTING", "TEST-ACCESS"}, state: "subscriber"},
		{handles: []string{"TEST-PROXY", "TEST-ACCESS"}, state: "subscriber", risk: true},
		{handles: []string{"TEST-ACCESS", "TEST-PROXY"}, state: "excluded", risk: true},
	} {
		ancestors := []arinOrganization{}
		for _, handle := range test.handles {
			ancestors = append(ancestors, arinOrganization{Handle: handle, Name: "Synthetic ISP"})
		}
		address := netip.MustParseAddr("192.0.2.1")
		actual := rules.classify(ancestors, address)
		if actual.qualityState != test.state || actual.nonQuality != (test.state != "subscriber") {
			t.Fatalf("%v: got %+v", test.handles, actual)
		}
		if risk := len(rules.networkRiskEvidence([][]arinOrganization{ancestors}, address)) != 0; risk != test.risk {
			t.Fatalf("%v: risk=%t want=%t", test.handles, risk, test.risk)
		}
	}
}

func TestSubscriberRulesRejectMissingAndContradictoryDecisions(t *testing.T) {
	for _, invalid := range []string{
		strings.Replace(testSubscriberRules, "    non_quality: false\n", "", 1),
		strings.Replace(testSubscriberRules, "quality_policy_version: 2", "quality_policy_version: 3", 1),
		strings.Replace(testSubscriberRules, "risk_category: virtual_isp", "risk_category: hosting", 1),
		strings.Replace(testSubscriberRules, "non_quality: true\n    risk_category", "non_quality: false\n    risk_category", 1),
	} {
		path := writeTestInput(t, filepath.Join(t.TempDir(), "rules.yml"), []byte(invalid))
		if _, err := loadClassificationRules(path); err == nil {
			t.Fatal("an incomplete or contradictory rule became subscriber evidence")
		}
	}
}

func TestSubscriberConflictingRulesDoNotDependOnOrder(t *testing.T) {
	for _, match := range []string{"org_handles: [TEST-ACCESS]", "prefixes: [192.0.2.0/24]"} {
		allow := classificationRule{Name: "allow", NonQuality: new(false), Source: "https://evidence.example/access", Reason: "reviewed access"}
		deny := classificationRule{Name: "exclude", NonQuality: new(true), Source: "https://evidence.example/hosting", Reason: "reviewed hosting"}
		if strings.HasPrefix(match, "org_handles") {
			allow.OrgHandles, deny.OrgHandles = []string{"TEST-ACCESS"}, []string{"TEST-ACCESS"}
		} else {
			allow.prefixes, deny.prefixes = []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}, []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}
		}
		for _, order := range [][]classificationRule{{allow, deny}, {deny, allow}, {deny, allow, allow}} {
			rules := classificationRules{QualityPolicyVersion: 2, Rules: order}
			result := rules.classify([]arinOrganization{{Handle: "TEST-ACCESS"}}, netip.MustParseAddr("192.0.2.1"))
			if !result.nonQuality || result.qualityState != "ambiguous" || !strings.Contains(result.source, allow.Source) || !strings.Contains(result.source, deny.Source) {
				t.Fatal("equally specific disagreement became access by input order")
			}
		}
	}
}

// A country match, allow override, or conflicting owner must not clear a
// reviewed virtual-ISP exclusion. Uncovered IPv4/IPv6 remain unknown Quality.
func TestSubscriberBuildRetainsIndependentRiskAndUnknown(t *testing.T) {
	dir := t.TempDir()
	source := writeTestInput(t, filepath.Join(dir, "source.xml"), []byte(`<bulkwhois>
<org><handle>TEST-PROXY</handle><name>Synthetic Proxy</name><iso3166-1><code2>US</code2></iso3166-1></org>
<org><handle>TEST-ACCESS</handle><name>Synthetic Access</name><iso3166-1><code2>US</code2></iso3166-1></org>
<net><handle>TEST-NET-PROXY</handle><orgHandle>TEST-PROXY</orgHandle><netBlocks><netBlock><type>S</type><cidrLength>24</cidrLength><startAddress>192.0.2.0</startAddress></netBlock></netBlocks></net>
<net><handle>TEST-NET-ACCESS</handle><orgHandle>TEST-ACCESS</orgHandle><netBlocks><netBlock><type>S</type><cidrLength>24</cidrLength><startAddress>192.0.2.0</startAddress></netBlock></netBlocks></net>
</bulkwhois>`))
	rules := writeTestInput(t, filepath.Join(dir, "rules.yml"), []byte(testSubscriberRules))
	geo := writeTestInput(t, filepath.Join(dir, "geo.mmdb"), testGeoDatabase(t))
	output := filepath.Join(dir, "release")
	if err := publishDirectory(output, func(stage string) error {
		return buildArinDatabase(t.Context(), source, geo, rules, stage)
	}); err != nil {
		t.Fatal(err)
	}
	db, err := mmdb.Open(filepath.Join(output, "arin.mmdb"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, test := range []struct {
		address, state string
		risk           bool
	}{
		{address: "192.0.2.1", state: "ambiguous", risk: true},
		{address: "198.51.100.1", state: "unknown"},
		{address: "2001:db8::1", state: "unknown"},
	} {
		var record struct {
			State          string `maxminddb:"quality_state"`
			Policy         uint32 `maxminddb:"quality_policy_version"`
			NonQuality     bool   `maxminddb:"non_quality"`
			Risk           bool   `maxminddb:"risk"`
			GeographicRisk bool   `maxminddb:"geographic_risk"`
			NetworkRisk    bool   `maxminddb:"network_risk"`
			Evidence       []struct {
				Category string `maxminddb:"category"`
				Source   string `maxminddb:"source"`
			} `maxminddb:"network_risk_evidence"`
		}
		if err := db.Lookup(netip.MustParseAddr(test.address)).Decode(&record); err != nil {
			t.Fatal(err)
		}
		if record.State != test.state || record.Policy != 2 || !record.NonQuality || record.Risk != test.risk || record.GeographicRisk || record.NetworkRisk != test.risk {
			t.Fatalf("%s: lost independent exclusions: %+v", test.address, record)
		}
		if test.risk && (len(record.Evidence) != 1 || record.Evidence[0].Category != "virtual_isp" || record.Evidence[0].Source == "") {
			t.Fatal("virtual-ISP risk lost reviewed provenance")
		}
	}
}

func TestReviewedProxyAndLeasingOwners(t *testing.T) {
	path := os.Getenv("ARIN_SUBSCRIBER_RULES_PATH")
	if path == "" {
		t.Skip("set ARIN_SUBSCRIBER_RULES_PATH to an explicit unpublished subscriber candidate")
	}
	rules, err := loadClassificationRules(path)
	if err != nil {
		t.Fatal(err)
	}
	if rules.QualityPolicyVersion != 2 {
		t.Fatal("subscriber candidate must explicitly select policy two")
	}
	for _, test := range []struct {
		handle string
		risk   bool
	}{{handle: "IL-909", risk: true}, {handle: "PL-1198", risk: true}, {handle: "IL-845"}} {
		ancestors := []arinOrganization{{Handle: test.handle}}
		address := netip.MustParseAddr("192.0.2.1")
		if result := rules.classify(ancestors, address); !result.nonQuality || result.source == "" {
			t.Fatalf("%s lost its reviewed quality exclusion", test.handle)
		}
		if risk := len(rules.networkRiskEvidence([][]arinOrganization{ancestors}, address)) != 0; risk != test.risk {
			t.Fatalf("%s: risk=%t want=%t", test.handle, risk, test.risk)
		}
	}
}
