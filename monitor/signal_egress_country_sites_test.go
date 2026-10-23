package monitor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/qualityprobe/egresshealth"
)

var egressCountryNow = time.Date(2026, 10, 22, 6, 32, 0, 0, time.UTC)

var egressCountrySlots = []fleetSlot{
	{service: "taskworker", host: "worker-a.example", block: "g1", allowed: true},
	{service: "taskworker", host: "worker-b.example", block: "g1", allowed: true},
}

// Synthetic sites on reserved example domains, one registrable domain each.
func egressCountryList(prefix string, count int) []egressCountrySite {
	sites := []egressCountrySite{}
	for i := 0; i < count; i++ {
		sites = append(sites, egressCountrySite{url: fmt.Sprintf("https://www.%s-site-%d.example/", prefix, i)})
	}
	return sites
}

type egressCountryFixture struct {
	catalog     *egressCountryCatalog
	catalogErr  error
	denominator []Row
	pgErr       error
	desired     urlProbeCoverageDesired
	desiredErr  error
	sources     map[string]float64
	fresh       map[string]time.Time
	mimirErr    error
	commands    []string
	queries     []string
}

// Two denominator countries with full lists, a full global list and an even
// general/country split.
func healthyEgressCountryFixture() *egressCountryFixture {
	global := []string{}
	for _, site := range egressCountryList("global", 100) {
		global = append(global, site.url)
	}
	at := fleetEvaluationTime(egressCountryNow, DefaultEgressCountrySitesSettings().EvaluationLag)
	fixture := &egressCountryFixture{
		catalog: &egressCountryCatalog{globalUrls: global, countries: map[string][]egressCountrySite{
			"de": egressCountryList("de", 100), "fr": egressCountryList("fr", 100),
		}},
		denominator: []Row{{"de"}, {"fr"}},
		desired:     urlProbeCoverageDesired{enabled: true, shardCount: 4},
		sources:     map[string]float64{"general": 510, "country": 490, "general_country_unavailable": 0, "security_recheck": 1000},
		fresh:       map[string]time.Time{},
	}
	for _, slot := range egressCountrySlots {
		fixture.fresh[slot.host] = at.Add(-20 * time.Second)
	}
	return fixture
}

func (self *egressCountryFixture) run(t *testing.T, mutate func(*SignalSettings)) Alerts {
	t.Helper()
	at := fleetEvaluationTime(egressCountryNow, DefaultEgressCountrySitesSettings().EvaluationLag)
	source := &syntheticSource{
		hostFn: func(host HostSettings, command string) (string, error) {
			if host.Name != "metrics-1" {
				t.Fatalf("receipt read contacted a non-gateway host")
			}
			self.commands = append(self.commands, command)
			if self.mimirErr != nil {
				return "", self.mimirErr
			}
			fixture := newFleetFixture(at)
			for source, value := range self.sources {
				fixture.labeled("sources", map[string]string{"source": source}, value)
			}
			for hostName, sourceTime := range self.fresh {
				fixture.fresh("sources", fleetSlot{service: "taskworker", host: hostName, block: "g1"}, sourceTime)
			}
			return fixture.json(t), nil
		},
		postgresFn: func(query string) ([]Row, error) {
			self.queries = append(self.queries, query)
			return self.denominator, self.pgErr
		},
	}
	settings := syntheticSettings(source)
	settings.Now = func() time.Time { return egressCountryNow }
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "metrics-1", Roles: []string{"services"}})
	hosts := []string{}
	for _, slot := range egressCountrySlots {
		settings.Hosts = append(settings.Hosts, HostSettings{Name: slot.host, Roles: []string{"edge"}})
		hosts = append(hosts, slot.host)
	}
	settings.LogServices = []string{"taskworker"}
	settings.LogServiceHosts = map[string][]string{"taskworker": hosts}
	settings.LogServiceBlocks = map[string][]string{"taskworker": {"g1"}}
	if mutate != nil {
		mutate(&settings)
	}
	signal := &signalAdapter{
		number: "2.19d", key: "egress-country-sites", name: "Country-specific egress site coverage",
		probe: egressCountrySitesProbe{
			settings:    DefaultEgressCountrySitesSettings(),
			loadCatalog: func() (*egressCountryCatalog, error) { return self.catalog, self.catalogErr },
			loadDesired: func() (urlProbeCoverageDesired, error) { return self.desired, self.desiredErr },
		},
	}
	alerts, err := signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	for _, alert := range alerts {
		requireAlertOmits(t, alert, "https://", "-site-", "www.")
	}
	return alerts
}

// Complete lists still carry the standing provenance gap, and nothing else.
func TestEgressCountrySitesHealthyCatalogKeepsTheProvenanceGap(t *testing.T) {
	alerts := healthyEgressCountryFixture().run(t, nil)
	gap := requireAlertClass(t, alerts, "egress-country-provenance-missing")
	if len(alerts) != 1 || gap.unobservable || gap.Severity != SeverityWarn ||
		!strings.Contains(gap.Observed, "missing_records=source_identity,source_period,curation_time,load_verification_time,pool_generation_receipts,per_country_draw_receipts") {
		t.Fatalf("healthy catalog alerts = %+v", alerts)
	}
}

func TestEgressCountrySitesMissingAndUnderfilledLists(t *testing.T) {
	fixture := healthyEgressCountryFixture()
	fixture.denominator = []Row{{"de"}, {"fr"}, {"jp"}, {"zy"}}
	fixture.catalog.countries["fr"] = egressCountryList("fr", 42)
	fixture.catalog.countries["xk"] = egressCountryList("xk", 100)
	fixture.catalog.countries["zy"] = nil
	fixture.catalog.globalUrls = fixture.catalog.globalUrls[:99]
	fixture.sources["general_country_unavailable"] = 250
	alerts := fixture.run(t, nil)
	missing := requireAlertClass(t, alerts, "egress-country-list-missing")
	for _, want := range []string{"denominator_countries=4", "missing=2", "missing_countries=jp,zy", "lists_outside_denominator=1", "outside_countries=xk", "country_unavailable_draws=250", "normal_country_fallback_share=20.0%"} {
		if !strings.Contains(missing.Observed, want) {
			t.Fatalf("missing-list alert lacks %q: %s", want, missing.Observed)
		}
	}
	underfilled := requireAlertClass(t, alerts, "egress-country-list-underfilled")
	if !strings.Contains(underfilled.Observed, "global_sites=99") || !strings.Contains(underfilled.Observed, "underfilled=fr:42") {
		t.Fatalf("underfilled alert = %s", underfilled.Observed)
	}
	if !strings.Contains(missing.Mechanism, "not a provider or country fault") || !strings.Contains(missing.Mechanism, "general URL measurements remain valid") {
		t.Fatal("list coverage was attributed to providers")
	}
	if len(alerts) != 3 {
		t.Fatalf("alerts = %+v", alerts)
	}
}

func TestEgressCountrySitesOverlapUsesRegistrableDomains(t *testing.T) {
	fixture := healthyEgressCountryFixture()
	de := egressCountryList("de", 100)
	de[0].url = "https://shop.global-site-1.example/news"
	de[1].url = "https://news.de-site-2.example/today"
	de[3].url = "http://www.de-site-3.example/"
	fixture.catalog.countries["de"] = de
	alert := requireAlertClass(t, fixture.run(t, nil), "egress-country-list-overlap")
	for _, want := range []string{"global_overlap_sites=1", "duplicate_sites=1", "invalid_country_urls=1", "countries=de"} {
		if !strings.Contains(alert.Observed, want) {
			t.Fatalf("overlap alert lacks %q: %s", want, alert.Observed)
		}
	}
}

// A whole-country exclusion removes a site from its own list; a region
// exclusion is a provider-specific sampling limit and still counts.
func TestEgressCountrySitesCountsOnlyCompatibleSites(t *testing.T) {
	fixture := healthyEgressCountryFixture()
	de := egressCountryList("de", 100)
	de[0].incompatible = []egresshealth.Place{{Country: "de"}}
	de[1].incompatible = []egresshealth.Place{{Country: "DE"}}
	de[2].incompatible = []egresshealth.Place{{Country: "de", Region: "Bavaria"}}
	fixture.catalog.countries["de"] = de
	fixture.catalog.countries["fr"] = []egressCountrySite{{url: "https://www.fr-site-0.example/", incompatible: []egresshealth.Place{{Country: "fr"}}}}
	alerts := fixture.run(t, nil)
	underfilled := requireAlertClass(t, alerts, "egress-country-list-underfilled")
	missing := requireAlertClass(t, alerts, "egress-country-list-missing")
	if !strings.Contains(underfilled.Observed, "underfilled=de:98") || !strings.Contains(missing.Observed, "missing_countries=fr") {
		t.Fatalf("compatible counts = %s / %s", underfilled.Observed, missing.Observed)
	}
}

func TestEgressCountrySitesDrawSplit(t *testing.T) {
	for _, c := range []struct {
		general, country float64
		split            bool
	}{
		{general: 900, country: 100, split: true},
		{general: 100, country: 900, split: true},
		{general: 520, country: 480},
		{general: 300, country: 0},
	} {
		fixture := healthyEgressCountryFixture()
		fixture.sources["general"], fixture.sources["country"] = c.general, c.country
		alerts := fixture.run(t, nil)
		if count := requireAlertClassCount(alerts, "egress-country-draw-split"); (count == 1) != c.split {
			t.Fatalf("%.0f/%.0f: alerts = %+v", c.general, c.country, alerts)
		}
		if c.split && !strings.Contains(requireAlertClass(t, alerts, "egress-country-draw-split").Observed, "deviation_sigma=25.3") {
			t.Fatalf("split deviation lost: %+v", alerts)
		}
	}
}

func TestEgressCountrySitesUnknownControls(t *testing.T) {
	for _, c := range []struct {
		name     string
		mutate   func(*egressCountryFixture)
		settings func(*SignalSettings)
		reason   string
		absent   string
	}{
		{name: "catalog", mutate: func(f *egressCountryFixture) { f.catalog, f.catalogErr = nil, errors.New("synthetic catalog refused") }, reason: "catalog_unavailable", absent: "egress-country-provenance-missing"},
		{name: "denominator", mutate: func(f *egressCountryFixture) { f.pgErr = errors.New("synthetic database refused") }, reason: "denominator_unavailable", absent: "egress-country-list-missing"},
		{name: "denominator row", mutate: func(f *egressCountryFixture) { f.denominator = []Row{{"de"}, {"Hostile Code"}} }, reason: "denominator_unavailable"},
		{name: "empty denominator", mutate: func(f *egressCountryFixture) { f.denominator = nil }, reason: "denominator_unavailable"},
		{name: "receipts", mutate: func(f *egressCountryFixture) { f.mimirErr = errors.New("synthetic gateway refused") }, reason: "receipts_unavailable", absent: "egress-country-draw-split"},
		{name: "receipt slot", mutate: func(f *egressCountryFixture) { delete(f.fresh, "worker-b.example") }, reason: "receipt_slot_coverage_incomplete"},
		{name: "receipt slot excluded", settings: func(s *SignalSettings) { s.ExcludedHosts = []string{"worker-a.example"} }, reason: "receipt_slot_coverage_incomplete"},
		{name: "source label", mutate: func(f *egressCountryFixture) { f.sources["hostile"] = 1 }, reason: "unrecognized_draw_source"},
		{name: "source missing", mutate: func(f *egressCountryFixture) { delete(f.sources, "country") }, reason: "draw_sources_incomplete", absent: "egress-country-draw-split"},
		{name: "workflow", mutate: func(f *egressCountryFixture) { f.desiredErr = errors.New("synthetic configuration refused") }, reason: "url_workflow_unknown"},
	} {
		fixture := healthyEgressCountryFixture()
		if c.mutate != nil {
			c.mutate(fixture)
		}
		alerts := fixture.run(t, c.settings)
		unknown := requireAlertClass(t, alerts, "egress-country-sites-unobservable")
		if !unknown.unobservable || !strings.Contains(unknown.Observed, c.reason) {
			t.Fatalf("%s: unknown alert lacks %s: %+v", c.name, c.reason, unknown)
		}
		if c.absent != "" && requireAlertClassCount(alerts, c.absent) != 0 {
			t.Fatalf("%s: unread source still produced %s", c.name, c.absent)
		}
		requireAlertOmits(t, unknown, "synthetic catalog refused", "synthetic database refused", "synthetic gateway refused", "Hostile Code", "hostile")
	}
}

// A disabled URL workflow has no receipts to read and is not unknown.
func TestEgressCountrySitesDisabledWorkflowReadsNoReceipts(t *testing.T) {
	fixture := healthyEgressCountryFixture()
	fixture.desired = urlProbeCoverageDesired{}
	alerts := fixture.run(t, nil)
	gap := requireAlertClass(t, alerts, "egress-country-provenance-missing")
	if len(alerts) != 1 || len(fixture.commands) != 0 || !strings.Contains(gap.Observed, "url_workflow=disabled") {
		t.Fatalf("disabled workflow = %+v, %d reads", alerts, len(fixture.commands))
	}
}

func TestEgressCountrySitesReadShapes(t *testing.T) {
	fixture := healthyEgressCountryFixture()
	fixture.run(t, nil)
	if len(fixture.queries) != 1 || len(fixture.commands) != 1 {
		t.Fatalf("reads = %d queries, %d commands", len(fixture.queries), len(fixture.commands))
	}
	if !strings.Contains(fixture.queries[0], "FROM location WHERE location_type = 'country' GROUP BY 1 ORDER BY 1 LIMIT 513") {
		t.Fatalf("denominator query = %s", fixture.queries[0])
	}
	for _, want := range []string{
		"--max-time 15", "--max-filesize 262144",
		`sum by (source) (increase(urnetwork_url_probe_sources_total{env="synthetic",job="taskworker"}[21600s]))`,
		`max by (job,host,block) (timestamp(urnetwork_url_probe_sources_total{env="synthetic",job=~"taskworker"`,
	} {
		if !strings.Contains(fixture.commands[0], want) {
			t.Fatalf("receipt read lacks %q:\n%s", want, fixture.commands[0])
		}
	}
}

func TestEgressCountrySitesCanceledParentPublishesNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) {
		cancel()
		return nil, context.Canceled
	}}
	settings := syntheticSettings(source)
	signal := &signalAdapter{number: "2.19d", key: "egress-country-sites", name: "Country-specific egress site coverage", probe: egressCountrySitesProbe{
		settings:    DefaultEgressCountrySitesSettings(),
		loadCatalog: func() (*egressCountryCatalog, error) { return healthyEgressCountryFixture().catalog, nil },
		loadDesired: func() (urlProbeCoverageDesired, error) { return urlProbeCoverageDesired{}, nil },
	}}
	alerts, err := signal.Run(ctx, settings)
	if !errors.Is(err, context.Canceled) || len(alerts) != 0 {
		t.Fatalf("canceled run = %+v, %v", alerts, err)
	}
}
