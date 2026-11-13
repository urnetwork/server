// Disjointness of the destination catalog: the parse holds every country list
// to the rule the served pool applies, against destinations and candidates
// alike; a candidate never takes a destination's name; and whatever the parse
// accepts is served unchanged, with no candidate promoted or every one. These
// tests use only what the code before the rule had, so each also runs, and
// fails, against it; provider_egress_catalog_refusal_test.go pins the rest.
package controller

import (
	"context"
	"math/rand"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// Destinations and a candidate on reserved example domains, one registrable
// domain each.
const testDisjointGeneral = `schema_version: 1
destinations:
  - {name: general-news, class: site, category: news, url: "https://www.news.example.com/"}
  - {name: general-video, class: site, category: video, url: "https://video.example/"}
candidates:
  - {name: candidate-shop, class: site, category: shopping, url: "https://www.shop.example/"}
`

// Two clean entries per country, which every case keeps.
const testDisjointCleanDe = `
    - {name: de-one, class: site, category: news, url: "https://one.example/"}
    - {name: de-two, class: site, category: news, url: "https://www.two.example.co.uk/"}
`

const testDisjointCleanFr = `
  fr:
    - {name: fr-one, class: site, category: news, url: "https://un.example/"}
    - {name: fr-two, class: site, category: news, url: "https://deux.example/"}
`

func testParseDisjointCatalog(t *testing.T, text string) *ProviderEgressSites {
	t.Helper()
	sites, err := ParseProviderEgressSites(func(out any) error {
		return yaml.Unmarshal([]byte(text), out)
	})
	if err != nil {
		t.Fatalf("the parse refused the whole catalog: %v", err)
	}
	return sites
}

func testCountryNames(sites *ProviderEgressSites, country string) []string {
	names := []string{}
	for _, destination := range sites.Countries[country] {
		names = append(names, destination.Name)
	}
	return names
}

// A country entry that repeats a general entry's name, or loads the same
// website -- the same registrable domain, so www., another subdomain or a path
// on it all count -- is refused, whether the general entry is a destination
// or a candidate. The rest of the file stands: the entry's own list keeps its
// other sites and the other countries are untouched.
func TestParseProviderEgressSitesRefusesCountrySitesCollidingWithGeneralEntries(t *testing.T) {
	for name, colliding := range map[string]string{
		"a candidate's host":                 `{name: de-shop, class: site, category: shopping, url: "https://shop.example/de"}`,
		"a candidate's registrable domain":   `{name: de-sport, class: site, category: sport, url: "https://sport.shop.example/"}`,
		"a candidate's name":                 `{name: candidate-shop, class: site, category: news, url: "https://three.example/"}`,
		"a destination's host":               `{name: de-news, class: site, category: news, url: "https://news.example.com/de"}`,
		"a destination's registrable domain": `{name: de-local-news, class: site, category: news, url: "https://local.news.example.com/"}`,
		"a destination's name":               `{name: general-video, class: site, category: video, url: "https://four.example/"}`,
	} {
		t.Run(name, func(t *testing.T) {
			sites := testParseDisjointCatalog(t, testDisjointGeneral+"countries:\n  de:\n    - "+colliding+testDisjointCleanDe+testDisjointCleanFr)
			if got := strings.Join(testCountryNames(sites, "de"), ","); got != "de-one,de-two" {
				t.Fatalf("country de = %s, want the colliding entry refused and de-one,de-two kept", got)
			}
			if got := strings.Join(testCountryNames(sites, "fr"), ","); got != "fr-one,fr-two" {
				t.Fatalf("country fr = %s, want it untouched", got)
			}
			if len(sites.Destinations) != 2 || len(sites.Candidates) != 1 {
				t.Fatalf("general entries = %d destinations, %d candidates; a country refusal must not touch them", len(sites.Destinations), len(sites.Candidates))
			}
		})
	}
}

// A candidate named like a destination is refused, and the destination's
// contract is the one served under that name, whatever state the stored row
// is in: a candidate never silently replaces a destination.
func TestParseProviderEgressSitesRefusesACandidateNamedLikeADestination(t *testing.T) {
	sites := testParseDisjointCatalog(t, `schema_version: 1
destinations:
  - {name: cdn-a, class: cdn, category: cdn, url: "https://cdn-a.example/robots.txt"}
candidates:
  - {name: cdn-a, class: cdn, category: cdn, url: "https://cdn-b.example/robots.txt", revision: 1}
  - {name: cdn-c, class: cdn, category: cdn, url: "https://cdn-c.example/robots.txt"}
`)
	for _, candidate := range sites.Candidates {
		if candidate.Name == "cdn-a" {
			t.Fatalf("a candidate named like a destination was accepted: %+v", candidate)
		}
	}
	if len(sites.Candidates) != 1 || sites.Candidates[0].Name != "cdn-c" {
		t.Fatalf("candidates = %d, want cdn-c alone", len(sites.Candidates))
	}
	for _, stored := range [][]*model.ProviderEgressDestination{
		nil,
		{{Name: "cdn-a", Url: "https://cdn-b.example/robots.txt", Active: true, Revision: 1}},
	} {
		served := map[string]string{}
		for _, row := range sites.configuredRows(stored) {
			served[row.Name] = row.Url
		}
		if served["cdn-a"] != "https://cdn-a.example/robots.txt" {
			t.Fatalf("cdn-a is served from %q, want the destination's contract", served["cdn-a"])
		}
	}
}

// The parse and the served pool agree: whatever the parse accepts is served
// unchanged -- the pool builds, the prober's own ValidatePool accepts it, and
// it carries every parsed country entry -- with no candidate promoted and
// with every one promoted, as the refresh may leave the pool any day.
func testProviderEgressCatalogServesWhatItParses(t *testing.T, sites *ProviderEgressSites) {
	t.Helper()
	now := time.Date(2026, time.November, 7, 9, 0, 0, 0, time.UTC)
	promoted := []*model.ProviderEgressDestination{}
	for _, candidate := range sites.Candidates {
		row := *candidate
		row.Active = true
		promoted = append(promoted, &row)
	}
	for _, stored := range []struct {
		name string
		rows []*model.ProviderEgressDestination
	}{
		{name: "no candidate promoted", rows: nil},
		{name: "every candidate promoted", rows: promoted},
	} {
		pool, err := BuildProviderEgressDestinationPool(sites.configuredRows(stored.rows), sites.Profile, now, 0, rand.New(rand.NewSource(1)))
		if err != nil {
			t.Errorf("%s: the general pool cannot be served: %v", stored.name, err)
			continue
		}
		pool.Countries = sites.Countries
		pool.UrlProbePolicy = &sites.UrlProbePolicy
		if err := egresshealth.ValidatePool(pool); err != nil {
			t.Errorf("%s: the served pool is refused: %v", stored.name, err)
		}
	}
}

// Every collision the review's mutants made (m1 a candidate's host, m2 a
// candidate's name, m4 a candidate's registrable domain, m6 a candidate named
// like a destination on a country site's host) parses into a catalog that is
// served unchanged.
func TestQualityProbeCatalogParserAndServerAgree(t *testing.T) {
	for name, text := range map[string]string{
		"clean": testDisjointGeneral + "countries:\n  de:" + testDisjointCleanDe + testDisjointCleanFr,
		"m1 a candidate's host": testDisjointGeneral + "countries:\n  de:\n    - " +
			`{name: de-shop, class: site, category: shopping, url: "https://shop.example/de"}` + testDisjointCleanDe,
		"m2 a candidate's name": testDisjointGeneral + "countries:\n  de:\n    - " +
			`{name: candidate-shop, class: site, category: news, url: "https://three.example/"}` + testDisjointCleanDe,
		"m4 a candidate's registrable domain": testDisjointGeneral + "countries:\n  de:\n    - " +
			`{name: de-sport, class: site, category: sport, url: "https://sport.shop.example/"}` + testDisjointCleanDe,
		"m6 a candidate named like a destination on a country site's host": testDisjointGeneral + `
  - {name: general-video, class: site, category: video, url: "https://orf.example/", revision: 1}
countries:
  at:
    - {name: at-orf, class: site, category: news, url: "https://www.orf.example/"}
    - {name: at-two, class: site, category: news, url: "https://zwei.example/"}
`,
	} {
		t.Run(name, func(t *testing.T) {
			testProviderEgressCatalogServesWhatItParses(t, testParseDisjointCatalog(t, text))
		})
	}
}

// The deployed or proposed file under the same agreement, with
// FP2_QUALITY_CATALOG_PATH naming config/<env>/qualityprobe.yml, like
// TestQualityProbeCatalogFromConfiguredPath.
func TestQualityProbeCatalogFromConfiguredPathServesWhatItParses(t *testing.T) {
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
		t.Fatalf("configured catalog failed production validation: %v", err)
	}
	testProviderEgressCatalogServesWhatItParses(t, sites)
	countries := make([]string, 0, len(sites.Countries))
	siteCount := 0
	for country, destinations := range sites.Countries {
		countries = append(countries, country)
		siteCount += len(destinations)
	}
	sort.Strings(countries)
	h3 := func(rows []*model.ProviderEgressDestination) int {
		count := 0
		for _, row := range rows {
			if row.H3 {
				count++
			}
		}
		return count
	}
	t.Logf("served as parsed: general=%d (h3 %d) candidates=%d (h3 %d) countries=%d country_sites=%d",
		len(sites.Destinations), h3(sites.Destinations), len(sites.Candidates), h3(sites.Candidates), len(countries), siteCount)
	families := func(rows []*model.ProviderEgressDestination) (ipv6 int, noIpv4 int) {
		for _, row := range rows {
			if row.Ipv6 {
				ipv6++
			}
			if row.NoIpv4 {
				noIpv4++
			}
		}
		return
	}
	generalIpv6, generalNoIpv4 := families(sites.Destinations)
	candidateIpv6, candidateNoIpv4 := families(sites.Candidates)
	udpDisabled := []string{}
	for country := range sites.CountrySettings {
		udpDisabled = append(udpDisabled, country)
	}
	sort.Strings(udpDisabled)
	t.Logf("ip families: general ipv6 %d, ipv4: false %d; candidates ipv6 %d, ipv4: false %d; countries with udp_probes: false %v",
		generalIpv6, generalNoIpv4, candidateIpv6, candidateNoIpv4, udpDisabled)
}

// The served pool keeps every country list the general entries leave alone
// and drops only a country site that collides with a promoted candidate,
// rather than refusing the whole pool: one bad country entry must never stop
// URL measurement fleet-wide.
func TestGetProviderEgressDestinationPoolDropsOnlyACollidingCountrySite(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		catalog := []byte(`schema_version: 1
destinations:
  - {name: first-synthetic-site, class: site, category: reference, url: "https://first.example/"}
  - {name: second-synthetic-site, class: site, category: reference, url: "https://second.example/"}
candidates:
  - {name: news-candidate, class: site, category: news, url: "https://www.spiegel.example/"}
countries:
  de:
    - {name: de-spiegel, class: site, category: news, url: "https://spiegel.example/"}
    - {name: de-one, class: site, category: news, url: "https://one.example/"}
    - {name: de-two, class: site, category: news, url: "https://two.example/"}
  fr:
    - {name: fr-one, class: site, category: news, url: "https://un.example/"}
    - {name: fr-two, class: site, category: news, url: "https://deux.example/"}
`)
		t.Cleanup(server.Config.PushSimpleResource(model.ProviderEgressSitesResourceName, catalog))
		ctx := context.Background()
		if _, err := EnsureProviderEgressDestinationsSeeded(ctx); err != nil {
			t.Fatal(err)
		}
		sites, err := LoadProviderEgressSites()
		if err != nil {
			t.Fatalf("catalog: %v", err)
		}
		model.SyncProviderEgressDestinationCandidates(ctx, sites.Candidates)
		for _, row := range model.GetProviderEgressDestinations(ctx) {
			if row.Name == "news-candidate" {
				promotedTime := server.NowUtc()
				row.Active, row.Probation, row.PromotedTime = true, true, &promotedTime
				model.SetProviderEgressDestination(ctx, row)
			}
		}

		pool, err := GetProviderEgressDestinationPool(ctx)
		if err != nil {
			t.Fatalf("one colliding country site refused the whole pool: %v", err)
		}
		served := []string{}
		for _, destination := range pool.Destinations {
			served = append(served, destination.Name)
		}
		if got := strings.Join(served, ","); got != "first-synthetic-site,news-candidate,second-synthetic-site" {
			t.Fatalf("general pool = %s, want the destinations and the promoted candidate", got)
		}
		countryNames := func(country string) string {
			names := []string{}
			for _, destination := range pool.Countries[country] {
				names = append(names, destination.Name)
			}
			return strings.Join(names, ",")
		}
		if got := countryNames("de"); got != "de-one,de-two" {
			t.Fatalf("country de = %s, want de-spiegel dropped and the rest served", got)
		}
		if got := countryNames("fr"); got != "fr-one,fr-two" {
			t.Fatalf("country fr = %s, want it served whole", got)
		}
		if err := egresshealth.ValidatePool(pool); err != nil {
			t.Fatalf("the prober refuses the served pool: %v", err)
		}
	})
}
