package controller

import (
	"encoding/json"
	"math/rand"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
)

// The per-country UDP switch (owner, 2026-11-11): qualityprobe.yml
// country_settings <cc>: udp_probes: false turns the HTTP/3 legs off for exits
// in a country whose national firewall makes UDP results say nothing about
// the provider. The key is optional, and every country defaults to on.
const udpCountryTestCatalog = `schema_version: 1
destinations:
  - {name: synthetic-h3-site, class: site, category: news, url: "https://h3.example/", h3: true}
  - {name: synthetic-tcp-site, class: site, category: video, url: "https://tcp.example/"}
`

// Parses text and serves the pool a prober would load from it, every
// destination active.
func udpCountryTestServe(t *testing.T, text string) (*ProviderEgressSites, *egresshealth.Pool, string) {
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
	raw, err := json.Marshal(pool)
	if err != nil {
		t.Fatal(err)
	}
	return sites, pool, string(raw)
}

// The catalog parses with the field absent, with only default entries, and
// with countries spelled in lower and upper case: a country is read in either
// case, as the prober matches a provider's country, and only an explicit
// udp_probes: false is served, under its lower-case code. Without one, the
// pool and its version are what they were before the field existed.
func TestUdpCountrySwitchCatalogParsesEitherCaseAndAbsence(t *testing.T) {
	absentSites, absent, absentRaw := udpCountryTestServe(t, udpCountryTestCatalog)
	if strings.Contains(absentRaw, "country_settings") || len(absentSites.Refused) != 0 {
		t.Fatalf("absent: the pool serves %s, refused %v", absentRaw, absentSites.Refused)
	}

	defaultsSites, defaults, defaultsRaw := udpCountryTestServe(t, udpCountryTestCatalog+`country_settings:
  de:
    udp_probes: true
  fr: {}
`)
	if strings.Contains(defaultsRaw, "country_settings") || defaults.Version != absent.Version || len(defaultsSites.Refused) != 0 {
		t.Fatalf("defaults only: the pool serves %s at version %d (absent: %d), refused %v",
			defaultsRaw, defaults.Version, absent.Version, defaultsSites.Refused)
	}

	switchedSites, switched, switchedRaw := udpCountryTestServe(t, udpCountryTestCatalog+`country_settings:
  ir:
    udp_probes: false
  CN:
    udp_probes: false
  "Tm":
    udp_probes: false
  de:
    udp_probes: true
`)
	const want = `"country_settings":{"cn":{"udp_probes":false},"ir":{"udp_probes":false},"tm":{"udp_probes":false}}`
	if !strings.Contains(switchedRaw, want) || len(switchedSites.Refused) != 0 {
		t.Fatalf("lower and upper case: the pool serves %s, want %s; refused %v", switchedRaw, want, switchedSites.Refused)
	}
	if switched.Version == absent.Version {
		t.Fatal("turning a country's UDP probes off must change the pool version")
	}
	// the general pool is unchanged: the h3 URL keeps its h3 for everyone
	for _, destination := range switched.Destinations {
		if destination.Name == "synthetic-h3-site" && !destination.H3 {
			t.Fatal("the h3 URL lost its h3")
		}
	}
}
