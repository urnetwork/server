package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/maxmind/mmdbwriter/mmdbtype"
	mmdb "github.com/oschwald/maxminddb-golang/v2"
)

func TestHostingPrefixesParseEachPublishedFormat(t *testing.T) {
	for _, tc := range []struct {
		name     string
		source   hostingPrefixSource
		document string
		want     string
		skipped  int
	}{
		{"aws selects services", hostingPrefixSource{Format: hostingFormatAwsIpRanges, Services: []string{"EC2"}}, `{"syncToken":"1","createDate":"2026-10-04-10-00-00","prefixes":[{"ip_prefix":"192.0.2.0/25","region":"us-east-1","service":"EC2","network_border_group":"us-east-1-wl1-mia-wlz-1"},{"ip_prefix":"192.0.2.128/25","service":"CLOUDFRONT"},{"ip_prefix":"10.0.0.0/8","service":"EC2"}],"ipv6_prefixes":[{"ipv6_prefix":"2001:db8:1::/48","service":"EC2"}]}`, "[192.0.2.0/25 2001:db8:1::/48]", 1},
		{"google cloud", hostingPrefixSource{Format: hostingFormatGcpCloud}, `{"syncToken":"1","creationTime":"2026-10-04T07:06:03","prefixes":[{"ipv4Prefix":"198.51.100.0/24","service":"Google Cloud","scope":"us-east1"},{"ipv6Prefix":"2001:db8:2::/48"}]}`, "[198.51.100.0/24 2001:db8:2::/48]", 0},
		{"azure selected tag with bom", hostingPrefixSource{Format: hostingFormatAzureTags, Services: []string{"AzureCloud"}}, "\ufeff" + `{"changeNumber":420,"cloud":"Public","values":[{"name":"AzureCloud","properties":{"addressPrefixes":["203.0.113.0/24","fe80::/64"]}},{"name":"Storage","properties":{"addressPrefixes":["192.0.2.0/24"]}}]}`, "[203.0.113.0/24]", 1},
		{"oracle", hostingPrefixSource{Format: hostingFormatOracleRanges}, `{"last_updated_timestamp":"2026-08-25T08:06:24","regions":[{"region":"x","cidrs":[{"cidr":"192.0.2.64/26","tags":["OCI"]}],"ipv6_cidrs":[{"cidr":"2001:db8:3::/48","tags":["OCI"]}]}]}`, "[192.0.2.64/26 2001:db8:3::/48]", 0},
		{"geofeed skips 6to4 and teredo", hostingPrefixSource{Format: hostingFormatGeofeed}, "# Vultr.com GeoFeed\n192.0.2.0/24,US,US-NJ,Piscataway,08854\n2002::/16,US,,,\n2001::/32,US,,,\n2001:2::/48,US,,,\n", "[192.0.2.0/24]", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prefixes, skipped, err := readHostingPrefixes(t.Context(), tc.source, strings.NewReader(tc.document))
			if err != nil || fmt.Sprint(prefixes) != tc.want || skipped != tc.skipped {
				t.Fatalf("prefixes=%v skipped=%d err=%v", prefixes, skipped, err)
			}
		})
	}
	for name, tc := range map[string]struct {
		source   hostingPrefixSource
		document string
	}{
		"aws no create date":     {hostingPrefixSource{Format: hostingFormatAwsIpRanges, Services: []string{"EC2"}}, `{"prefixes":[{"ip_prefix":"192.0.2.0/24","service":"EC2"}]}`},
		"aws nothing selected":   {hostingPrefixSource{Format: hostingFormatAwsIpRanges, Services: []string{"EC2"}}, `{"createDate":"x","prefixes":[{"ip_prefix":"192.0.2.0/24","service":"S3"}]}`},
		"azure missing tag":      {hostingPrefixSource{Format: hostingFormatAzureTags, Services: []string{"AzureCloud"}}, `{"changeNumber":1,"values":[{"name":"Storage","properties":{"addressPrefixes":["192.0.2.0/24"]}}]}`},
		"malformed geofeed":      {hostingPrefixSource{Format: hostingFormatGeofeed}, "not-a-prefix,US\n"},
		"truncated google":       {hostingPrefixSource{Format: hostingFormatGcpCloud}, `{"creationTime":"x","prefixes":[`},
		"only non-global oracle": {hostingPrefixSource{Format: hostingFormatOracleRanges}, `{"last_updated_timestamp":"x","regions":[{"cidrs":[{"cidr":"10.0.0.0/8"}]}]}`},
	} {
		if _, _, err := readHostingPrefixes(t.Context(), tc.source, strings.NewReader(tc.document)); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	for _, source := range []hostingPrefixSource{
		{Format: hostingFormatAwsIpRanges, Reason: "r"},
		{Format: hostingFormatGcpCloud, Services: []string{"x"}, Reason: "r"},
		{Format: hostingFormatGeofeed},
		{Format: "cloudflare-ips", Reason: "r"},
		{Format: hostingFormatAzureTags, Services: []string{" AzureCloud"}, Reason: "r"},
	} {
		if validHostingPrefixSource(source) {
			t.Fatalf("invalid hosting source accepted: %+v", source)
		}
	}
}

// A cloud prefix inside an identified access network loses the inferred
// approval at exactly that prefix; a direct reviewed approval becomes a
// conflict; exclusions and risk are preserved; neighbours are untouched.
func TestHostingPrefixesExcludeCloudInsideAccessNetworks(t *testing.T) {
	aws := []byte(`{"syncToken":"1","createDate":"2026-10-04-10-00-00","prefixes":[{"ip_prefix":"192.0.2.0/27","region":"us-east-1","service":"EC2","network_border_group":"us-east-1-wl1-mia-wlz-1"},{"ip_prefix":"192.0.2.224/29","service":"EC2"},{"ip_prefix":"192.0.2.232/29","service":"EC2"},{"ip_prefix":"192.0.2.96/27","service":"CLOUDFRONT"}],"ipv6_prefixes":[]}`)
	feed := []byte("192.0.2.64/28,US,US-NJ,Piscataway,08854\n2002::/16,US,,,\n")
	catalog := subscriberFixtureCatalogHeader + `hosting_prefix_sources:
  - id: aws-ec2
    url: https://ip-ranges.amazonaws.com/ip-ranges.json
    file: aws.json
    sha256: SHA256_AWS_JSON
    observed_at: OBSERVED
    expires_at: EXPIRES
    format: aws-ip-ranges-json
    services: [EC2]
    reason: AWS-published EC2 ranges
  - id: vultr
    url: https://geofeed.constant.com/
    file: vultr.csv
    sha256: SHA256_VULTR_CSV
    observed_at: OBSERVED
    expires_at: EXPIRES
    format: rfc8805-geofeed
    reason: Vultr-published geofeed
` + subscriberFixtureOperators
	fixture := newSubscriberBuildFixture(t, "64500 192.0.2.0/24 40\n", map[string][]byte{"aws.json": aws, "vultr.csv": feed}, catalog)
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
		address, state, source string
		risk                   bool
	}{
		{"192.0.2.1", "excluded", "aws-ec2", false}, {"192.0.2.31", "excluded", "aws-ec2", false}, {"192.0.2.32", "subscriber", "", false},
		{"192.0.2.70", "excluded", "vultr", false}, {"192.0.2.80", "subscriber", "", false},
		{"192.0.2.100", "subscriber", "", false},
		{"192.0.2.225", "excluded", "aws-ec2", false}, {"192.0.2.233", "excluded", "aws-ec2", true},
	} {
		record := lookupSubscriberRecord(t, db, tc.address)
		ids, _ := record["hosting_prefix_source_ids"].(mmdbtype.Slice)
		if record["quality_state"] != mmdbtype.String(tc.state) || record["risk"] != mmdbtype.Bool(tc.risk) || (tc.source == "") != (len(ids) == 0) || tc.source != "" && ids[0] != mmdbtype.String(tc.source) {
			t.Fatalf("%s: %+v", tc.address, record)
		}
		if tc.source != "" && (record["subscriber_evidence_kind"] != nil || record["network_risk_evidence"] != nil && !tc.risk) {
			t.Fatalf("%s: hosting kept the inference or invented risk: %+v", tc.address, record)
		}
		if _, err := augmentSubscriberRecord(record, nil); tc.source != "" && err == nil {
			t.Fatal("hosting prefixes were accepted as an unaugmented base")
		}
	}
	manifest, err := os.ReadFile(filepath.Join(out, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Entries int               `json:"hosting_prefix_entries"`
		Skipped int               `json:"hosting_prefix_skipped_non_global"`
		Inputs  map[string]string `json:"inputs_sha256"`
	}
	if err := json.Unmarshal(manifest, &parsed); err != nil || parsed.Entries != 4 || parsed.Skipped != 1 || parsed.Inputs["hosting_prefix/aws-ec2"] != fixtureSha256(aws) {
		t.Fatalf("manifest lacks hosting provenance: %s", manifest)
	}
	direct := hostingPrefixRecord(subscriberFixtureRecord("subscriber", false), hostingPrefixSource{countryEvidenceSource: countryEvidenceSource{Id: "aws-ec2"}})
	if direct["quality_state"] != mmdbtype.String("ambiguous") || direct["non_quality"] != mmdbtype.Bool(true) {
		t.Fatalf("a direct reviewed approval silently lost to a hosting prefix: %+v", direct)
	}
	for _, state := range []string{"excluded", "ambiguous"} {
		if kept := hostingPrefixRecord(subscriberFixtureRecord(state, false), hostingPrefixSource{}); kept["quality_state"] != mmdbtype.String(state) {
			t.Fatalf("%s changed to %v", state, kept["quality_state"])
		}
	}
	p := writeTestInput(t, filepath.Join(t.TempDir(), "catalog.yml"), []byte(strings.Replace(fixture.catalogText, "    services: [EC2]\n", "", 1)))
	if _, err := loadSubscriberOriginCatalog(p); err == nil {
		t.Fatal("AWS hosting source without a reviewed service selection accepted")
	}
}

func TestSubscriberEvidenceRefreshPinsHostingPrefixesAndResolvesAzure(t *testing.T) {
	at := time.Date(2026, 10, 4, 16, 0, 0, 0, time.UTC)
	bodies := evidenceFixtureBodies(t, at)
	azure := "https://download.microsoft.com/download/7/1/d/71d86715-5596-4529-9b13-da13a5de5b63/ServiceTags_Public_20260928.json"
	bodies[subscriberEvidenceAzurePage] = []byte(`<html><a href="https://example.com/ServiceTags_Public_20260928.json">x</a><a href="` + azure + `">Download</a></html>`)
	bodies[azure] = []byte(`{"changeNumber":420,"values":[{"name":"AzureCloud","properties":{"addressPrefixes":["203.0.113.0/24"]}}]}`)
	bodies[subscriberEvidenceAwsRanges] = []byte(`{"createDate":"2026-10-04-10-00-00","prefixes":[{"ip_prefix":"192.0.2.0/27","service":"EC2"}],"ipv6_prefixes":[]}`)
	bodies[subscriberEvidenceGcpRanges] = []byte(`{"creationTime":"2026-10-04T07:06:03","prefixes":[{"ipv4Prefix":"198.51.100.0/24"}]}`)
	bodies[subscriberEvidenceOracleRanges] = []byte(`{"last_updated_timestamp":"2026-08-25T08:06:24","regions":[{"cidrs":[{"cidr":"192.0.2.64/26"}]}]}`)
	for _, url := range []string{subscriberEvidenceDoGeofeed, subscriberEvidenceLinodeGeofeed, subscriberEvidenceVultrGeofeed} {
		bodies[url] = []byte("192.0.2.128/26,US,US-NJ,Piscataway,\n")
	}
	transport := &evidenceRoundTripper{bodies: bodies}
	out := filepath.Join(t.TempDir(), "refreshed")
	if err := publishDirectory(out, func(stage string) error {
		return refreshSubscriberEvidence(t.Context(), "", subscriberEvidenceOptions{HostingPrefixes: true}, stage, &http.Client{Transport: transport}, at)
	}); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(out, "evidence.yml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"hosting_prefix_sources:", "url: " + azure, "format: oracle-public-ip-ranges-json", "id: vultr", "- AzureCloud"} {
		if !strings.Contains(string(content), want) {
			t.Fatalf("evidence fragment lacks %q: %s", want, content)
		}
	}
	bodies[subscriberEvidenceAzurePage] = []byte(`<html><a href="https://evil.example/download/ServiceTags_Public_20260928.json">x</a></html>`)
	if err := publishDirectory(filepath.Join(t.TempDir(), "bad"), func(stage string) error {
		return refreshSubscriberEvidence(t.Context(), "", subscriberEvidenceOptions{HostingPrefixes: true}, stage, &http.Client{Transport: transport}, at)
	}); err == nil {
		t.Fatal("an Azure link outside download.microsoft.com was followed")
	}
}
