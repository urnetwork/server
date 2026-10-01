// URL policy is validated once with the authoritative catalog and served intact.
package controller

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
	"gopkg.in/yaml.v3"
)

func TestQualityProbeCatalogCarriesExplicitUrlPolicy(t *testing.T) {
	sites, err := testParseProviderEgressSites(`
url_probe_policy:
  version: 1
  max_redirects: 5
  max_body_bytes: 1048576
  max_ttfb_ms: 2000
  min_throughput_bps: 100000
  min_throughput_bytes: 16384
`)
	if err != nil {
		t.Fatal(err)
	}
	if sites.UrlProbePolicy != egresshealth.DefaultUrlProbePolicy() {
		t.Fatalf("policy changed in catalog parse: %+v", sites.UrlProbePolicy)
	}
	encoded, err := json.Marshal(&egresshealth.Pool{UrlProbePolicy: &sites.UrlProbePolicy})
	if err != nil {
		t.Fatal(err)
	}
	var served egresshealth.Pool
	if err := json.Unmarshal(encoded, &served); err != nil || served.UrlProbePolicy == nil || *served.UrlProbePolicy != sites.UrlProbePolicy {
		t.Fatal("policy did not survive the wire")
	}
	_, err = testParseProviderEgressSites("url_probe_policy: {version: 99}\n")
	if err == nil {
		t.Fatal("unknown success policy version silently accepted")
	}
}

// Optional operator readback validates the actual file without publishing its
// destinations. Ordinary tests use synthetic catalogs and never read deployment config.
func TestQualityProbeCatalogFromConfiguredPath(t *testing.T) {
	path := os.Getenv("FP2_QUALITY_CATALOG_PATH")
	if path == "" {
		t.Skip("set FP2_QUALITY_CATALOG_PATH for explicit local catalog readback")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("configured catalog could not be read")
	}
	sites, err := ParseProviderEgressSites(func(out any) error { return yaml.Unmarshal(data, out) })
	if err != nil {
		t.Fatal("configured catalog failed production validation")
	}
	countryUrls := 0
	for _, destinations := range sites.Countries {
		countryUrls += len(destinations)
	}
	for _, destination := range sites.Destinations {
		if destination.Expect == "status" && (destination.Status == 204 || destination.Status == 205) {
			t.Fatal("configured catalog still contains a bodyless legacy success contract")
		}
	}
	if err := sites.UrlProbePolicy.Validate(); err != nil {
		t.Fatal("configured URL policy failed validation")
	}
	t.Logf("catalog valid: general=%d countries=%d country_urls=%d policy_version=%d", len(sites.Destinations), len(sites.Countries), countryUrls, sites.UrlProbePolicy.Version)
}
