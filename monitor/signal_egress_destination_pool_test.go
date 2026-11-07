package monitor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/urnetwork/server/controller"
)

var egressDestinationPoolNow = time.Date(2026, 11, 7, 9, 30, 0, 0, time.UTC)

var egressDestinationPoolSlots = []fleetSlot{
	{service: "taskworker", host: "worker-a.example", block: "g1", allowed: true},
	{service: "taskworker", host: "worker-b.example", block: "g1", allowed: true},
	{service: "api", host: "api-a.example", block: "g1", allowed: true},
}

// A catalog on reserved example domains with one country list and nothing
// refused.
const egressDestinationPoolCleanCatalog = `schema_version: 1
destinations:
  - {name: cdn-a, class: cdn, category: cdn, url: "https://cdn-a.example/robots.txt"}
  - {name: news-a, class: site, category: news, url: "https://news-a.example/"}
candidates:
  - {name: shop-a, class: site, category: shopping, url: "https://www.shop-a.example/"}
countries:
  de:
    - {name: de-one, class: site, category: news, url: "https://one-de.example/"}
`

// The clean catalog plus a candidate repeating a destination's name and a
// country site on a candidate's registrable domain.
const egressDestinationPoolRefusingCatalog = `schema_version: 1
destinations:
  - {name: cdn-a, class: cdn, category: cdn, url: "https://cdn-a.example/robots.txt"}
  - {name: news-a, class: site, category: news, url: "https://news-a.example/"}
candidates:
  - {name: shop-a, class: site, category: shopping, url: "https://www.shop-a.example/"}
  - {name: cdn-a, class: cdn, category: cdn, url: "https://cdn-b.example/robots.txt", revision: 1}
countries:
  de:
    - {name: de-shop, class: site, category: shopping, url: "https://sport.shop-a.example/"}
    - {name: de-one, class: site, category: news, url: "https://one-de.example/"}
`

type egressDestinationPoolFixture struct {
	catalog    string
	catalogErr error
	fetches    map[string]float64
	refusals   map[[2]string]float64
	// the counter families each slot exports fresh, by host
	fresh    map[string][]string
	mimirErr error
}

// Two Taskworkers and an api exporting both counters, every pass loading the
// served pool, and a clean catalog.
func healthyEgressDestinationPoolFixture() *egressDestinationPoolFixture {
	fixture := &egressDestinationPoolFixture{
		catalog: egressDestinationPoolCleanCatalog,
		fetches: map[string]float64{"server": 240, "unavailable": 0},
		refusals: map[[2]string]float64{
			{"serve", "country_conflict"}:   0,
			{"refresh", "country_conflict"}: 0,
		},
		fresh: map[string][]string{},
	}
	for _, slot := range egressDestinationPoolSlots {
		if slot.service == "taskworker" {
			fixture.fresh[slot.host] = []string{"fetches", "refusals"}
		} else {
			fixture.fresh[slot.host] = []string{"refusals"}
		}
	}
	return fixture
}

func (self *egressDestinationPoolFixture) settings(t *testing.T) SignalSettings {
	t.Helper()
	at := fleetEvaluationTime(egressDestinationPoolNow, DefaultEgressDestinationPoolSettings().EvaluationLag)
	source := &syntheticSource{
		hostFn: func(host HostSettings, command string) (string, error) {
			if host.Name != "metrics-1" {
				t.Fatalf("the fleet read contacted a non-gateway host")
			}
			if self.mimirErr != nil {
				return "", self.mimirErr
			}
			fixture := newFleetFixture(at)
			for result, value := range self.fetches {
				fixture.labeled("fetches", map[string]string{"result": result}, value)
			}
			for key, value := range self.refusals {
				fixture.labeled("refusals", map[string]string{"stage": key[0], "reason": key[1]}, value)
			}
			for _, slot := range egressDestinationPoolSlots {
				for _, family := range self.fresh[slot.host] {
					fixture.fresh(family, slot, at.Add(-20*time.Second))
				}
			}
			return fixture.json(t), nil
		},
	}
	settings := syntheticSettings(source)
	settings.Now = func() time.Time { return egressDestinationPoolNow }
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "metrics-1", Roles: []string{"services"}})
	hosts := map[string][]string{}
	for _, slot := range egressDestinationPoolSlots {
		settings.Hosts = append(settings.Hosts, HostSettings{Name: slot.host, Roles: []string{"edge"}})
		hosts[slot.service] = append(hosts[slot.service], slot.host)
	}
	settings.LogServices = []string{"taskworker", "api"}
	settings.LogServiceHosts = hosts
	settings.LogServiceBlocks = map[string][]string{"taskworker": {"g1"}, "api": {"g1"}}
	return settings
}

func (self *egressDestinationPoolFixture) probe() egressDestinationPoolProbe {
	return egressDestinationPoolProbe{
		settings: DefaultEgressDestinationPoolSettings(),
		loadCatalog: func() (*controller.ProviderEgressSites, error) {
			if self.catalogErr != nil {
				return nil, self.catalogErr
			}
			return controller.ParseProviderEgressSites(func(out any) error { return yaml.Unmarshal([]byte(self.catalog), out) })
		},
	}
}

// The alerts of one run, which never render a URL or a website.
func (self *egressDestinationPoolFixture) run(t *testing.T) Alerts {
	t.Helper()
	signal := &signalAdapter{
		number: "2.19h", key: "egress-destination-pool", name: "Destination pool availability and catalog refusals",
		probe: self.probe(),
	}
	alerts, err := signal.Run(context.Background(), self.settings(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, alert := range alerts {
		requireAlertOmits(t, alert, "https://", "robots.txt", "shop-a.example", "cdn-b.example", "one-de.example")
	}
	return alerts
}

// The healthy classes one run reports, by class.
func (self *egressDestinationPoolFixture) healthy(t *testing.T) map[string]bool {
	t.Helper()
	env, err := newProbeEnv(self.settings(t).withDefaults())
	if err != nil {
		t.Fatal(err)
	}
	findings, err := self.probe().check(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	healthy := map[string]bool{}
	for _, finding := range findings {
		if finding.healthy {
			healthy[finding.class] = true
		}
	}
	return healthy
}

func TestEgressDestinationPoolHealthyFleetAndCatalog(t *testing.T) {
	fixture := healthyEgressDestinationPoolFixture()
	if alerts := fixture.run(t); len(alerts) != 0 {
		t.Fatalf("healthy pool alerts = %+v", alerts)
	}
	healthy := fixture.healthy(t)
	if !healthy["egress-destination-pool-unavailable"] || !healthy["egress-destination-catalog-refused"] {
		t.Fatalf("healthy classes = %v, want both incident classes resolved", healthy)
	}
}

// Every pass failing to get a pool alerts, with the share; a Taskworker built
// before the rename reports the same result as builtin. One failed fetch in
// the window is a blip.
func TestEgressDestinationPoolUnavailableFetchesAlert(t *testing.T) {
	fixture := healthyEgressDestinationPoolFixture()
	fixture.fetches = map[string]float64{"server": 0, "unavailable": 6, "builtin": 3}
	alert := requireAlertClass(t, fixture.run(t), "egress-destination-pool-unavailable")
	if alert.Severity != SeverityWarn || !strings.Contains(alert.Observed, "unavailable_fetches=9") || !strings.Contains(alert.Observed, "unavailable_share=100.0%") {
		t.Fatalf("unavailable alert = %+v", alert)
	}

	fixture = healthyEgressDestinationPoolFixture()
	fixture.fetches = map[string]float64{"server": 240, "unavailable": 1}
	requireNoAlertClass(t, fixture.run(t), "egress-destination-pool-unavailable")
	if !fixture.healthy(t)["egress-destination-pool-unavailable"] {
		t.Fatal("a single failed fetch did not resolve as healthy")
	}
}

// The unavailable alert of each cadence over ticks consecutive cadences, as
// the scheduler's cadence gate releases it: the sustain holds the first one,
// and the page sustain promotes a lasting one to PAGE.
func (self *egressDestinationPoolFixture) gated(t *testing.T, ticks int) []Alerts {
	t.Helper()
	signal := &signalAdapter{
		number: "2.19h", key: "egress-destination-pool", name: "Destination pool availability and catalog refusals",
		probe: self.probe(),
	}
	gate := newCadenceAlertGate()
	released := []Alerts{}
	for range ticks {
		alerts, err := signal.Run(context.Background(), self.settings(t))
		if err != nil {
			t.Fatal(err)
		}
		unavailable := Alerts{}
		for _, alert := range alerts {
			if alert.Class == "egress-destination-pool-unavailable" {
				unavailable = append(unavailable, alert)
			}
		}
		released = append(released, gate.filter(signal, unavailable))
	}
	return released
}

// Every pass fetches the pool, so a window holds a few hundred fetches: three
// failed of 243 (1.2%) is transient, and over hours of cadences it neither
// warns nor pages, and the class resolves healthy.
func TestEgressDestinationPoolTransientFailuresNeitherWarnNorPage(t *testing.T) {
	fixture := healthyEgressDestinationPoolFixture()
	fixture.fetches = map[string]float64{"server": 240, "unavailable": 3}
	for tick, released := range fixture.gated(t, 12) {
		if 0 < len(released) {
			t.Fatalf("cadence %d released %+v for 3 of 243 fetches failing", tick+1, released)
		}
	}
	if !fixture.healthy(t)["egress-destination-pool-unavailable"] {
		t.Fatal("transient failures did not resolve the class as healthy")
	}
}

// When most fetches fail, most passes measure nothing: the alert warns from
// its second cadence and pages from its sixth.
func TestEgressDestinationPoolMostFetchesFailingPages(t *testing.T) {
	fixture := healthyEgressDestinationPoolFixture()
	fixture.fetches = map[string]float64{"server": 20, "unavailable": 220}
	for tick, released := range fixture.gated(t, 8) {
		cadence := tick + 1
		want := SeverityWarn
		switch {
		case cadence < 2:
			if len(released) != 0 {
				t.Fatalf("cadence %d released %+v before the sustain", cadence, released)
			}
			continue
		case 6 <= cadence:
			want = SeverityPage
		}
		if len(released) != 1 || released[0].Severity != want {
			t.Fatalf("cadence %d released %+v, want one %s", cadence, released, want)
		}
		if !strings.Contains(released[0].Observed, "unavailable_share=91.7%") || !strings.Contains(released[0].Observed, "threshold_share=50%") {
			t.Fatalf("cadence %d observed %q", cadence, released[0].Observed)
		}
	}
}

// The alert needs half or more of the window's fetches unavailable, and at
// least two of them.
func TestEgressDestinationPoolUnavailableShareAndFloor(t *testing.T) {
	for _, test := range []struct {
		served, unavailable float64
		alerts              bool
	}{
		{served: 120, unavailable: 120, alerts: true},
		{served: 123, unavailable: 117, alerts: false},
		{served: 0, unavailable: 2, alerts: true},
		{served: 0, unavailable: 1, alerts: false},
		{served: 0, unavailable: 0, alerts: false},
	} {
		fixture := healthyEgressDestinationPoolFixture()
		fixture.fetches = map[string]float64{"server": test.served, "unavailable": test.unavailable}
		alerted := false
		for _, alert := range fixture.run(t) {
			alerted = alerted || alert.Class == "egress-destination-pool-unavailable"
		}
		if alerted != test.alerts {
			t.Errorf("served=%.0f unavailable=%.0f alerted=%t, want %t", test.served, test.unavailable, alerted, test.alerts)
		}
		if !test.alerts && !fixture.healthy(t)["egress-destination-pool-unavailable"] {
			t.Errorf("served=%.0f unavailable=%.0f did not resolve as healthy", test.served, test.unavailable)
		}
	}
}

// The catalog's own refusals alert by entry name, country code and reason,
// never by URL or website.
func TestEgressDestinationPoolCatalogRefusalsAlert(t *testing.T) {
	fixture := healthyEgressDestinationPoolFixture()
	fixture.catalog = egressDestinationPoolRefusingCatalog
	alert := requireAlertClass(t, fixture.run(t), "egress-destination-catalog-refused")
	for _, want := range []string{"catalog_refused=2", "candidate_name:1", "country_conflict:1", "country_invalid:0", "candidates/cdn-a", "de/de-shop", "fleet_refusals=none"} {
		if !strings.Contains(alert.Observed, want) {
			t.Errorf("refusal alert observed %q lacks %q", alert.Observed, want)
		}
	}
}

// A serve or refresh refusal counted by the fleet alerts with a clean catalog.
func TestEgressDestinationPoolFleetRefusalsAlert(t *testing.T) {
	fixture := healthyEgressDestinationPoolFixture()
	fixture.refusals[[2]string{"refresh", "country_conflict"}] = 1
	alert := requireAlertClass(t, fixture.run(t), "egress-destination-catalog-refused")
	if !strings.Contains(alert.Observed, "catalog_refused=0") || !strings.Contains(alert.Observed, "fleet_refusals=refresh/country_conflict:1") {
		t.Fatalf("fleet refusal alert = %q", alert.Observed)
	}
}

// A slot without the refusal counter -- a process built before it -- leaves
// the state unknown: no healthy class, a visibility alert. Positive evidence
// read beside it still alerts.
func TestEgressDestinationPoolIncompleteCoverageIsUnobservable(t *testing.T) {
	fixture := healthyEgressDestinationPoolFixture()
	fixture.fresh["worker-b.example"] = []string{"fetches"}
	alerts := fixture.run(t)
	visibility := requireAlertClass(t, alerts, "egress-destination-pool-unobservable")
	if !strings.Contains(visibility.Observed, "slot_coverage_incomplete") || !strings.Contains(visibility.Observed, "worker-b.example") {
		t.Fatalf("visibility alert = %q", visibility.Observed)
	}
	if healthy := fixture.healthy(t); healthy["egress-destination-pool-unavailable"] || healthy["egress-destination-catalog-refused"] {
		t.Fatalf("incomplete coverage resolved classes as healthy: %v", healthy)
	}

	fixture.catalog = egressDestinationPoolRefusingCatalog
	fixture.fetches = map[string]float64{"server": 0, "unavailable": 8}
	alerts = fixture.run(t)
	requireAlertClass(t, alerts, "egress-destination-catalog-refused")
	requireAlertClass(t, alerts, "egress-destination-pool-unavailable")
	requireAlertClass(t, alerts, "egress-destination-pool-unobservable")
}

// An unreadable catalog or Mimir path is unknown, never healthy; what the
// other source proves is still reported.
func TestEgressDestinationPoolUnreadableSourcesAreUnobservable(t *testing.T) {
	fixture := healthyEgressDestinationPoolFixture()
	fixture.catalogErr = errors.New("synthetic catalog failure")
	visibility := requireAlertClass(t, fixture.run(t), "egress-destination-pool-unobservable")
	if !strings.Contains(visibility.Observed, "catalog_unavailable") {
		t.Fatalf("visibility alert = %q", visibility.Observed)
	}
	healthy := fixture.healthy(t)
	if healthy["egress-destination-catalog-refused"] || !healthy["egress-destination-pool-unavailable"] {
		t.Fatalf("healthy classes = %v, want only the fleet-judged one", healthy)
	}

	fixture = healthyEgressDestinationPoolFixture()
	fixture.catalog = egressDestinationPoolRefusingCatalog
	fixture.mimirErr = errors.New("synthetic gateway failure")
	alerts := fixture.run(t)
	requireAlertClass(t, alerts, "egress-destination-catalog-refused")
	requireAlertClass(t, alerts, "egress-destination-pool-unobservable")
	requireNoAlertClass(t, alerts, "egress-destination-pool-unavailable")
	if healthy := fixture.healthy(t); healthy["egress-destination-pool-unavailable"] {
		t.Fatal("an unread fleet resolved the unavailable class")
	}
}
