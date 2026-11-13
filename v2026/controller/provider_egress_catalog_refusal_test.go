// The refusals of the destination catalog: the website key every stage
// compares, the refusals the parse names and counts, the served pool's
// fail-small drop, and the refresh's promotion check. The behaviour these
// pin down is new; provider_egress_catalog_disjoint_test.go holds the tests
// that also run against the code before it.
package controller

import (
	"math/rand"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"gopkg.in/yaml.v3"

	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// The website key: the registrable domain of the host without "www." or a
// trailing dot, under the Public Suffix List's private section too, and a
// host with none is its own site.
func TestProviderEgressSiteKey(t *testing.T) {
	for rawUrl, want := range map[string]string{
		"https://www.example.co.uk/":           "example.co.uk",
		"https://shop.example.co.uk/path?q=1":  "example.co.uk",
		"https://example.co.uk/":               "example.co.uk",
		"https://WWW.News.Example.COM./a":      "example.com",
		"https://sport.spiegel.example/":       "spiegel.example",
		"https://a.github.io/":                 "a.github.io",
		"https://b.github.io/":                 "b.github.io",
		"https://github.io/":                   "github.io",
		"https://www.github.io/":               "github.io",
		"https://co.uk/":                       "co.uk",
		"https://192.0.2.7/":                   "192.0.2.7",
		"https://[2001:db8::1]/":               "2001:db8::1",
		"https://doh.360.cn/resolve?name=a.cn": "360.cn",
	} {
		if got := ProviderEgressSiteKey(rawUrl); got != want {
			t.Errorf("ProviderEgressSiteKey(%q) = %q, want %q", rawUrl, got, want)
		}
	}
	// a function of the prober's own overlap key (the host without "www."):
	// two urls the prober calls one host are always one site
	for _, pair := range [][2]string{
		{"https://www.github.io/", "https://github.io/x"},
		{"https://www.co.uk/", "https://co.uk/"},
		{"https://www.example.com/", "https://example.com/"},
		{"https://WWW.EXAMPLE.COM/", "https://example.com/"},
	} {
		if ProviderEgressSiteKey(pair[0]) != ProviderEgressSiteKey(pair[1]) {
			t.Errorf("%s and %s share a host for the prober but not a site", pair[0], pair[1])
		}
	}
}

// Every refusal names its entry, its list and its reason, and the parse keeps
// the rest of the file: a country entry the prober could not run, or a list
// under a key that is no country code, is refused alone; a candidate named
// like a destination and a country site colliding with a general entry too.
func TestParseProviderEgressSitesNamesEveryRefusal(t *testing.T) {
	sites := testParseDisjointCatalog(t, testDisjointGeneral+`
  - {name: general-news, class: site, category: news, url: "https://other-news.example/", revision: 1}
countries:
  de:
    - {name: de-shop, class: site, category: shopping, url: "https://sport.shop.example/"}
    - {name: de-plain, class: site, category: news, url: "http://plain.example/"}
    - {name: de-uncategorized, class: site, url: "https://uncategorized.example/"}
    - {name: de-one, class: site, category: news, url: "https://one.example/"}
    - {name: de-one, class: site, category: news, url: "https://one-again.example/"}
    - {name: de-two, class: site, category: news, url: "https://www.two.example.co.uk/"}
  DE:
    - {name: upper-one, class: site, category: news, url: "https://upper.example/"}
  "no":
    - {name: no-one, class: site, category: news, url: "https://en.example/"}
`)
	refused := []string{}
	for _, refusal := range sites.Refused {
		refused = append(refused, refusal.Reason+" "+refusal.Country+"/"+refusal.Name)
	}
	want := []string{
		"candidate_name /general-news",
		"country_invalid DE/upper-one",
		"country_invalid de/de-plain",
		"country_invalid de/de-uncategorized",
		"country_invalid de/de-one",
		"country_conflict de/de-shop",
	}
	if strings.Join(refused, "; ") != strings.Join(want, "; ") {
		t.Fatalf("refused = %v, want %v", refused, want)
	}
	for _, refusal := range sites.Refused {
		if refusal.Detail == "" || !strings.Contains(refusal.String(), refusal.Name) {
			t.Errorf("refusal %+v does not say what it refuses and why", refusal)
		}
	}
	if !strings.Contains(sites.Refused[len(sites.Refused)-1].Detail, "shop.example") {
		t.Errorf("the conflict does not name the shared website: %s", sites.Refused[len(sites.Refused)-1].Detail)
	}
	if got := strings.Join(testCountryNames(sites, "de"), ","); got != "de-one,de-two" {
		t.Fatalf("country de = %s, want the first de-one and de-two", got)
	}
	if got := strings.Join(testCountryNames(sites, "no"), ","); got != "no-one" {
		t.Fatalf("country no = %s, want it kept", got)
	}
	if _, ok := sites.Countries["DE"]; ok {
		t.Fatal("a list under an upper-case key was kept")
	}
	if len(sites.Destinations) != 2 || len(sites.Candidates) != 1 || sites.Destinations[0].Url != "https://www.news.example.com/" {
		t.Fatalf("general entries changed: %d destinations, %d candidates", len(sites.Destinations), len(sites.Candidates))
	}
}

// A clean catalog refuses nothing.
func TestParseProviderEgressSitesRefusesNothingInACleanCatalog(t *testing.T) {
	sites := testParseDisjointCatalog(t, testDisjointGeneral+"countries:\n  de:"+testDisjointCleanDe+testDisjointCleanFr)
	if len(sites.Refused) != 0 {
		t.Fatalf("a clean catalog refused %v", sites.Refused)
	}
}

// Each refusal of a read is counted under the parse stage, once per read.
func TestLoadProviderEgressSitesCountsItsRefusals(t *testing.T) {
	t.Cleanup(server.Config.PushSimpleResource(model.ProviderEgressSitesResourceName, []byte(testDisjointGeneral+`
  - {name: general-video, class: site, category: video, url: "https://video-candidate.example/"}
countries:
  de:
    - {name: de-shop, class: site, category: shopping, url: "https://shop.example/"}
    - {name: de-one, class: site, category: news, url: "https://one.example/"}
`)))
	count := func(reason string) float64 {
		return testutil.ToFloat64(providerEgressCatalogRefusalsTotal.WithLabelValues(ProviderEgressRefusalStageParse, reason))
	}
	names, conflicts := count(ProviderEgressRefusalCandidateName), count(ProviderEgressRefusalCountryConflict)
	for range 2 {
		if _, err := LoadProviderEgressSites(); err != nil {
			t.Fatal(err)
		}
	}
	if got := count(ProviderEgressRefusalCandidateName) - names; got != 2 {
		t.Errorf("candidate_name refusals counted %v times over two reads, want 2", got)
	}
	if got := count(ProviderEgressRefusalCountryConflict) - conflicts; got != 2 {
		t.Errorf("country_conflict refusals counted %v times over two reads, want 2", got)
	}
}

// Were the served general rows and the parsed catalog ever to come apart, the
// served pool drops the one country site that collides with a served entry,
// counts it under the serve stage, and serves everything else.
func TestServePoolDropsOnlyACountrySiteCollidingWithAServedEntry(t *testing.T) {
	sites := testParseDisjointCatalog(t, testDisjointGeneral+"countries:\n  de:"+testDisjointCleanDe+testDisjointCleanFr)
	// a site the parse would have refused, put back as if it had not been
	sites.Countries["de"] = append([]egresshealth.Destination{{
		Name: "de-shop", Class: egresshealth.ClassSite, Url: "https://shop.example/", Expect: egresshealth.ExpectBody,
	}}, sites.Countries["de"]...)
	stored := []*model.ProviderEgressDestination{{Name: "candidate-shop", Active: true, Probation: true}}
	dropped := testutil.ToFloat64(providerEgressCatalogRefusalsTotal.WithLabelValues(ProviderEgressRefusalStageServe, ProviderEgressRefusalCountryConflict))

	pool, err := sites.servePool(stored, time.Date(2026, time.November, 7, 9, 0, 0, 0, time.UTC), rand.New(rand.NewSource(1)))
	if err != nil {
		t.Fatalf("one colliding country site refused the whole pool: %v", err)
	}
	served := []string{}
	for _, destination := range pool.Destinations {
		served = append(served, destination.Name)
	}
	if got := strings.Join(served, ","); got != "candidate-shop,general-news,general-video" {
		t.Fatalf("general pool = %s", got)
	}
	names := func(country string) string {
		list := []string{}
		for _, destination := range pool.Countries[country] {
			list = append(list, destination.Name)
		}
		return strings.Join(list, ",")
	}
	if names("de") != "de-one,de-two" || names("fr") != "fr-one,fr-two" {
		t.Fatalf("served countries de=%s fr=%s, want de-shop alone dropped", names("de"), names("fr"))
	}
	if got := testutil.ToFloat64(providerEgressCatalogRefusalsTotal.WithLabelValues(ProviderEgressRefusalStageServe, ProviderEgressRefusalCountryConflict)) - dropped; got != 1 {
		t.Fatalf("serve-stage drops counted %v, want 1", got)
	}
	if err := egresshealth.ValidatePool(pool); err != nil {
		t.Fatalf("the prober refuses the served pool: %v", err)
	}

	// with the candidate not promoted there is nothing to collide with
	pool, err = sites.servePool(nil, time.Date(2026, time.November, 7, 9, 0, 0, 0, time.UTC), rand.New(rand.NewSource(1)))
	if err != nil || names("de") != "de-shop,de-one,de-two" {
		t.Fatalf("with no candidate promoted: de=%s err=%v, want de-shop served", names("de"), err)
	}
}

// A candidate is held back from promotion when the entry served under its
// name would collide with any country site the file lists, the ones the parse
// refused for colliding with it included; a destination's name is judged by
// the destination's contract, and a name the file no longer lists is never
// served, so never held back.
func TestPromotionConflictJudgesTheEntryServedUnderTheName(t *testing.T) {
	sites := testParseDisjointCatalog(t, testDisjointGeneral+`
  - {name: candidate-clean, class: site, category: news, url: "https://clean.example/"}
  - {name: general-video, class: site, category: video, url: "https://orf.example/", revision: 1}
countries:
  de:
    - {name: de-shop, class: site, category: shopping, url: "https://sport.shop.example/"}
    - {name: de-one, class: site, category: news, url: "https://one.example/"}
  at:
    - {name: at-orf, class: site, category: news, url: "https://www.orf.example/"}
`)
	for name, want := range map[string]bool{
		// the parse refused de-shop for colliding with it
		"candidate-shop":  true,
		"candidate-clean": false,
		// served from the destination's video.example, not the refused
		// candidate's orf.example
		"general-video": false,
		"unlisted-row":  false,
	} {
		refusal, held := sites.PromotionConflict(&model.ProviderEgressDestination{Name: name, Url: "https://www.shop.example/stale"})
		if held != want {
			t.Errorf("%s held = %t, want %t (%+v)", name, held, want, refusal)
		}
		if held && (refusal.Reason != ProviderEgressRefusalCountryConflict || !strings.Contains(refusal.Detail, "countries.de entry \"de-shop\"")) {
			t.Errorf("%s refusal = %+v, want it to name the country site", name, refusal)
		}
	}
	if names := strings.Join(testCountryNames(sites, "at"), ","); names != "at-orf" {
		t.Fatalf("country at = %s, want at-orf kept beside the destination it does not collide with", names)
	}
}

// The deployed or proposed file, with FP2_QUALITY_CATALOG_PATH naming
// config/<env>/qualityprobe.yml, keeps every entry it lists: a catalog the
// parse would refuse entries of is not the catalog its author meant, so this
// readback names each refusal and fails before the file is released.
func TestQualityProbeCatalogFromConfiguredPathRefusesNothing(t *testing.T) {
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
	reasons := map[string]int{}
	for _, refusal := range sites.Refused {
		reasons[refusal.Reason]++
		t.Errorf("refused: %s", refusal)
	}
	t.Logf("refusals: total=%d by reason=%v", len(sites.Refused), reasons)
}
