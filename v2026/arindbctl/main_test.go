// Synthetic databases and transports reproduce refresh and classification failures.
package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/maxmind/mmdbwriter"
	"github.com/maxmind/mmdbwriter/mmdbtype"
	mmdb "github.com/oschwald/maxminddb-golang/v2"
)

const testArinSource = `<bulkwhois>
<org><handle>TEST-HOSTING</handle><name>Synthetic Hosting</name><iso3166-1><code2>US</code2></iso3166-1></org>
<org><handle>TEST-ACCESS</handle><name>Synthetic Business Access</name><parentOrgHandle>TEST-HOSTING</parentOrgHandle><iso3166-1><code2>CA</code2></iso3166-1></org>
<net><orgHandle>TEST-ACCESS</orgHandle><netBlocks><netBlock><type>DS</type><cidrLength>26</cidrLength><startAddress>192.0.2.128</startAddress><endAddress>192.0.2.191</endAddress></netBlock></netBlocks></net>
<net><orgHandle>TEST-HOSTING</orgHandle><netBlocks><netBlock><type>DA</type><cidrLenth>24</cidrLenth><startAddress>192.000.002.000</startAddress><endAddress>192.0.2.255</endAddress></netBlock></netBlocks></net>
</bulkwhois>`

const testArinRules = `version: 1
rules:
  - name: synthetic-hosting
    org_handles: [TEST-HOSTING]
    non_quality: true
    reason: synthetic hosting service
    source: https://evidence.example/hosting
  - name: synthetic-access-exception
    prefixes: [192.0.2.64/26]
    non_quality: false
    reason: verified synthetic access provider
    source: https://evidence.example/access
`

func writeTestInput(t *testing.T, path string, content []byte) string {
	t.Helper()
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Every address in the fixture is from documentation space.
func testGeoDatabase(t *testing.T) []byte {
	t.Helper()
	w, err := mmdbwriter.New(mmdbwriter.Options{DatabaseType: "GeoLite2-City", IncludeReservedNetworks: true, Description: map[string]string{"en": "synthetic country fixture"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct{ prefix, country string }{{prefix: "192.0.2.0/25", country: "US"}, {prefix: "192.0.2.128/25", country: "CA"}} {
		_, network, err := net.ParseCIDR(row.prefix)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Insert(network, mmdbtype.Map{"country": mmdbtype.Map{"iso_code": mmdbtype.String(row.country), "geoname_id": mmdbtype.Uint32(1), "names": mmdbtype.Map{"en": mmdbtype.String(row.country)}}, "continent": mmdbtype.Map{"code": mmdbtype.String("NA"), "names": mmdbtype.Map{"en": mmdbtype.String("Synthetic continent")}}}); err != nil {
			t.Fatal(err)
		}
	}
	var content bytes.Buffer
	if _, err := w.WriteTo(&content); err != nil {
		t.Fatal(err)
	}
	return content.Bytes()
}

func TestArinBuildSplitsCountriesAndNarrowAccessExceptions(t *testing.T) {
	dir := t.TempDir()
	source := writeTestInput(t, filepath.Join(dir, "source.xml"), []byte(testArinSource))
	rules := writeTestInput(t, filepath.Join(dir, "rules.yml"), []byte(testArinRules))
	geo := writeTestInput(t, filepath.Join(dir, "geolite2.mmdb"), testGeoDatabase(t))
	output := filepath.Join(dir, "release")
	if err := publishDirectory(output, func(stage string) error { return buildArinDatabase(context.Background(), source, geo, rules, stage) }); err != nil {
		t.Fatal(err)
	}
	db, err := mmdb.Open(filepath.Join(output, "arin.mmdb"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, c := range []struct {
		address          string
		risk, nonQuality bool
	}{
		{address: "192.0.2.1", nonQuality: true}, {address: "192.0.2.65"},
		{address: "192.0.2.129", nonQuality: true}, {address: "192.0.2.200", risk: true, nonQuality: true},
		{address: "198.51.100.1"},
	} {
		var record struct {
			Risk       bool `maxminddb:"risk"`
			NonQuality bool `maxminddb:"non_quality"`
		}
		if err := db.Lookup(netip.MustParseAddr(c.address)).Decode(&record); err != nil {
			t.Fatal(err)
		}
		if record.Risk != c.risk || record.NonQuality != c.nonQuality {
			t.Fatalf("%s: got %+v want risk=%t non_quality=%t", c.address, record, c.risk, c.nonQuality)
		}
	}
}

// Existing readers treat the last registration country as the most specific.
func TestArinBuildKeepsParentFirstCountryOrder(t *testing.T) {
	dir := t.TempDir()
	source := writeTestInput(t, filepath.Join(dir, "source.xml"), []byte(testArinSource))
	rules := writeTestInput(t, filepath.Join(dir, "rules.yml"), []byte(testArinRules))
	geo := writeTestInput(t, filepath.Join(dir, "geolite2.mmdb"), testGeoDatabase(t))
	output := filepath.Join(dir, "release")
	if err := publishDirectory(output, func(stage string) error { return buildArinDatabase(t.Context(), source, geo, rules, stage) }); err != nil {
		t.Fatal(err)
	}
	db, err := mmdb.Open(filepath.Join(output, "arin.mmdb"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var record struct {
		Countries []string `maxminddb:"org_country_codes"`
	}
	if err := db.Lookup(netip.MustParseAddr("192.0.2.129")).Decode(&record); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(record.Countries, []string{"us", "ca"}) {
		t.Fatalf("registration countries=%v, want parent first and direct owner last", record.Countries)
	}
}

// An unknown child inherits its known hosting parent until a reviewed child
// or narrower prefix rule explicitly classifies that allocation differently.
func TestArinBuildInheritsHostingParentUntilReviewedOverride(t *testing.T) {
	for _, test := range []struct {
		rules               string
		nonQuality          bool
		reason              string
		classificationOwner string
		ruleName            string
		evidence            string
	}{
		{rules: testArinRules, nonQuality: true, reason: "synthetic hosting service", classificationOwner: "TEST-HOSTING",
			ruleName: "synthetic-hosting", evidence: "https://evidence.example/hosting"},
		{rules: testArinRules + `  - name: synthetic-child-hosting
    org_handles: [TEST-ACCESS]
    non_quality: true
    reason: reviewed synthetic child hosting
    source: https://evidence.example/child-hosting
`, nonQuality: true, reason: "reviewed synthetic child hosting", classificationOwner: "TEST-ACCESS",
			ruleName: "synthetic-child-hosting", evidence: "https://evidence.example/child-hosting"},
		{rules: testArinRules + `  - name: synthetic-child-hosting
    org_handles: [TEST-ACCESS]
    non_quality: true
    reason: reviewed synthetic child hosting
    source: https://evidence.example/child-hosting
  - name: synthetic-child-access
    org_handles: [TEST-ACCESS]
    non_quality: false
    reason: reviewed synthetic child access
    source: https://evidence.example/child-access
`, reason: "reviewed synthetic child access", classificationOwner: "TEST-ACCESS",
			ruleName: "synthetic-child-access", evidence: "https://evidence.example/child-access"},
		{rules: testArinRules + `  - name: synthetic-child-prefix-access
    prefixes: [192.0.2.128/27]
    non_quality: false
    reason: reviewed synthetic child prefix access
    source: https://evidence.example/child-prefix-access
`, reason: "reviewed synthetic child prefix access",
			ruleName: "synthetic-child-prefix-access", evidence: "https://evidence.example/child-prefix-access"},
	} {
		dir := t.TempDir()
		source := writeTestInput(t, filepath.Join(dir, "source.xml"), []byte(testArinSource))
		rules := writeTestInput(t, filepath.Join(dir, "rules.yml"), []byte(test.rules))
		geo := writeTestInput(t, filepath.Join(dir, "geolite2.mmdb"), testGeoDatabase(t))
		output := filepath.Join(dir, "release")
		if err := publishDirectory(output, func(stage string) error { return buildArinDatabase(t.Context(), source, geo, rules, stage) }); err != nil {
			t.Fatal(err)
		}
		db, err := mmdb.Open(filepath.Join(output, "arin.mmdb"))
		if err != nil {
			t.Fatal(err)
		}
		var record struct {
			NonQuality          bool   `maxminddb:"non_quality"`
			Reason              string `maxminddb:"reason"`
			OrgHandle           string `maxminddb:"org_handle"`
			ClassificationOwner string `maxminddb:"classification_org_handle"`
			RuleName            string `maxminddb:"classification_rule"`
			Evidence            string `maxminddb:"classification_source"`
		}
		err = db.Lookup(netip.MustParseAddr("192.0.2.129")).Decode(&record)
		db.Close()
		if err != nil {
			t.Fatal(err)
		}
		if record.NonQuality != test.nonQuality || record.Reason != test.reason || record.OrgHandle != "TEST-ACCESS" {
			t.Fatalf("direct-owner override: got %+v, want non_quality=%t reason=%q", record, test.nonQuality, test.reason)
		}
		if record.ClassificationOwner != test.classificationOwner || record.RuleName != test.ruleName || record.Evidence != test.evidence {
			t.Fatalf("inherited/override rule provenance was lost: %+v", record)
		}
	}
}

// Inputs must be distinguishable even when two rule revisions emit equal data.
func TestArinBuildManifestBindsEveryInput(t *testing.T) {
	dir := t.TempDir()
	source := writeTestInput(t, filepath.Join(dir, "source.xml"), []byte(testArinSource))
	rules := writeTestInput(t, filepath.Join(dir, "rules.yml"), []byte(testArinRules))
	geo := writeTestInput(t, filepath.Join(dir, "geolite2.mmdb"), testGeoDatabase(t))
	output := filepath.Join(dir, "release")
	if err := publishDirectory(output, func(stage string) error { return buildArinDatabase(t.Context(), source, geo, rules, stage) }); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(output, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Inputs map[string]string `json:"inputs_sha256"`
	}
	if err := json.Unmarshal(content, &manifest); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{"arin_xml": source, "classification_rules": rules, "geolite2": geo} {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(content)
		if manifest.Inputs[name] != hex.EncodeToString(hash[:]) {
			t.Errorf("manifest does not bind %s input", name)
		}
	}
}

// Registry/referral organizations are administrative pointers, not evidence
// that every routed address must share the registry office's country. A real
// ARIN child allocation still supplies its own authoritative registration.
func TestArinBuildReferralCountryIsUnknownAndDirectChildrenRemainAuthoritative(t *testing.T) {
	for _, test := range []struct {
		blockType string
		scope     string
		risk      bool
	}{
		{blockType: "AF", scope: "referral"}, {blockType: "FX", scope: "referral"},
		{blockType: "AP", scope: "referral"}, {blockType: "LN", scope: "referral"},
		{blockType: "LX", scope: "referral"}, {blockType: "PV", scope: "referral"},
		{blockType: "PX", scope: "referral"}, {blockType: "RN", scope: "referral"},
		{blockType: "RV", scope: "referral"}, {blockType: "RX", scope: "referral"},
		{blockType: "AR", scope: "registry"}, {blockType: "IR", scope: "reserved"},
		{blockType: "IU", scope: "reserved"}, {blockType: "VR", scope: "unknown"},
		{blockType: "FUTURE", scope: "unknown"}, {scope: "unknown"},
		{blockType: "A", scope: "arin", risk: true}, {blockType: "AV", scope: "arin", risk: true},
		{blockType: "DA", scope: "arin", risk: true}, {blockType: "DS", scope: "arin", risk: true},
		{blockType: "S", scope: "arin", risk: true},
	} {
		dir := t.TempDir()
		xml := fmt.Sprintf(`<bulkwhois>
<org><handle>TEST-REGISTRY</handle><name>Synthetic registry pointer</name><iso3166-1><code2>DE</code2></iso3166-1></org>
<org><handle>TEST-DIRECT-CA</handle><name>Synthetic direct owner CA</name><iso3166-1><code2>CA</code2></iso3166-1></org>
<org><handle>TEST-DIRECT-US</handle><name>Synthetic direct owner US</name><iso3166-1><code2>US</code2></iso3166-1></org>
<net><orgHandle>TEST-REGISTRY</orgHandle><netBlocks><netBlock><type>%s</type><cidrLength>24</cidrLength><startAddress>192.0.2.0</startAddress><endAddress>192.0.2.255</endAddress></netBlock></netBlocks></net>
<net><orgHandle>TEST-DIRECT-CA</orgHandle><netBlocks><netBlock><type>DS</type><cidrLength>26</cidrLength><startAddress>192.0.2.128</startAddress><endAddress>192.0.2.191</endAddress></netBlock></netBlocks></net>
<net><orgHandle>TEST-DIRECT-US</orgHandle><netBlocks><netBlock><type>DA</type><cidrLength>27</cidrLength><startAddress>192.0.2.192</startAddress><endAddress>192.0.2.223</endAddress></netBlock></netBlocks></net>
</bulkwhois>`, test.blockType)
		source := writeTestInput(t, filepath.Join(dir, "source.xml"), []byte(xml))
		rules := writeTestInput(t, filepath.Join(dir, "rules.yml"), []byte(testArinRules))
		geo := writeTestInput(t, filepath.Join(dir, "geolite2.mmdb"), testGeoDatabase(t))
		output := filepath.Join(dir, "release")
		if err := publishDirectory(output, func(stage string) error { return buildArinDatabase(t.Context(), source, geo, rules, stage) }); err != nil {
			t.Fatal(err)
		}
		db, err := mmdb.Open(filepath.Join(output, "arin.mmdb"))
		if err != nil {
			t.Fatal(err)
		}
		for _, check := range []struct {
			address, scope, country string
			risk                    bool
		}{
			{address: "192.0.2.1", scope: test.scope, risk: test.risk},
			{address: "192.0.2.129", scope: "arin", country: "ca"},
			{address: "192.0.2.193", scope: "arin", country: "us", risk: true},
		} {
			if check.address == "192.0.2.1" && test.scope == "arin" {
				check.country = "de"
			}
			var record struct {
				Risk              bool   `maxminddb:"risk"`
				RegistrationScope string `maxminddb:"registration_scope"`
				RegisteredCountry string `maxminddb:"registered_country"`
			}
			if err := db.Lookup(netip.MustParseAddr(check.address)).Decode(&record); err != nil {
				db.Close()
				t.Fatal(err)
			}
			if record.Risk != check.risk || record.RegistrationScope != check.scope || record.RegisteredCountry != check.country {
				db.Close()
				t.Fatalf("type=%q address=%s got %+v, want risk=%t scope=%s country=%s", test.blockType, check.address, record, check.risk, check.scope, check.country)
			}
		}
		db.Close()
	}
}

func TestArinTruncatedSourceDoesNotPublishPartialDatabase(t *testing.T) {
	dir := t.TempDir()
	source := writeTestInput(t, filepath.Join(dir, "source.xml"), []byte(strings.TrimSuffix(testArinSource, "</bulkwhois>")))
	output := filepath.Join(dir, "release")
	err := publishDirectory(output, func(string) error { return scanArinXml(context.Background(), source, nil, nil) })
	if err == nil {
		t.Fatal("truncated XML was treated as a complete database")
	}
	if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial release published: %v", err)
	}
}

func TestIpDatabasePublicationPreservesExistingVersion(t *testing.T) {
	output := t.TempDir()
	old := writeTestInput(t, filepath.Join(output, "arin.mmdb"), []byte("old complete version"))
	called := false
	err := publishDirectory(output, func(string) error { called = true; return nil })
	if err == nil || called {
		t.Fatal("refresh overwrote an existing release directory")
	}
	content, err := os.ReadFile(old)
	if err != nil || string(content) != "old complete version" {
		t.Fatal("old version changed")
	}
}

func TestGeoLiteRefreshPublishesDatabaseAndPlacesTogether(t *testing.T) {
	output := filepath.Join(t.TempDir(), "version")
	err := publishDirectory(output, func(stage string) error {
		return refreshGeolite2(context.Background(), "protected-test-config", stage, func(_ context.Context, config, directory string) error {
			if config != "protected-test-config" {
				t.Fatal("credential path was changed")
			}
			return os.WriteFile(filepath.Join(directory, "GeoLite2-City.mmdb"), testGeoDatabase(t), 0o600)
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"geolite2.mmdb", "places.yml", "manifest.json"} {
		if _, err := os.Stat(filepath.Join(output, name)); err != nil {
			t.Fatal(err)
		}
	}
}

type testRoundTripper func(*http.Request) (*http.Response, error)

func (self testRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return self(request)
}

func TestArinDownloadCredentialsNeverEnterTransportErrors(t *testing.T) {
	dir := t.TempDir()
	secret := "SYNTHETIC_TEST_ONLY_SECRET"
	credentials := writeTestInput(t, filepath.Join(dir, "arin.yml"), []byte("api_key: "+secret+"\n"))
	client := &http.Client{Transport: testRoundTripper(func(request *http.Request) (*http.Response, error) {
		if request.URL.Query().Get("apikey") != secret {
			t.Fatal("credential was not bound to ARIN request")
		}
		return nil, errors.New("synthetic transport echoed " + request.URL.String())
	})}
	err := refreshArin(context.Background(), credentials, filepath.Join(dir, "source.xml"), client)
	if err == nil || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "apikey") {
		t.Fatalf("unsafe error: %v", err)
	}
}

func TestArinDownloadExtractsOnlyValidatedXml(t *testing.T) {
	dir := t.TempDir()
	credentials := writeTestInput(t, filepath.Join(dir, "arin.yml"), []byte("api_key: SYNTHETIC_TEST_ONLY\n"))
	var archive bytes.Buffer
	z := zip.NewWriter(&archive)
	entry, err := z.Create("arin_db.xml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(entry, testArinSource); err != nil {
		t.Fatal(err)
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: testRoundTripper(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/public/rest/downloads/bulkwhois/orgs+nets.zip" {
			t.Error("download must request only the organization/network archive, not contact records")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(archive.Bytes())), Header: make(http.Header)}, nil
	})}
	if err := refreshArin(context.Background(), credentials, filepath.Join(dir, "source.xml"), client); err != nil {
		t.Fatal(err)
	}
}

// The read budget applies before parsing or buffering any oversized secret.
func TestArinCredentialReadIsBoundedAndRedacted(t *testing.T) {
	input := strings.NewReader(strings.Repeat("x", int(maximumArinCredentialBytes)*2))
	before := input.Len()
	credential, err := decodeArinCredentials(input)
	if err == nil || credential.ApiKey != "" || before-input.Len() != int(maximumArinCredentialBytes)+1 {
		t.Fatal("credential decoder read past its fixed byte budget")
	}
	for _, input := range []string{"api_key: [SYNTHETIC_SECRET]\n", "unknown: SYNTHETIC_SECRET\n", "api_key: SYNTHETIC_SECRET\n---\napi_key: SECOND_SECRET\n"} {
		credential, err := decodeArinCredentials(strings.NewReader(input))
		if err == nil || credential.ApiKey != "" || strings.Contains(err.Error(), "SECRET") {
			t.Fatal("invalid credential leaked a value or bypassed strict single-document decoding")
		}
	}
}

// Both real refresh/build/publication paths run with command-owned transports.
// No partial bundle can escape a failed first or second download stage.
func TestIpDatabaseRefreshOrdersSourcesAndPublishesOnlyCompleteBundle(t *testing.T) {
	for _, failureStage := range []string{"", "geoip", "arin"} {
		dir := t.TempDir()
		credentials := writeTestInput(t, filepath.Join(dir, "arin.yml"), []byte("api_key: SYNTHETIC_TEST_ONLY\n"))
		rules := writeTestInput(t, filepath.Join(dir, "rules.yml"), []byte(testArinRules))
		output := filepath.Join(dir, "bundle")
		var archive bytes.Buffer
		writer := zip.NewWriter(&archive)
		entry, err := writer.Create("arin_db.xml")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(entry, testArinSource); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		events := []string{}
		dependencies := commandDependencies{
			geoipUpdate: func(_ context.Context, _ string, directory string) error {
				events = append(events, "geoip")
				if failureStage == "geoip" {
					return errors.New("synthetic GeoLite failure")
				}
				return os.WriteFile(filepath.Join(directory, "GeoLite2-City.mmdb"), testGeoDatabase(t), 0o600)
			},
			arinClient: &http.Client{Transport: testRoundTripper(func(*http.Request) (*http.Response, error) {
				events = append(events, "arin")
				status := http.StatusOK
				if failureStage == "arin" {
					status = http.StatusServiceUnavailable
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(bytes.NewReader(archive.Bytes())), Header: make(http.Header)}, nil
			})},
		}
		err = runCommandWithDependencies(t.Context(), []string{"refresh", "--geoip-config", filepath.Join(dir, "GeoIP.conf"), "--credentials", credentials, "--rules", rules, "--output", output}, io.Discard, dependencies)
		want := []string{"geoip", "arin"}
		if failureStage == "geoip" {
			want = []string{"geoip"}
		}
		if !slices.Equal(events, want) {
			t.Fatalf("stage %q: source order=%v, want %v", failureStage, events, want)
		}
		if failureStage != "" {
			if err == nil {
				t.Fatalf("stage %s: failed refresh succeeded", failureStage)
			}
			if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed source refresh published a partial bundle")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"mmdb/geolite2.mmdb", "mmdb/places.yml", "mmdb/manifest.json", "arindb/arin.mmdb", "arindb/manifest.json"} {
			if _, err := os.Stat(filepath.Join(output, name)); err != nil {
				t.Fatal(err)
			}
		}
	}
}
