package controller

import (
	"os"
	"strings"
	"testing"
)

// A country_settings key that is no two-letter code after case folding, or
// that repeats a country another spelling already set, is refused alone and
// named under country_settings; the rest of the file, and the other
// countries' settings, stand. A setting that applies to no country must never
// stop URL measurement fleet-wide.
func TestUdpCountrySwitchCatalogRefusesABadKeyAlone(t *testing.T) {
	sites, pool, raw := udpCountryTestServe(t, udpCountryTestCatalog+`country_settings:
  iran:
    udp_probes: false
  IR:
    udp_probes: false
  ir:
    udp_probes: true
  tm:
    udp_probes: false
`)
	if len(sites.Destinations) != 2 || !pool.UdpProbesDisabled("ir") || !pool.UdpProbesDisabled("tm") {
		t.Fatalf("the file did not stand: %s", raw)
	}
	if len(sites.CountrySettings) != 2 || strings.Contains(raw, "iran") {
		t.Fatalf("served country settings %+v", sites.CountrySettings)
	}
	if len(sites.Refused) != 2 {
		t.Fatalf("refused %v, want iran and the repeated ir", sites.Refused)
	}
	// keys in order: IR sets ir, ir repeats it, iran is no code, tm sets tm
	for i, want := range []string{
		`country_settings entry "ir" repeats country ir, which "IR" already sets`,
		`country_settings entry "iran" is not a two-letter country code, so its settings apply to no country`,
	} {
		refusal := sites.Refused[i]
		if refusal.Reason != ProviderEgressRefusalCountryInvalid || refusal.List != "country_settings" || refusal.Country != "" || refusal.String() != want {
			t.Errorf("refusal %d = %+v (%s), want %s", i, refusal, refusal, want)
		}
	}
	// a value that is not a boolean is a catalog error like any other
	if _, err := testParseProviderEgressSites("country_settings:\n  ir:\n    udp_probes: maybe\n"); err == nil {
		t.Fatal("udp_probes: maybe was accepted")
	}
}

// The deployed or proposed file's country settings, as served, with
// FP2_QUALITY_CATALOG_PATH naming config/<env>/qualityprobe.yml like the other
// readbacks: none of its country_settings entries may be refused.
func TestQualityProbeCatalogFromConfiguredPathServesCountrySettings(t *testing.T) {
	path := os.Getenv("FP2_QUALITY_CATALOG_PATH")
	if path == "" {
		t.Skip("set FP2_QUALITY_CATALOG_PATH for explicit local catalog readback")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("configured catalog could not be read")
	}
	sites, pool, _ := udpCountryTestServe(t, string(data))
	for _, refusal := range sites.Refused {
		if refusal.List == "country_settings" {
			t.Errorf("refused: %s", refusal)
		}
	}
	disabled := []string{}
	for _, country := range providerEgressCountryCodes(pool.CountrySettings) {
		if pool.UdpProbesDisabled(country) {
			disabled = append(disabled, country)
		}
	}
	t.Logf("served country settings: udp_probes off in %v", disabled)
}
