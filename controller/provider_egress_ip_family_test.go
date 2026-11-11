package controller

import (
	"encoding/json"
	"math/rand"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/qualityprobe/egresshealth"
)

// The schema the catalog's IP family marks and country settings take
// (owner, 2026-11-11): ipv6: true, ipv4: false, and country_settings with
// udp_probes: false.
const testIpFamilyCatalog = `schema_version: 1
country_settings:
  ru:
    udp_probes: false
  de:
    udp_probes: true
  fr: {}
  zz:
    udp_probes: false
destinations:
  - {name: dual-news, class: site, category: news, url: "https://dual.example/", h3: true, ipv6: true}
  - {name: plain-video, class: site, category: video, url: "https://plain.example/"}
  - {name: ipv6-only-docs, class: site, category: docs, url: "https://v6only.example/", ipv4: false, ipv6: true}
  - {name: explicit-v4, class: site, category: shopping, url: "https://v4.example/", ipv4: true}
`

func testIpFamilyPool(t *testing.T, text string) (*ProviderEgressSites, *egresshealth.Pool) {
	t.Helper()
	sites, err := ParseProviderEgressSites(func(out any) error { return yaml.Unmarshal([]byte(text), out) })
	if err != nil {
		t.Fatalf("the parse refused the catalog: %v", err)
	}
	stored := []*model.ProviderEgressDestination{}
	for _, row := range sites.Destinations {
		copy := *row
		copy.Active = true
		stored = append(stored, &copy)
	}
	pool, err := sites.servePool(stored, time.Date(2026, time.November, 11, 12, 0, 0, 0, time.UTC), rand.New(rand.NewSource(1)))
	if err != nil {
		t.Fatalf("the parsed catalog cannot be served: %v", err)
	}
	return sites, pool
}

// The marks reach the served pool exactly as declared: ipv6: true on a URL
// with AAAA, ipv4: false only where declared, and nothing on a URL that says
// nothing (an explicit ipv4: true is the default and is not served), so an
// unmarked URL keeps the pool version it had.
func TestQualityprobeCatalogServesIpFamilyMarks(t *testing.T) {
	_, pool := testIpFamilyPool(t, testIpFamilyCatalog)
	byName := map[string]egresshealth.Destination{}
	for _, destination := range pool.Destinations {
		byName[destination.Name] = destination
	}
	if dual := byName["dual-news"]; !dual.ServesIpv6() || !dual.ServesIpv4() || !dual.H3 {
		t.Fatalf("dual-news served as %+v", dual)
	}
	if only := byName["ipv6-only-docs"]; !only.ServesIpv6() || only.ServesIpv4() {
		t.Fatalf("ipv6-only-docs served as %+v", only)
	}
	for _, name := range []string{"plain-video", "explicit-v4"} {
		raw, err := json.Marshal(byName[name])
		if err != nil || strings.Contains(string(raw), "ipv4") || strings.Contains(string(raw), "ipv6") {
			t.Fatalf("%s changed shape: %s %v", name, raw, err)
		}
	}
	// the default spelled out serves nothing new; a mark changes the version
	_, implicit := testIpFamilyPool(t, strings.Replace(testIpFamilyCatalog, ", ipv4: true}", "}", 1))
	_, unmarked := testIpFamilyPool(t, strings.Replace(testIpFamilyCatalog, `h3: true, ipv6: true}`, `h3: true}`, 1))
	if implicit.Version != pool.Version || unmarked.Version == pool.Version {
		t.Fatalf("versions: marked %d, explicit default dropped %d, ipv6 mark dropped %d", pool.Version, implicit.Version, unmarked.Version)
	}
}

// ipv4: false without ipv6: true serves the probe over no family: a catalog
// error, refused at load with the entry named, like any other.
func TestQualityprobeCatalogRefusesAUrlOfNoFamily(t *testing.T) {
	text := strings.Replace(testIpFamilyCatalog, `ipv4: false, ipv6: true}`, `ipv4: false}`, 1)
	_, err := ParseProviderEgressSites(func(out any) error { return yaml.Unmarshal([]byte(text), out) })
	if err == nil || !strings.Contains(err.Error(), "ipv6-only-docs") || !strings.Contains(err.Error(), "no IP family") {
		t.Fatalf("err=%v; want the URL of no family refused by name", err)
	}
}

// country_settings: only an explicit udp_probes: false is served, under its
// country; true and an empty entry are the default and are not served; a
// well-formed code ISO 3166-1 does not assign matches no provider and is
// ignored; and the pool version moves with the settings.
func TestQualityprobeCatalogServesCountrySettings(t *testing.T) {
	sites, pool := testIpFamilyPool(t, testIpFamilyCatalog)
	if len(sites.CountrySettings) != 1 || len(pool.CountrySettings) != 1 || !pool.UdpProbesDisabled("ru") ||
		pool.UdpProbesDisabled("de") || pool.UdpProbesDisabled("fr") || pool.UdpProbesDisabled("zz") {
		t.Fatalf("country settings served as %+v", pool.CountrySettings)
	}
	raw, err := json.Marshal(pool)
	if err != nil || !strings.Contains(string(raw), `"country_settings":{"ru":{"udp_probes":false}}`) {
		t.Fatalf("the pool's wire form: %s %v", raw, err)
	}
	none := strings.Replace(testIpFamilyCatalog, "  ru:\n    udp_probes: false\n", "", 1)
	_, without := testIpFamilyPool(t, none)
	if without.CountrySettings != nil || without.Version == pool.Version {
		t.Fatalf("without a disabled country: settings %+v, version %d (with: %d)", without.CountrySettings, without.Version, pool.Version)
	}
	if raw, _ := json.Marshal(without); strings.Contains(string(raw), "country_settings") {
		t.Fatalf("a pool with default settings serves them: %s", raw)
	}
}

// country_settings keys are read in either case. A key that is no two-letter
// code after case folding, or that repeats a country another spelling already
// set, is refused alone (country_invalid, under country_settings) and the rest
// of the file stands: one bad key must never refuse the catalog and stop URL
// probing. A well-formed code ISO 3166-1 does not assign (zz, ZZ) matches no
// provider and is ignored, refused or not. A value that is not a boolean is a
// catalog error like any other.
func TestQualityprobeCatalogRefusesInvalidCountrySettingsCodes(t *testing.T) {
	parse := func(text string) *ProviderEgressSites {
		t.Helper()
		sites, err := ParseProviderEgressSites(func(out any) error { return yaml.Unmarshal([]byte(text), out) })
		if err != nil {
			t.Fatalf("one country key refused the whole catalog: %v", err)
		}
		return sites
	}
	countryRefusals := func(sites *ProviderEgressSites) []ProviderEgressCatalogRefusal {
		refusals := []ProviderEgressCatalogRefusal{}
		for _, refusal := range sites.Refused {
			if refusal.List == "country_settings" {
				refusals = append(refusals, refusal)
			}
		}
		return refusals
	}
	for _, key := range []string{"rus", "r1", "r", `""`, "r-u"} {
		sites := parse(strings.Replace(testIpFamilyCatalog, "  ru:\n", "  "+key+":\n", 1))
		refusals := countryRefusals(sites)
		if len(refusals) != 1 || refusals[0].Reason != ProviderEgressRefusalCountryInvalid || len(sites.CountrySettings) != 0 || len(sites.Destinations) != 4 {
			t.Fatalf("%s: refusals %v, settings %+v, destinations %d; want the key refused alone", key, refusals, sites.CountrySettings, len(sites.Destinations))
		}
	}
	upper := parse(strings.Replace(testIpFamilyCatalog, "  ru:\n", "  RU:\n", 1))
	if refusals := countryRefusals(upper); len(refusals) != 0 || len(upper.CountrySettings) != 1 || upper.CountrySettings["ru"].UdpProbes == nil || *upper.CountrySettings["ru"].UdpProbes {
		t.Fatalf("RU: refusals %v, settings %+v; want it read as ru", refusals, upper.CountrySettings)
	}
	repeated := parse(strings.Replace(testIpFamilyCatalog, "  ru:\n    udp_probes: false\n", "  RU:\n    udp_probes: false\n  ru:\n    udp_probes: true\n", 1))
	refusals := countryRefusals(repeated)
	if len(refusals) != 1 || refusals[0].Name != "ru" || refusals[0].String() != `country_settings entry "ru" repeats country ru, which "RU" already sets` ||
		len(repeated.CountrySettings) != 1 || *repeated.CountrySettings["ru"].UdpProbes {
		t.Fatalf("a repeated country: refusals %v, settings %+v; want the second spelling refused alone", refusals, repeated.CountrySettings)
	}
	unassigned := parse(strings.Replace(testIpFamilyCatalog, "  zz:\n", "  ZZ:\n", 1) + "")
	if refusals := countryRefusals(unassigned); len(refusals) != 0 || len(unassigned.CountrySettings) != 1 {
		t.Fatalf("an unassigned code: refusals %v, settings %+v; want it ignored", refusals, unassigned.CountrySettings)
	}
	text := strings.Replace(testIpFamilyCatalog, "  ru:\n    udp_probes: false\n", "  ru:\n    udp_probes: maybe\n", 1)
	if _, err := ParseProviderEgressSites(func(out any) error { return yaml.Unmarshal([]byte(text), out) }); err == nil {
		t.Fatal("udp_probes: maybe was accepted")
	}
}

// What a binary from before these keys reads: the file's parse is not strict
// (gopkg.in/yaml.v3's Unmarshal, as server.SimpleResource.UnmarshalYamlE), so
// the shapes it knows decode as they always did, and country_settings, ipv6
// and ipv4 are ignored.
func TestQualityprobeCatalogIpFamilyKeysAreIgnoredByOlderBinaries(t *testing.T) {
	type entryBefore struct {
		Name     string `yaml:"name"`
		Class    string `yaml:"class"`
		Category string `yaml:"category"`
		Url      string `yaml:"url"`
		H3       bool   `yaml:"h3"`
	}
	type documentBefore struct {
		SchemaVersion int           `yaml:"schema_version"`
		Destinations  []entryBefore `yaml:"destinations"`
	}
	var before documentBefore
	if err := yaml.Unmarshal([]byte(testIpFamilyCatalog), &before); err != nil {
		t.Fatalf("an older binary's parse refused the file: %v", err)
	}
	if before.SchemaVersion != 1 || len(before.Destinations) != 4 || before.Destinations[0].Name != "dual-news" || !before.Destinations[0].H3 {
		t.Fatalf("an older binary read %+v", before)
	}
	if _, err := model.ProviderEgressSiteSettingsFromYaml(func(out any) error { return yaml.Unmarshal([]byte(testIpFamilyCatalog), out) }); err != nil {
		t.Fatalf("the settings block's parse refused the file: %v", err)
	}
}

// The place judgement of a failed IPv6 leg is the server's
// (model.JudgeProviderEgressLegs), over TCP as over UDP: an outcome that
// arrives with a verdict on either is refused before anything is read or
// stored, so no prober can buy a place verdict for its provider's IPv6.
func TestProviderEgressIngestRefusesAProberIpv6Verdict(t *testing.T) {
	for _, transport := range []string{"tcp", "udp"} {
		evidence := statsEgressTestUrlEvidence(time.Now().UTC(), true)
		evidence.Destination.Ipv6 = true
		evidence.Destination.H3 = transport == "udp"
		evidence.Security[0].Destination = evidence.Destination
		evidence.IpFamily = egresshealth.IpFamilyDualstack
		evidence.FailureStage = egresshealth.FailureStageIpv6
		evidence.Ipv6 = &egresshealth.UrlProbeIpv6Evidence{Tcp: egresshealth.UrlProbeIpv6TcpFailed, FailureStage: "dial_tcp"}
		if transport == "udp" {
			evidence.H3 = &egresshealth.UrlProbeH3Evidence{Udp: egresshealth.UrlProbeH3UdpAnswered, FailureStage: "tls"}
			evidence.Ipv6 = &egresshealth.UrlProbeIpv6Evidence{Tcp: egresshealth.UrlProbeIpv6TcpPassed, ContentClassification: evidence.ContentClassification,
				PerformanceClassification: evidence.PerformanceClassification, TlsAuthenticated: true, StatusCode: evidence.StatusCode,
				ByteCount: evidence.ByteCount, WireByteCount: evidence.WireByteCount, WireSampleByteCount: evidence.WireSampleByteCount,
				BodyComplete: evidence.BodyComplete, RequestWritten: true, FirstByteReceived: true, TtfbMillis: evidence.TtfbMillis,
				BodyMillis: evidence.BodyMillis, BodyBitsPerSecond: evidence.BodyBitsPerSecond,
				H3: &egresshealth.UrlProbeH3Evidence{Udp: egresshealth.UrlProbeH3UdpSilent, FailureStage: egresshealth.FailureStageDialQuic}}
		}
		if err := evidence.ValidateOutcome(0, 1, false); err != nil {
			t.Fatalf("%s: the failed IPv6 leg's evidence is not valid without a verdict: %v", transport, err)
		}
		for _, verdict := range []string{egresshealth.UrlProbeH3VerdictPlace, egresshealth.UrlProbeH3VerdictProvider} {
			if transport == "udp" {
				evidence.Ipv6.H3.Verdict = verdict
			} else {
				evidence.Ipv6.Verdict = verdict
			}
			args := &SubmitProviderEgressHealthArgs{
				ClientId:         server.NewId(),
				RunId:            server.NewId(),
				UrlProbeEvidence: evidence,
				OKCount:          0,
				TotalCount:       1,
				ClassResults:     map[string]ProviderEgressHealthClassResult{"site": {OK: 0, Total: 1}},
				FailedNames:      evidence.Destination.Name,
			}
			refusal := ProviderEgressIngestRefusalOf(SubmitProviderEgressHealth(t.Context(), args))
			if refusal == nil || refusal.Status != 400 || !strings.Contains(refusal.Message, "IPv6 verdict") {
				t.Fatalf("%s verdict %q: the ingest took a prober's verdict: %+v", transport, verdict, refusal)
			}
		}
	}
}
