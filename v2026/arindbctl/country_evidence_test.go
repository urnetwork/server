// Synthetic complete databases exercise prefix evidence, authority and publication.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/maxmind/mmdbwriter"
	"github.com/maxmind/mmdbwriter/mmdbtype"
	mmdb "github.com/oschwald/maxminddb-golang/v2"
	"gopkg.in/yaml.v3"
)

// The access owner intentionally inherits hosting independently of geography.
const testCountryArinSource = `<bulkwhois>
<org><handle>TEST-HOSTING</handle><name>Synthetic Hosting</name><iso3166-1><code2>US</code2></iso3166-1></org>
<org><handle>TEST-OPERATOR</handle><name>Synthetic Operator</name><parentOrgHandle>TEST-HOSTING</parentOrgHandle><iso3166-1><code2>US</code2></iso3166-1></org>
<net><handle>TEST-NET</handle><orgHandle>TEST-OPERATOR</orgHandle><netBlocks><netBlock><type>DA</type><cidrLength>24</cidrLength><startAddress>192.0.2.0</startAddress><endAddress>192.0.2.255</endAddress></netBlock></netBlocks></net>
</bulkwhois>`

// One invocation owns all inputs and an explicit freshness instant.
type countryEvidenceFixture struct {
	directory string
	source    string
	geo       string
	rulesPath string
	buildTime time.Time
	rules     classificationRules
}

// Every identity and address is synthetic; snapshots have no external I/O.
func newCountryEvidenceFixture(t *testing.T) *countryEvidenceFixture {
	t.Helper()
	directory := t.TempDir()
	buildTime := time.Date(2030, 1, 2, 0, 0, 0, 0, time.UTC)
	snapshot := []byte("192.0.2.128/26,CA,,,\n")
	hash := sha256.Sum256(snapshot)
	writeTestInput(t, filepath.Join(directory, "geography.csv"), snapshot)
	return &countryEvidenceFixture{
		directory: directory,
		source:    writeTestInput(t, filepath.Join(directory, "source.xml"), []byte(testCountryArinSource)),
		geo:       writeTestInput(t, filepath.Join(directory, "geolite2.mmdb"), testGeoDatabase(t)),
		rulesPath: filepath.Join(directory, "rules.yml"), buildTime: buildTime,
		rules: classificationRules{
			Version: 1,
			Rules: []classificationRule{{Name: "synthetic-hosting", OrgHandles: []string{"TEST-HOSTING"}, NonQuality: new(true),
				Reason: "synthetic reviewed hosting ancestor", Source: "https://evidence.example/hosting"}},
			CountryPolicyVersion: 2,
			CountrySources: []countryEvidenceSource{{Id: "synthetic-geography", Url: "https://evidence.example/geography.csv",
				File: "geography.csv", Sha256: hex.EncodeToString(hash[:]), ObservedAt: buildTime.Add(-24 * time.Hour), ExpiresAt: buildTime.Add(24 * time.Hour)}},
			CountryRules: []countryEvidenceRule{{Name: "synthetic-canadian-prefix", Prefix: "192.0.2.128/26",
				Owners: []countryEvidenceOwner{{NetHandle: "TEST-NET", OrgHandle: "TEST-OPERATOR"}}, SourceId: "synthetic-geography",
				CountryCodes: []string{"CA"}, Reason: "reviewed synthetic prefix geography"}},
		},
	}
}

// Exercise the real atomic-publication entry point with a deterministic clock.
func (self *countryEvidenceFixture) build(t *testing.T, name string) (string, error) {
	t.Helper()
	content, err := yaml.Marshal(self.rules)
	if err != nil {
		t.Fatal(err)
	}
	writeTestInput(t, self.rulesPath, content)
	output := filepath.Join(self.directory, name)
	err = publishDirectory(output, func(stage string) error {
		return buildArinDatabaseAt(t.Context(), self.source, self.geo, self.rulesPath, stage, self.buildTime)
	})
	return output, err
}

// Decode both final flags and retained evidence from the generated database.
type countryEvidenceTestRecord struct {
	Risk                 bool     `maxminddb:"risk"`
	NonQuality           bool     `maxminddb:"non_quality"`
	RegistrationMismatch bool     `maxminddb:"registration_mismatch"`
	ClassifierVersion    uint32   `maxminddb:"classifier_version"`
	CountryPolicyVersion uint32   `maxminddb:"country_policy_version"`
	State                string   `maxminddb:"country_evidence_state"`
	Countries            []string `maxminddb:"credible_country_codes"`
	Evidence             []struct {
		Rule   string `maxminddb:"rule"`
		Source string `maxminddb:"source_id"`
		Reason string `maxminddb:"reason"`
	} `maxminddb:"country_evidence"`
}

// Visit every address, including every country/rule boundary and both endpoints.
func checkCountryEvidenceDatabase(t *testing.T, output string, check func(int, countryEvidenceTestRecord)) {
	t.Helper()
	db, err := mmdb.Open(filepath.Join(output, "arin.mmdb"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Verify(); err != nil {
		t.Fatal(err)
	}
	for i := range 256 {
		var record countryEvidenceTestRecord
		if err := db.Lookup(netip.AddrFrom4([4]byte{192, 0, 2, byte(i)})).Decode(&record); err != nil {
			t.Fatal(err)
		}
		if !record.NonQuality || record.ClassifierVersion != 1 {
			t.Fatalf("address offset %d lost inherited hosting or reader compatibility: %+v", i, record)
		}
		check(i, record)
	}
}

// Raw registration mismatch is the pre-fix behavior; only evidenced cells change.
func TestArinCountryEvidenceRefinesOnlyReviewedPrefix(t *testing.T) {
	fixture := newCountryEvidenceFixture(t)
	output, err := fixture.build(t, "reviewed")
	if err != nil {
		t.Fatal(err)
	}
	checkCountryEvidenceDatabase(t, output, func(i int, record countryEvidenceTestRecord) {
		reviewed := 128 <= i && i < 192
		if record.RegistrationMismatch != reviewed || record.Risk != (i >= 192) || (record.CountryPolicyVersion == 2) != reviewed {
			t.Fatalf("address offset %d: evidence broadened or raw mismatch lost: %+v", i, record)
		}
		if reviewed && (record.State != "known" || !slices.Equal(record.Countries, []string{"ca"}) || len(record.Evidence) != 1 || record.Evidence[0].Source == "" || record.Evidence[0].Reason == "") {
			t.Fatalf("address offset %d lost country provenance: %+v", i, record)
		}
		if !reviewed && (record.State != "" || len(record.Evidence) != 0) {
			t.Fatal("an unreviewed sibling acquired country-policy evidence")
		}
	})
	fixture.rules.CountryPolicyVersion, fixture.rules.CountryRules, fixture.rules.CountrySources = 0, nil, nil
	baseline, err := fixture.build(t, "baseline")
	if err != nil {
		t.Fatal(err)
	}
	checkCountryEvidenceDatabase(t, baseline, func(i int, record countryEvidenceTestRecord) {
		if record.Risk != (i >= 128) || record.CountryPolicyVersion != 0 || record.State != "" {
			t.Fatalf("legacy registration policy changed at offset %d: %+v", i, record)
		}
	})
}

// A reviewed set can include several countries; it is not an unconditional allow.
func TestArinCountryEvidenceCompleteCountrySets(t *testing.T) {
	for _, countries := range [][]string{{"CA"}, {"US", "CA"}} {
		fixture := newCountryEvidenceFixture(t)
		fixture.rules.CountryRules[0].Prefix = "192.0.2.0/24"
		fixture.rules.CountryRules[0].CountryCodes = countries
		output, err := fixture.build(t, "release")
		if err != nil {
			t.Fatal(err)
		}
		checkCountryEvidenceDatabase(t, output, func(i int, record countryEvidenceTestRecord) {
			if record.Risk != (len(countries) == 1 && i < 128) || record.State != "known" || record.CountryPolicyVersion != 2 {
				t.Fatalf("countries=%v offset=%d: %+v", countries, i, record)
			}
		})
	}
}

// Explicit uncertainty is narrow, and conflicting assertions never become a set.
func TestArinCountryEvidenceUnknownAndConflictingSources(t *testing.T) {
	for _, mode := range []string{"unknown", "country-conflict", "known-unknown-conflict"} {
		fixture := newCountryEvidenceFixture(t)
		wantState, wantEvidence := "unknown", 1
		if mode == "unknown" {
			fixture.rules.CountryRules[0].CountryCodes = nil
			fixture.rules.CountryRules[0].Uncertainty = "documented global routing without a complete country set"
		} else {
			secondSource := fixture.rules.CountrySources[0]
			secondSource.Id, secondSource.Url = "synthetic-second", "https://second.example/geography.csv"
			fixture.rules.CountrySources = append(fixture.rules.CountrySources, secondSource)
			secondRule := fixture.rules.CountryRules[0]
			secondRule.Name, secondRule.SourceId, secondRule.CountryCodes = "synthetic-second", secondSource.Id, []string{"US"}
			if mode == "known-unknown-conflict" {
				secondRule.CountryCodes, secondRule.Uncertainty = nil, "documented uncertain deployment"
			}
			fixture.rules.CountryRules = append(fixture.rules.CountryRules, secondRule)
			wantState, wantEvidence = "ambiguous", 2
		}
		for _, order := range []string{"forward", "reverse"} {
			output, err := fixture.build(t, order)
			if err != nil {
				t.Fatal(err)
			}
			checkCountryEvidenceDatabase(t, output, func(i int, record countryEvidenceTestRecord) {
				if i >= 128 && i < 192 {
					if record.Risk || record.State != wantState || len(record.Countries) != 0 || len(record.Evidence) != wantEvidence {
						t.Fatalf("%s/%s offset=%d: %+v", mode, order, i, record)
					}
				} else if record.Risk != (i >= 192) || record.CountryPolicyVersion != 0 {
					t.Fatalf("%s/%s affected an unreviewed sibling", mode, order)
				}
			})
			slices.Reverse(fixture.rules.CountryRules)
			slices.Reverse(fixture.rules.CountrySources)
		}
	}
}

// A more-specific reviewed prefix wins within its range, independent of order.
func TestArinCountryEvidenceMostSpecificPrefixWins(t *testing.T) {
	fixture := newCountryEvidenceFixture(t)
	narrow := fixture.rules.CountryRules[0]
	narrow.Name, narrow.Prefix, narrow.CountryCodes = "synthetic-narrow", "192.0.2.160/27", []string{"US"}
	fixture.rules.CountryRules = append(fixture.rules.CountryRules, narrow)
	for _, order := range []string{"forward", "reverse"} {
		output, err := fixture.build(t, order)
		if err != nil {
			t.Fatal(err)
		}
		checkCountryEvidenceDatabase(t, output, func(i int, record countryEvidenceTestRecord) {
			if record.Risk != (i >= 160) {
				t.Fatalf("%s offset=%d: narrow country evidence was lost: %+v", order, i, record)
			}
		})
		slices.Reverse(fixture.rules.CountryRules)
	}
}

// A direct child keeps its own baseline and still inherits hosting separately.
func TestArinCountryEvidenceDoesNotFollowOrganizationAncestry(t *testing.T) {
	fixture := newCountryEvidenceFixture(t)
	child := `<org><handle>TEST-CHILD</handle><name>Synthetic Child</name><parentOrgHandle>TEST-OPERATOR</parentOrgHandle><iso3166-1><code2>US</code2></iso3166-1></org>
<net><handle>TEST-CHILD-NET</handle><parentNetHandle>TEST-NET</parentNetHandle><orgHandle>TEST-CHILD</orgHandle><netBlocks><netBlock><type>S</type><cidrLength>27</cidrLength><startAddress>192.0.2.160</startAddress><endAddress>192.0.2.191</endAddress></netBlock></netBlocks></net>`
	writeTestInput(t, fixture.source, []byte(strings.Replace(testCountryArinSource, "</bulkwhois>", child+"</bulkwhois>", 1)))
	output, err := fixture.build(t, "release")
	if err != nil {
		t.Fatal(err)
	}
	checkCountryEvidenceDatabase(t, output, func(i int, record countryEvidenceTestRecord) {
		if record.Risk != (i >= 160) || (record.CountryPolicyVersion == 2) != (128 <= i && i < 160) {
			t.Fatalf("offset %d: parent country evidence leaked into a child: %+v", i, record)
		}
	})
}

// Unknown GeoLite cells cannot disagree with even a complete country assertion.
func TestArinCountryEvidenceMissingGeoLiteCountryRemainsUnknown(t *testing.T) {
	for _, country := range []string{"", "ZZ", "XX", "EU"} {
		fixture := newCountryEvidenceFixture(t)
		fixture.rules.CountryRules[0].Prefix = "192.0.2.0/24"
		writer, err := mmdbwriter.New(mmdbwriter.Options{DatabaseType: "GeoLite2-City", IncludeReservedNetworks: true, Description: map[string]string{"en": "synthetic unknown geography"}})
		if err != nil {
			t.Fatal(err)
		}
		_, network, err := net.ParseCIDR("192.0.2.0/24")
		if err != nil {
			t.Fatal(err)
		}
		if err := writer.Insert(network, mmdbtype.Map{"country": mmdbtype.Map{"iso_code": mmdbtype.String(country)}}); err != nil {
			t.Fatal(err)
		}
		var content bytes.Buffer
		if _, err := writer.WriteTo(&content); err != nil {
			t.Fatal(err)
		}
		writeTestInput(t, fixture.geo, content.Bytes())
		output, err := fixture.build(t, "release")
		if err != nil {
			t.Fatal(err)
		}
		checkCountryEvidenceDatabase(t, output, func(i int, record countryEvidenceTestRecord) {
			if record.Risk || record.RegistrationMismatch != (country != "") || record.State != "known" {
				t.Fatalf("country=%q offset=%d: unknown GeoLite country became risk: %+v", country, i, record)
			}
		})
	}
}

// Bad authority and schema inputs stop publication instead of widening a waiver.
func TestArinCountryEvidenceRejectsUnboundOrMalformedRules(t *testing.T) {
	for _, change := range []struct {
		name string
		edit func(*countryEvidenceFixture)
	}{
		{name: "broader-prefix", edit: func(f *countryEvidenceFixture) { f.rules.CountryRules[0].Prefix = "192.0.2.0/23" }},
		{name: "outside-prefix", edit: func(f *countryEvidenceFixture) { f.rules.CountryRules[0].Prefix = "198.51.100.0/24" }},
		{name: "host-bits", edit: func(f *countryEvidenceFixture) { f.rules.CountryRules[0].Prefix = "192.0.2.129/26" }},
		{name: "changed-owner", edit: func(f *countryEvidenceFixture) { f.rules.CountryRules[0].Owners[0].OrgHandle = "TEST-PREVIOUS-OWNER" }},
		{name: "missing-network", edit: func(f *countryEvidenceFixture) { f.rules.CountryRules[0].Owners[0].NetHandle = "TEST-MISSING-NET" }},
		{name: "no-authority", edit: func(f *countryEvidenceFixture) { f.rules.CountryRules[0].Owners = nil }},
		{name: "referral", edit: func(f *countryEvidenceFixture) {
			writeTestInput(t, f.source, []byte(strings.Replace(testCountryArinSource, "<type>DA</type>", "<type>AP</type>", 1)))
		}},
		{name: "unknown-country", edit: func(f *countryEvidenceFixture) { f.rules.CountryRules[0].CountryCodes = []string{"ZZ"} }},
		{name: "no-country-or-uncertainty", edit: func(f *countryEvidenceFixture) { f.rules.CountryRules[0].CountryCodes = nil }},
		{name: "country-and-uncertainty", edit: func(f *countryEvidenceFixture) { f.rules.CountryRules[0].Uncertainty = "unknown" }},
		{name: "duplicate-country", edit: func(f *countryEvidenceFixture) { f.rules.CountryRules[0].CountryCodes = []string{"CA", "ca"} }},
		{name: "wrong-policy", edit: func(f *countryEvidenceFixture) { f.rules.CountryPolicyVersion = 3 }},
	} {
		fixture := newCountryEvidenceFixture(t)
		change.edit(fixture)
		output, err := fixture.build(t, "rejected")
		if err == nil {
			t.Fatalf("%s: invalid rule was published", change.name)
		}
		if _, err := os.Stat(output); !os.IsNotExist(err) {
			t.Fatalf("%s: invalid rule left a visible release", change.name)
		}
	}
}

// Every incomparable owner must be named; a single owner cannot clear the group.
func TestArinCountryEvidenceRequiresCompleteOwnerConsensus(t *testing.T) {
	fixture := newCountryEvidenceFixture(t)
	second := `<org><handle>TEST-SECOND</handle><name>Synthetic Second</name><parentOrgHandle>TEST-HOSTING</parentOrgHandle><iso3166-1><code2>US</code2></iso3166-1></org>
<net><handle>TEST-SECOND-NET</handle><orgHandle>TEST-SECOND</orgHandle><netBlocks><netBlock><type>DA</type><cidrLength>24</cidrLength><startAddress>192.0.2.0</startAddress><endAddress>192.0.2.255</endAddress></netBlock></netBlocks></net>`
	writeTestInput(t, fixture.source, []byte(strings.Replace(testCountryArinSource, "</bulkwhois>", second+"</bulkwhois>", 1)))
	if _, err := fixture.build(t, "partial-authority"); err == nil {
		t.Fatal("one incomparable owner silently waived the whole prefix")
	}
	fixture.rules.CountryRules[0].Owners = append(fixture.rules.CountryRules[0].Owners, countryEvidenceOwner{NetHandle: "TEST-SECOND-NET", OrgHandle: "TEST-SECOND"})
	output, err := fixture.build(t, "complete-authority")
	if err != nil {
		t.Fatal(err)
	}
	checkCountryEvidenceDatabase(t, output, func(i int, record countryEvidenceTestRecord) {
		if record.Risk != (i >= 192) {
			t.Fatalf("offset %d lost complete owner evidence: %+v", i, record)
		}
	})
}

// Freshness uses the exact build instant, and corrupt snapshots never fall back.
func TestArinCountryEvidenceRejectsStaleChangedAndEscapingSources(t *testing.T) {
	for _, change := range []struct {
		name string
		edit func(*countryEvidenceFixture)
	}{
		{name: "expiry-equality", edit: func(f *countryEvidenceFixture) { f.rules.CountrySources[0].ExpiresAt = f.buildTime }},
		{name: "past-expiry", edit: func(f *countryEvidenceFixture) {
			f.rules.CountrySources[0].ExpiresAt = f.buildTime.Add(-time.Nanosecond)
		}},
		{name: "future-observation", edit: func(f *countryEvidenceFixture) {
			f.rules.CountrySources[0].ObservedAt = f.buildTime.Add(time.Nanosecond)
		}},
		{name: "digest-mismatch", edit: func(f *countryEvidenceFixture) {
			writeTestInput(t, filepath.Join(f.directory, "geography.csv"), []byte("truncated"))
		}},
		{name: "missing-file", edit: func(f *countryEvidenceFixture) { f.rules.CountrySources[0].File = "absent.csv" }},
		{name: "parent-path", edit: func(f *countryEvidenceFixture) { f.rules.CountrySources[0].File = "../outside.csv" }},
		{name: "absolute-path", edit: func(f *countryEvidenceFixture) {
			f.rules.CountrySources[0].File = filepath.Join(f.directory, "geography.csv")
		}},
		{name: "fifo", edit: func(f *countryEvidenceFixture) {
			if err := syscall.Mkfifo(filepath.Join(f.directory, "pipe.csv"), 0o600); err != nil {
				t.Fatal(err)
			}
			f.rules.CountrySources[0].File = "pipe.csv"
		}},
		{name: "symlink-escape", edit: func(f *countryEvidenceFixture) {
			outside := writeTestInput(t, filepath.Join(t.TempDir(), "outside.csv"), []byte("192.0.2.128/26,CA,,,\n"))
			if err := os.Symlink(outside, filepath.Join(f.directory, "escape.csv")); err != nil {
				t.Fatal(err)
			}
			f.rules.CountrySources[0].File = "escape.csv"
		}},
		{name: "oversize", edit: func(f *countryEvidenceFixture) {
			file, err := os.OpenFile(filepath.Join(f.directory, "large.csv"), os.O_CREATE|os.O_RDWR, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			if err := file.Truncate(maxCountryEvidenceSourceBytes + 1); err != nil {
				file.Close()
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			f.rules.CountrySources[0].File = "large.csv"
		}},
	} {
		fixture := newCountryEvidenceFixture(t)
		change.edit(fixture)
		output, err := fixture.build(t, "rejected")
		if err == nil {
			t.Fatalf("%s: invalid source was published", change.name)
		}
		if _, err := os.Stat(output); !os.IsNotExist(err) {
			t.Fatalf("%s: invalid source left a visible release", change.name)
		}
	}
	fixture := newCountryEvidenceFixture(t)
	fixture.rules.CountrySources[0].ObservedAt = fixture.buildTime
	fixture.rules.CountrySources[0].ExpiresAt = fixture.buildTime.Add(time.Nanosecond)
	if _, err := fixture.build(t, "exact-fresh-boundary"); err != nil {
		t.Fatal(err)
	}
}

// Repeated inputs and the same build instant produce identical bytes and hashes.
func TestArinCountryEvidenceManifestAndReplay(t *testing.T) {
	fixture := newCountryEvidenceFixture(t)
	var previous []byte
	for i := range 2 {
		output, err := fixture.build(t, fmt.Sprintf("release-%d", i))
		if err != nil {
			t.Fatal(err)
		}
		content, err := os.ReadFile(filepath.Join(output, "arin.mmdb"))
		if err != nil {
			t.Fatal(err)
		}
		if i != 0 && !bytes.Equal(previous, content) {
			t.Fatal("identical country evidence did not reproduce database bytes")
		}
		previous = content
		manifestBytes, err := os.ReadFile(filepath.Join(output, "manifest.json"))
		if err != nil {
			t.Fatal(err)
		}
		var manifest struct {
			CountryPolicyVersion uint32                  `json:"country_policy_version"`
			Inputs               map[string]string       `json:"inputs_sha256"`
			Sources              []countryEvidenceSource `json:"country_evidence_sources"`
			BuiltAt              time.Time               `json:"built_at"`
		}
		if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
			t.Fatal(err)
		}
		if manifest.CountryPolicyVersion != 2 || len(manifest.Inputs) != 4 || len(manifest.Sources) != 1 ||
			manifest.Inputs["country_evidence/synthetic-geography"] != fixture.rules.CountrySources[0].Sha256 || !manifest.BuiltAt.Equal(fixture.buildTime) {
			t.Fatal("country evidence lost its source digest, policy version or build time")
		}
	}
}
