// Tests for the pool's join to the prober's wire (connect/GEOMAP.md §11.4):
// the seed, the conversions, egress-sites.yml, and the served pool.
package controller

import (
	"context"
	"errors"
	"math/rand"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// The seed is the prober's whole built-in table, every row with a category
// and a region, the two costly cdn objects re-pointed, and every row one the
// prober can run.
func TestProviderEgressDestinationSeedIsTheBuiltinTable(t *testing.T) {
	seed, err := ProviderEgressDestinationSeed()
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if len(seed) != len(egresshealth.Destinations()) {
		t.Fatalf("seed rows = %d, want the %d built-in destinations", len(seed), len(egresshealth.Destinations()))
	}
	for _, row := range seed {
		if row.Category == "" || row.Region == "" || row.Source != model.ProviderEgressDestinationSourceBuiltin || row.Revision != 0 {
			t.Errorf("seed row %s = category %q region %q source %q revision %d", row.Name, row.Category, row.Region, row.Source, row.Revision)
		}
		if _, err := ProviderEgressDestinationToWire(row); err != nil {
			t.Errorf("seed row %s cannot be served: %v", row.Name, err)
		}
		if url, ok := providerEgressBuiltinUrlReplacements[row.Name]; ok && row.Url != url {
			t.Errorf("seed row %s url = %q, want the small object %q", row.Name, row.Url, url)
		}
	}
	for name := range providerEgressBuiltinSiteMeta {
		found := false
		for _, row := range seed {
			found = found || row.Name == name
		}
		if !found {
			t.Errorf("the category table names %s, which the built-in table does not hold", name)
		}
	}
}

// A destination survives the trip to a row and back, and a row the prober
// could not run is refused on the way in.
func TestProviderEgressDestinationWireRoundTrip(t *testing.T) {
	destination := egresshealth.Destination{
		Name:     "news-site",
		Class:    egresshealth.Class("site"),
		Url:      "https://news.example/robots.txt",
		Headers:  map[string]string{"Accept": "text/plain"},
		MaxBytes: 1024,
		Verify:   egresshealth.BodyCheck{Kind: egresshealth.BodyCheckKind("contains"), Text: "User-agent"},
		Incompatible: []egresshealth.Place{
			{Country: "cn"},
			{Country: "us", Region: "California"},
		},
	}
	row, err := ProviderEgressDestinationFromWire(destination, "news", "global", 2)
	if err != nil {
		t.Fatalf("from wire: %v", err)
	}
	if row.Expect != "body" || row.Incompatible[0].Country != "cn" || row.Incompatible[1].Region != "California" || row.Revision != 2 {
		t.Fatalf("row = %+v", row)
	}
	back, err := ProviderEgressDestinationToWire(row)
	if err != nil {
		t.Fatalf("to wire: %v", err)
	}
	if back.Name != destination.Name || back.Url != destination.Url || back.Headers["Accept"] != "text/plain" ||
		back.Verify != destination.Verify || back.MaxBytes != 1024 || len(back.Incompatible) != 2 || back.Canary {
		t.Fatalf("round trip = %+v", back)
	}

	for _, broken := range []egresshealth.Destination{
		{Name: "plain-site", Class: egresshealth.Class("site"), Url: "http://plain.example/"},
		{Name: "comma,site", Class: egresshealth.Class("site"), Url: "https://comma.example/"},
		{Name: "", Class: egresshealth.Class("site"), Url: "https://unnamed.example/"},
	} {
		if _, err := ProviderEgressDestinationFromWire(broken, "news", "global", 1); err == nil {
			t.Errorf("a destination the prober could not run was accepted: %+v", broken)
		}
	}
}

// One egress-sites.yml document, parsed as the refresh parses it.
func testParseProviderEgressSites(text string) (*ProviderEgressSites, error) {
	return ParseProviderEgressSites(func(out any) error {
		return yaml.Unmarshal([]byte(text), out)
	})
}

// A candidate list is taken whole: its entries validated with the prober's
// rules, the defaults filled in, the profile read.
func TestParseProviderEgressSitesReadsCandidatesAndProfile(t *testing.T) {
	sites, err := testParseProviderEgressSites(`
settings:
  site_min_samples: 300
profile:
  user_agent: "synthetic-agent/1.0"
  headers:
    Accept: "text/html"
candidates:
  - name: news-site
    class: site
    category: news
    url: https://news.example/robots.txt
    incompatible:
      - country: cn
    revision: 1
  - name: resolver
    class: dns
    category: resolver
    region: europe
    url: https://resolver.example/dns-query?name=example.com&type=A
    headers:
      Accept: application/dns-json
    verify:
      kind: dns_json
    max_bytes: 768
`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if sites.Settings.SiteMinSamples != 300 || sites.Settings.SiteRetireShare != model.DefaultProviderEgressSiteSettings().SiteRetireShare {
		t.Errorf("settings = %+v", sites.Settings)
	}
	if sites.Profile.UserAgent != "synthetic-agent/1.0" || sites.Profile.Headers["Accept"] != "text/html" {
		t.Errorf("profile = %+v", sites.Profile)
	}
	if len(sites.Candidates) != 2 {
		t.Fatalf("candidates = %d, want 2", len(sites.Candidates))
	}
	news, resolver := sites.Candidates[0], sites.Candidates[1]
	if news.Expect != "body" || news.Region != "global" || news.Source != model.ProviderEgressDestinationSourceCandidates ||
		len(news.Incompatible) != 1 || news.Incompatible[0].Country != "cn" || news.Revision != 1 {
		t.Errorf("news candidate = %+v", news)
	}
	if resolver.Region != "europe" || resolver.Verify.Kind != "dns_json" || resolver.MaxBytes != 768 {
		t.Errorf("resolver candidate = %+v", resolver)
	}

	sites, err = testParseProviderEgressSites("candidates: []\n")
	if err != nil || sites.Profile.UserAgent != egresshealth.DefaultRequestProfile().UserAgent || len(sites.Candidates) != 0 {
		t.Fatalf("an empty file = %+v, %v; want the defaults", sites, err)
	}
}

// One entry the prober could not run refuses the whole file, with every
// problem named.
func TestParseProviderEgressSitesRefusesWhatTheProberCouldNotRun(t *testing.T) {
	_, err := testParseProviderEgressSites(`
candidates:
  - name: plain-site
    class: site
    category: news
    url: http://plain.example/robots.txt
  - name: uncategorized-site
    class: site
    url: https://uncategorized.example/robots.txt
  - name: twice-site
    class: site
    category: news
    url: https://twice.example/robots.txt
  - name: twice-site
    class: site
    category: news
    url: https://twice.example/other.txt
  - name: odd-site
    class: site
    category: news
    expect: sometimes
    url: https://odd.example/robots.txt
`)
	if err == nil {
		t.Fatal("a file with broken entries was accepted")
	}
	for _, named := range []string{"plain-site", "no category", "repeats the name", "odd-site"} {
		if !strings.Contains(err.Error(), named) {
			t.Errorf("the refusal does not name %q: %v", named, err)
		}
	}
	if _, err := testParseProviderEgressSites("profile:\n  user_agent: \"\"\n"); err == nil {
		t.Error("an empty user agent was accepted")
	}
	if _, err := testParseProviderEgressSites("settings:\n  site_retire_share: 2\n"); err == nil {
		t.Error("broken settings were accepted")
	}
}

// The file shipped in the config repository parses with the prober's rules,
// holds the two cdn overrides and fills every class it names.
func TestLoadProviderEgressSitesAcceptsTheDeployedFile(t *testing.T) {
	if _, err := server.Config.SimpleResource(model.ProviderEgressSitesResourceName); errors.Is(err, server.ErrResourceNotFound) {
		t.Skip("no egress-sites.yml in this environment's config")
	}
	sites, err := LoadProviderEgressSites()
	if err != nil {
		t.Fatalf("the deployed egress-sites.yml is unusable: %v", err)
	}
	byName := map[string]*model.ProviderEgressDestination{}
	for _, candidate := range sites.Candidates {
		byName[candidate.Name] = candidate
	}
	for _, name := range []string{"cachefly", "amazon-cloudfront"} {
		if candidate, ok := byName[name]; !ok || candidate.Revision < 1 || candidate.Url != providerEgressBuiltinUrlReplacements[name] {
			t.Errorf("the deployed file does not override %s with its small object: %+v", name, candidate)
		}
	}
	if len(sites.Candidates) < 40 {
		t.Errorf("the deployed file holds %d candidates, want the forty or so the refresh promotes from", len(sites.Candidates))
	}
}

// Pool rows over every state: scored, on probation, a candidate, and one
// marked incompatible with a place.
func testPoolRows() []*model.ProviderEgressDestination {
	row := func(name string, active bool, probation bool) *model.ProviderEgressDestination {
		return &model.ProviderEgressDestination{
			Name:      name,
			Class:     "site",
			Url:       "https://" + name + ".example/robots.txt",
			Expect:    "body",
			Category:  "news",
			Region:    "global",
			Active:    active,
			Probation: probation,
		}
	}
	marked := row("marked-site", true, false)
	marked.Incompatible = []model.ProviderEgressDestinationPlace{{Country: "cn"}}
	// a pool the prober runs holds every class
	resolver := row("resolver", true, false)
	resolver.Class = "dns"
	resolver.Url = "https://resolver.example/dns-query?name=example.com&type=A"
	resolver.Headers = map[string]string{"Accept": "application/dns-json"}
	resolver.Verify = model.ProviderEgressDestinationVerify{Kind: "dns_json"}
	portal := row("portal", true, false)
	portal.Class = "connectivity"
	portal.Verify = model.ProviderEgressDestinationVerify{Kind: "contains", Text: "ok"}
	edge := row("edge", true, false)
	edge.Class = "cdn"
	return []*model.ProviderEgressDestination{
		row("scored-site", true, false),
		row("candidate-site", false, false),
		row("probation-site", true, true),
		marked,
		resolver,
		portal,
		edge,
	}
}

// Every active row is served in name order, probation included; a marked row
// is a canary in the canary share of fetches; and the version names the
// contents, whatever the canary draw.
func TestBuildProviderEgressDestinationPoolServesTheActiveRows(t *testing.T) {
	now := time.Date(2026, time.September, 20, 6, 0, 0, 0, time.UTC)
	profile := egresshealth.DefaultRequestProfile()
	pool, err := BuildProviderEgressDestinationPool(testPoolRows(), profile, now, 1, rand.New(rand.NewSource(1)))
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	names := []string{}
	for _, destination := range pool.Destinations {
		names = append(names, destination.Name)
		if destination.Canary != (destination.Name == "marked-site") {
			t.Errorf("%s canary = %t", destination.Name, destination.Canary)
		}
	}
	if strings.Join(names, ",") != "edge,marked-site,portal,probation-site,resolver,scored-site" {
		t.Fatalf("served = %v, want the active rows in name order", names)
	}
	if !pool.GeneratedAt.Equal(now) || pool.Profile.UserAgent != profile.UserAgent || pool.Version < 1 {
		t.Fatalf("pool = version %d generated %s", pool.Version, pool.GeneratedAt)
	}

	noCanary, err := BuildProviderEgressDestinationPool(testPoolRows(), profile, now, 0.0001, rand.New(rand.NewSource(1)))
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	for _, destination := range noCanary.Destinations {
		if destination.Canary {
			t.Errorf("%s drawn as a canary at a near-zero share", destination.Name)
		}
	}
	if noCanary.Version != pool.Version {
		t.Fatalf("the canary draw changed the version: %d, %d", noCanary.Version, pool.Version)
	}

	changed := testPoolRows()
	changed[0].Url = "https://scored-site.example/other.txt"
	other, err := BuildProviderEgressDestinationPool(changed, profile, now, 0, nil)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	if other.Version == pool.Version {
		t.Fatal("a changed row kept the pool's version")
	}
}

// A pool the prober would refuse is refused here, so its fallback is the
// server's decision and says why.
func TestBuildProviderEgressDestinationPoolRefusesAPoolTheProberWouldRefuse(t *testing.T) {
	rows := testPoolRows()
	rows[0].Url = "http://scored-site.example/robots.txt"
	if _, err := BuildProviderEgressDestinationPool(rows, egresshealth.DefaultRequestProfile(), time.Now(), 0, nil); err == nil {
		t.Fatal("a pool with a row the prober cannot run was served")
	}
	rows = testPoolRows()
	rows[0].Expect = "sometimes"
	if _, err := BuildProviderEgressDestinationPool(rows, egresshealth.DefaultRequestProfile(), time.Now(), 0, nil); err == nil {
		t.Fatal("a pool with an unknown contract was served")
	}
}

// The first request seeds an empty pool and serves the built-in table; the
// seed happens once, and a row taken out of the pool is no longer served.
func TestGetProviderEgressDestinationPoolSeedsAndServes(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		pool, err := GetProviderEgressDestinationPool(ctx)
		if err != nil {
			t.Fatalf("pool: %v", err)
		}
		if len(pool.Destinations) != len(egresshealth.Destinations()) {
			t.Fatalf("served %d destinations, want the %d built-in ones", len(pool.Destinations), len(egresshealth.Destinations()))
		}
		seeded, err := EnsureProviderEgressDestinationsSeeded(ctx)
		if err != nil || seeded {
			t.Fatalf("a seeded pool was seeded again: %t %v", seeded, err)
		}

		rows := model.GetProviderEgressDestinations(ctx)
		retired := rows[0]
		retired.Active = false
		model.SetProviderEgressDestination(ctx, retired)
		pool, err = GetProviderEgressDestinationPool(ctx)
		if err != nil {
			t.Fatalf("pool: %v", err)
		}
		for _, destination := range pool.Destinations {
			if destination.Name == retired.Name {
				t.Fatalf("the retired row %s is still served", retired.Name)
			}
		}
		if len(pool.Destinations) != len(egresshealth.Destinations())-1 {
			t.Fatalf("served %d destinations, want one fewer than the table", len(pool.Destinations))
		}
	})
}
