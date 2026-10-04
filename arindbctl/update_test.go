package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/maxmind/mmdbwriter/mmdbtype"
	mmdb "github.com/oschwald/maxminddb-golang/v2"
)

const updateFixtureCatalog = `version: 1
policy: identified-subscriber-default
minimum_origin_peers: 3
origin_sources:
  - id: previous-ris
    url: https://www.ris.ripe.net/dumps/riswhoisdump.IPv4.gz
    file: sources/riswhoisdump.IPv4.gz
    sha256: 0000000000000000000000000000000000000000000000000000000000000000
    observed_at: 2026-09-01T00:00:00Z
    expires_at: 2026-09-02T00:00:00Z
operators:
  - {id: access, name: Synthetic Subscriber ISP, asns: [64500], usage: subscriber, source: 'https://evidence.example/access', countries: [US]}
  - {id: host, name: Synthetic Hosting, asns: [64503], usage: hosting, source: 'https://evidence.example/hosting', countries: [GB]}
`

const updateFixtureAtlas = `{"objects":[
{"id":1,"address_v4":"198.51.100.10","asn_v4":64500,"status_name":"Connected","is_anchor":false,"is_public":true,"tags":["home","system-ipv4-works"],"country_code":"US"},
{"id":2,"address_v4":"198.51.100.11","asn_v4":64500,"status_name":"Connected","is_anchor":false,"is_public":true,"tags":["fibre"],"country_code":"US"},
{"id":3,"address_v4":"192.0.2.10","asn_v4":64503,"status_name":"Connected","is_anchor":false,"is_public":true,"tags":["datacentre"],"country_code":"US"},
{"id":4,"address_v4":"203.0.113.5","asn_v4":64999,"status_name":"Connected","is_anchor":false,"is_public":true,"tags":["home"],"country_code":"US"},
{"id":5,"address_v4":"198.51.100.200","asn_v4":64500,"status_name":"Disconnected","is_anchor":false,"is_public":true,"tags":["datacentre"],"country_code":"US"},
{"id":6,"address_v6":"2001:db8::1","asn_v6":64500,"status_name":"Connected","is_anchor":true,"is_public":true,"tags":["home"],"country_code":"US"},
{"id":7,"address_v4":"192.0.2.70","asn_v4":64500,"status_name":"Connected","is_anchor":false,"is_public":true,"tags":["vps"],"country_code":"US"}
],"meta":{"total_count":6}}`

type updateFixture struct {
	dir, credentials, rules, catalog string
	archive                          []byte
	bodies                           map[string][]byte
	at                               time.Time
}

func newUpdateFixture(t *testing.T) *updateFixture {
	t.Helper()
	dir := t.TempDir()
	at := time.Date(2026, 10, 4, 16, 0, 0, 0, time.UTC)
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
	bodies := evidenceFixtureBodies(t, at)
	// 198.51.100.0/24 has no ARIN allocation in the fixture, so the identified
	// ISP's route there is inferred; a VPN server sits inside it.
	bodies[subscriberEvidenceRisIPv4Url] = subscriberFixtureGzip(t, at.Add(-2*time.Hour), "64500 192.0.2.0/24 40\n64500 198.51.100.0/24 40\n64503 203.0.113.128/25 40\n")
	bodies[subscriberEvidenceMullvad] = []byte(`[{"ipv4_addr_in":"198.51.100.77","ipv6_addr_in":""}]`)
	bodies[subscriberEvidenceNordvpn] = []byte(`[{"station":"198.51.100.78","ips":[]}]`)
	bodies[subscriberEvidencePia] = []byte(`{"regions":[{"servers":{"wg":[{"ip":"198.51.100.79"}]}}]}` + "\nsig\n")
	bodies[subscriberEvidenceWindscribe] = []byte(`{"data":[{"groups":[{"nodes":[{"ip":"198.51.100.80"}]}]}]}`)
	bodies[subscriberEvidenceAwsRanges] = []byte(`{"createDate":"2026-10-04-10-00-00","prefixes":[{"ip_prefix":"198.51.100.128/26","service":"EC2"}],"ipv6_prefixes":[]}`)
	for _, d := range subscriberEvidenceRegistryAssignments {
		bodies[d.url] = gzipFixture(t, "inetnum: 198.51.100.192 - 198.51.100.223\nnetname: EXAMPLE-HOSTING\n\n")
	}
	bodies[subscriberEvidenceAtlasProbes] = []byte(updateFixtureAtlas)
	return &updateFixture{
		dir:         dir,
		credentials: writeTestInput(t, filepath.Join(dir, "arin.yml"), []byte("api_key: SYNTHETIC_TEST_ONLY\n")),
		rules:       writeTestInput(t, filepath.Join(dir, "rules.yml"), []byte(strings.Replace(testArinRules, "version: 1\n", "version: 1\nquality_policy_version: 2\n", 1))),
		catalog:     writeTestInput(t, filepath.Join(dir, "catalog.yml"), []byte(updateFixtureCatalog)),
		archive:     archive.Bytes(),
		bodies:      bodies,
		at:          at,
	}
}

func (self *updateFixture) run(t *testing.T, output string, catalog string) error {
	t.Helper()
	dependencies := commandDependencies{
		geoipUpdate: func(_ context.Context, _ string, directory string) error {
			return os.WriteFile(filepath.Join(directory, "GeoLite2-City.mmdb"), testGeoDatabase(t), 0o600)
		},
		arinClient: &http.Client{Transport: testRoundTripper(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(self.archive)), Header: make(http.Header)}, nil
		})},
		evidenceClient: &http.Client{Transport: &evidenceRoundTripper{bodies: self.bodies}},
		now:            func() time.Time { return self.at },
	}
	args := []string{"update", "--geoip-config", filepath.Join(self.dir, "GeoIP.conf"), "--credentials", self.credentials, "--rules", self.rules, "--output", output}
	if catalog != "" {
		args = append(args, "--subscriber-catalog", catalog)
	}
	return runCommandWithDependencies(t.Context(), args, io.Discard, dependencies)
}

// Every data set is refreshed; optional sources that fail are left out and
// named; the final resource is augmented, audited and validated.
func TestUpdateRefreshesEveryDataSetBestEffort(t *testing.T) {
	fixture := newUpdateFixture(t)
	// Label sources and Azure are unavailable upstream in this run.
	output := filepath.Join(fixture.dir, "bundle")
	if err := fixture.run(t, output, fixture.catalog); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"mmdb/geolite2.mmdb", "mmdb/manifest.json", "arindb/arin.mmdb", "arindb/manifest.json", "arindb/registration-manifest.json", "arindb/update-manifest.json",
		"subscriber-evidence/registration/arin.mmdb", "subscriber-evidence/catalog/catalog.yml", "subscriber-evidence/catalog/sources/riswhoisdump.IPv4.gz",
		"subscriber-evidence/catalog/sources/ripe.db.inetnum.gz", "subscriber-evidence/audit/catalog-audit.json", "subscriber-evidence/validation/validation.json", "update-summary.txt"} {
		if _, err := os.Stat(filepath.Join(output, name)); err != nil {
			t.Fatal(err)
		}
	}
	// The resource directory holds only what the runtime and auditors need.
	entries, err := os.ReadDir(filepath.Join(output, "arindb"))
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if !slices.Equal(names, []string{"arin.mmdb", "manifest.json", "registration-manifest.json", "update-manifest.json"}) {
		t.Fatalf("arindb resource contents: %v", names)
	}
	var manifest struct {
		Augmentation string              `json:"subscriber_augmentation"`
		Unavailable  []updateUnavailable `json:"unavailable_evidence"`
		Validation   map[string]any      `json:"validation"`
		Audit        string              `json:"catalog_audit"`
		Catalog      map[string]string   `json:"subscriber_catalog"`
	}
	content, err := os.ReadFile(filepath.Join(output, "arindb", "update-manifest.json"))
	if err != nil || json.Unmarshal(content, &manifest) != nil {
		t.Fatalf("update manifest: %v %s", err, content)
	}
	unavailable := []string{}
	for _, source := range manifest.Unavailable {
		unavailable = append(unavailable, source.Id)
	}
	for _, want := range []string{"azure-cloud", "asdb", "apnic-aspop", "bgp-tools-asns", "linnaeus-labels", "google-cloud"} {
		if !slices.Contains(unavailable, want) {
			t.Fatalf("unavailable sources %v lack %s", unavailable, want)
		}
	}
	for _, pinned := range []string{"aws-ec2", "ris-ipv4", "tor-exit-addresses", "mullvad-relays", "ripe-inetnum", "caida-as2org"} {
		if slices.Contains(unavailable, pinned) {
			t.Fatalf("available source %s reported unavailable", pinned)
		}
	}
	if manifest.Augmentation != "applied" || manifest.Validation["status"] != "completed" || manifest.Audit != "subscriber-evidence/audit/catalog-audit.json" || manifest.Catalog["sha256"] != fixtureSha256([]byte(updateFixtureCatalog)) {
		t.Fatalf("update manifest: %s", content)
	}
	db, err := mmdb.Open(filepath.Join(output, "arindb", "arin.mmdb"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, tc := range []struct{ address, state string }{
		{"198.51.100.10", "subscriber"}, {"198.51.100.77", "excluded"}, {"198.51.100.130", "excluded"},
		{"198.51.100.200", "unknown"}, {"192.0.2.10", "excluded"}, {"192.0.2.70", "subscriber"}, {"203.0.113.5", "unknown"},
	} {
		if record := lookupSubscriberRecord(t, db, tc.address); record["quality_state"] != mmdbtype.String(tc.state) {
			t.Fatalf("%s: %+v", tc.address, record)
		}
	}
	var validation struct {
		Precision   wilsonEstimate       `json:"clean_precision"`
		Recall      wilsonEstimate       `json:"residential_recall"`
		Datacentre  wilsonEstimate       `json:"datacentre_clean_rate"`
		Residential int                  `json:"residential_networks"`
		NotClean    map[string]int       `json:"residential_not_clean"`
		False       []atlasFalseApproval `json:"datacentre_false_approvals"`
	}
	content, err = os.ReadFile(filepath.Join(output, "subscriber-evidence", "validation", "validation.json"))
	if err != nil || json.Unmarshal(content, &validation) != nil {
		t.Fatalf("validation: %v %s", err, content)
	}
	// Probes 1 and 2 share 198.51.100.0/24 and count once; probe 4 has no
	// identified operator; the disconnected probe is ignored. The datacentre
	// /24 of probes 3 and 7 is clean through probe 7's directly approved
	// address, and the home-tagged anchor counts as a datacentre /48 inside the
	// identified ISP's inferred IPv6 space: both are false approvals.
	if validation.Residential != 2 || validation.Precision.Successes != 1 || validation.Precision.Trials != 3 || validation.Recall.Successes != 1 || validation.Recall.Trials != 2 ||
		validation.NotClean["no-identified-operator"] != 1 || validation.Datacentre.Trials != 2 || validation.Datacentre.Successes != 2 || len(validation.False) != 2 ||
		validation.False[0].Network != "192.0.2.0/24" || validation.False[1].Network != "2001:db8::/48" || validation.False[1].ASN != 64500 || validation.False[1].Evidence != "isp_inferred" {
		t.Fatalf("validation report: %s", content)
	}
	summary, err := os.ReadFile(filepath.Join(output, "update-summary.txt"))
	if err != nil || !strings.Contains(string(summary), "unavailable evidence: ") || !strings.Contains(string(summary), "Atlas clean precision") {
		t.Fatalf("update summary: %v %q", err, summary)
	}
}

func TestUpdateRequiresRoutingEvidenceAndStatesRegistrationOnlyOutput(t *testing.T) {
	fixture := newUpdateFixture(t)
	delete(fixture.bodies, subscriberEvidenceRisIPv6Url)
	failed := filepath.Join(fixture.dir, "failed")
	if err := fixture.run(t, failed, fixture.catalog); err == nil || !strings.Contains(err.Error(), "ris-ipv6") {
		t.Fatalf("update without required routing evidence: %v", err)
	}
	if _, err := os.Stat(failed); !os.IsNotExist(err) {
		t.Fatal("a failed update published a bundle")
	}
	plain := filepath.Join(fixture.dir, "plain")
	if err := fixture.run(t, plain, ""); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(plain, "arindb", "update-manifest.json"))
	if err != nil || !strings.Contains(string(content), "registration evidence only") {
		t.Fatalf("registration-only update manifest: %v %s", err, content)
	}
	if _, err := os.Stat(filepath.Join(plain, "subscriber-evidence")); !os.IsNotExist(err) {
		t.Fatal("a registration-only update produced subscriber evidence")
	}
	summary, _ := os.ReadFile(filepath.Join(plain, "update-summary.txt"))
	if !strings.Contains(string(summary), "registration only") {
		t.Fatalf("registration-only summary: %q", summary)
	}
}

func TestWilsonIntervals(t *testing.T) {
	for _, tc := range []struct {
		successes, trials int
		estimate, lower   float64
		upper             float64
	}{
		{0, 0, 0, 0, 0},
		{10, 10, 1, 0.7225, 1},
		{298, 300, 0.9933, 0.976, 0.9982},
		{0, 150, 0, 0, 0.025},
	} {
		got := wilson(tc.successes, tc.trials)
		if got.Estimate != tc.estimate || got.Lower95 != tc.lower || got.Upper95 != tc.upper {
			t.Fatalf("wilson(%d,%d) = %+v", tc.successes, tc.trials, got)
		}
	}
}
