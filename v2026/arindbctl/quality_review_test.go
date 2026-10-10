// Offline quality-catalog gates keep actual identities in explicit review inputs.
package main

import (
	"encoding/json"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	mmdb "github.com/oschwald/maxminddb-golang/v2"
)

// A reviewed owner can be absent from an otherwise internally consistent
// catalog. Independent expectations make that omission an explicit red gate.
func TestReviewedArinQualityExpectations(t *testing.T) {
	path := os.Getenv("ARIN_REVIEWED_QUALITY_EXPECTATIONS_PATH")
	if path == "" {
		t.Skip("set ARIN_REVIEWED_QUALITY_EXPECTATIONS_PATH to the offline review")
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal("quality review input unavailable")
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil || !stat.Mode().IsRegular() || stat.Size() > 64*1024 {
		t.Fatal("quality review input must be a bounded regular file")
	}
	var review struct {
		Version      int `json:"version"`
		Expectations []struct {
			OrgHandle  string `json:"org_handle"`
			RuleName   string `json:"rule_name"`
			NonQuality bool   `json:"non_quality"`
		} `json:"expectations"`
	}
	decoder := json.NewDecoder(io.LimitReader(file, 64*1024+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&review); err != nil {
		t.Fatal("quality review input is malformed")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		t.Fatal("quality review input must contain exactly one document")
	}
	if review.Version != 1 || len(review.Expectations) == 0 || len(review.Expectations) > 128 {
		t.Fatal("quality review input version or count is invalid")
	}
	rules, err := loadClassificationRules(os.Getenv("ARIN_REVIEWED_RULES_PATH"))
	if err != nil {
		t.Fatal("set ARIN_REVIEWED_RULES_PATH to the exact reviewed artifact")
	}
	seen := map[string]bool{}
	for i, expectation := range review.Expectations {
		if expectation.OrgHandle == "" || expectation.RuleName == "" || seen[expectation.OrgHandle] {
			t.Fatal("quality review has incomplete or duplicate owner expectations")
		}
		seen[expectation.OrgHandle] = true
		classification := rules.classify([]arinOrganization{{Handle: expectation.OrgHandle, Name: "Synthetic name"}}, netip.MustParseAddr("192.0.2.1"))
		if classification.nonQuality != expectation.NonQuality || classification.ruleName != expectation.RuleName || classification.orgHandle != expectation.OrgHandle || classification.reason == "" || classification.source == "" {
			t.Errorf("reviewed quality expectation %d is missing or has incorrect classification/provenance", i)
		}
	}
}

// Adding an exact hosting owner must change only quality: risk still follows
// registration, unknown children inherit, and reviewed access stays an override.
func TestArinQualityCatalogAdditionKeepsRiskAndAccessOverrides(t *testing.T) {
	directory := t.TempDir()
	source := writeTestInput(t, filepath.Join(directory, "source.xml"), []byte(`<bulkwhois>
<org><handle>TEST-NEW-CLOUD</handle><name>Synthetic Cloud Services</name><iso3166-1><code2>US</code2></iso3166-1></org>
<org><handle>TEST-UNKNOWN-CHILD</handle><name>Synthetic Unclassified Child</name><parentOrgHandle>TEST-NEW-CLOUD</parentOrgHandle><iso3166-1><code2>US</code2></iso3166-1></org>
<org><handle>TEST-ACCESS-CHILD</handle><name>Synthetic Reviewed Business Access</name><parentOrgHandle>TEST-NEW-CLOUD</parentOrgHandle><iso3166-1><code2>US</code2></iso3166-1></org>
<org><handle>TEST-NEAR-NAME</handle><name>Synthetic Cloud Services Access</name><iso3166-1><code2>US</code2></iso3166-1></org>
<net><handle>TEST-CLOUD-NET</handle><orgHandle>TEST-NEW-CLOUD</orgHandle><netBlocks><netBlock><type>DA</type><cidrLength>24</cidrLength><startAddress>192.0.2.0</startAddress><endAddress>192.0.2.255</endAddress></netBlock></netBlocks></net>
<net><handle>TEST-UNKNOWN-NET</handle><parentNetHandle>TEST-CLOUD-NET</parentNetHandle><orgHandle>TEST-UNKNOWN-CHILD</orgHandle><netBlocks><netBlock><type>S</type><cidrLength>27</cidrLength><startAddress>192.0.2.32</startAddress><endAddress>192.0.2.63</endAddress></netBlock></netBlocks></net>
<net><handle>TEST-ACCESS-NET</handle><parentNetHandle>TEST-CLOUD-NET</parentNetHandle><orgHandle>TEST-ACCESS-CHILD</orgHandle><netBlocks><netBlock><type>S</type><cidrLength>27</cidrLength><startAddress>192.0.2.64</startAddress><endAddress>192.0.2.95</endAddress></netBlock></netBlocks></net>
<net><handle>TEST-NEAR-NET</handle><orgHandle>TEST-NEAR-NAME</orgHandle><netBlocks><netBlock><type>S</type><cidrLength>27</cidrLength><startAddress>192.0.2.96</startAddress><endAddress>192.0.2.127</endAddress></netBlock></netBlocks></net>
</bulkwhois>`))
	geo := writeTestInput(t, filepath.Join(directory, "geolite2.mmdb"), testGeoDatabase(t))
	const accessRules = `version: 1
rules:
  - name: synthetic-reviewed-access
    org_handles: [TEST-ACCESS-CHILD]
    non_quality: false
    reason: synthetic reviewed business access
    source: https://evidence.example/access
  - name: synthetic-prefix-access
    prefixes: [192.0.2.192/27]
    non_quality: false
    reason: synthetic reviewed access prefix
    source: https://evidence.example/access-prefix
`
	const hostingAddition = `  - name: synthetic-new-cloud
    org_handles: [TEST-NEW-CLOUD]
    non_quality: true
    reason: synthetic cloud-only registration
    source: https://evidence.example/cloud
`
	for _, candidate := range []struct {
		name  string
		rules string
		added bool
	}{
		{name: "omitted", rules: accessRules},
		{name: "corrected", rules: accessRules + hostingAddition, added: true},
	} {
		rulesPath := writeTestInput(t, filepath.Join(directory, candidate.name+".yml"), []byte(candidate.rules))
		output := filepath.Join(directory, candidate.name)
		if err := publishDirectory(output, func(stage string) error {
			return buildArinDatabase(t.Context(), source, geo, rulesPath, stage)
		}); err != nil {
			t.Fatal(err)
		}
		db, err := mmdb.Open(filepath.Join(output, "arin.mmdb"))
		if err != nil {
			t.Fatal(err)
		}
		for offset := range 256 {
			var record struct {
				Risk          bool   `maxminddb:"risk"`
				NonQuality    bool   `maxminddb:"non_quality"`
				RuleName      string `maxminddb:"classification_rule"`
				Source        string `maxminddb:"classification_source"`
				Owner         string `maxminddb:"classification_org_handle"`
				Registered    string `maxminddb:"registered_country"`
				Associated    string `maxminddb:"associated_country"`
				FormatVersion uint32 `maxminddb:"classifier_version"`
			}
			if err := db.Lookup(netip.AddrFrom4([4]byte{192, 0, 2, byte(offset)})).Decode(&record); err != nil {
				db.Close()
				t.Fatal(err)
			}
			wantNonQuality := candidate.added && (offset < 64 || 128 <= offset && offset < 192 || 224 <= offset)
			wantAssociated := "us"
			if offset >= 128 {
				wantAssociated = "ca"
			}
			if record.Risk != (offset >= 128) || record.NonQuality != wantNonQuality || record.Registered != "us" || record.Associated != wantAssociated || record.FormatVersion != 1 {
				t.Errorf("%s offset %d changed quality/country invariants: %+v", candidate.name, offset, record)
			}
			if wantNonQuality && (record.RuleName != "synthetic-new-cloud" || record.Owner != "TEST-NEW-CLOUD" || record.Source != "https://evidence.example/cloud") {
				t.Errorf("%s offset %d lost exact owner or inherited provenance", candidate.name, offset)
			}
		}
		db.Close()
	}
}
