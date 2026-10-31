// The admitted provider supply each score export measures per country target
// and request IP family (SIGNALS.md §2.9a). Admission is decided here, in the
// export UpdateClientScores runs on the taskworker, so only the process that
// ran the latest export publishes it. The provider-count probe reads it to
// tell a country that lost its supply, or answers empty despite plenty, from
// one that never had any. No client, network, provider or location identity
// leaves the export: a country code, a family and a count.
package model

import (
	"maps"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/server"
)

// How long a measurement is published after its export. Exports run back to
// back, a few minutes apart; a process whose last export is older stopped
// exporting, and its count must not pass for the fleet's current supply.
const clientScoreSupplyMaxAge = time.Hour

// The outcome counter's request families, in label order.
var clientScoreSupplyFamilies = []string{"any", "v4", "v6"}

// The request families a provider of the facet serves: an any request draws
// dualstack and IPv4-only, a v4-only request IPv4-only, a v6 request
// dualstack and IPv6-only (ipFamilyFacetsForFilter).
func clientScoreSupplyFamiliesOf(facet ipFamilyFacet) []string {
	switch facet {
	case ipFamilyFacetDualstack:
		return []string{"any", "v6"}
	case ipFamilyFacetV4Only:
		return []string{"any", "v4"}
	case ipFamilyFacetV6Only:
		return []string{"v6"}
	default:
		return nil
	}
}

type clientScoreSupplyKey struct {
	country string
	family  string
}

// One export's measurement.
type clientScoreSupply struct {
	measuredAt time.Time
	countries  map[clientScoreSupplyKey]int
	fleet      map[string]int
}

// Counts the admitted providers of every country target, and of every
// departed country target at zero, by family. Admitted is what the
// non-forced union pool holds: online, or native in either mode. Per-caller
// network exclusions are not applied; the count is the target's baseline
// payload. The fleet totals sum the same country targets.
func newClientScoreSupply(
	measuredAt time.Time,
	locationClientScores map[server.Id]map[server.Id]*ClientScore,
	departedTargets []clientScoreTarget,
	countryCodes map[server.Id]string,
) *clientScoreSupply {
	supply := &clientScoreSupply{
		measuredAt: measuredAt,
		countries:  map[clientScoreSupplyKey]int{},
		fleet:      map[string]int{},
	}
	addCountry := func(countryId server.Id) (string, bool) {
		country, ok := countryCodes[countryId]
		if !ok || !clientScoreSupplyCountry(country) {
			return "", false
		}
		for _, family := range clientScoreSupplyFamilies {
			supply.countries[clientScoreSupplyKey{country: country, family: family}] += 0
		}
		return country, true
	}
	for _, target := range departedTargets {
		if !target.locationGroup {
			addCountry(target.id)
		}
	}
	for locationId, clientScores := range locationClientScores {
		country, ok := addCountry(locationId)
		if !ok {
			continue
		}
		for _, clientScore := range clientScores {
			if !clientScore.Online && !clientScore.PassesMinimums[RankModeQuality] && !clientScore.PassesMinimums[RankModeSpeed] {
				continue
			}
			for _, family := range clientScoreSupplyFamiliesOf(clientScore.ipFamilyFacet()) {
				supply.countries[clientScoreSupplyKey{country: country, family: family}] += 1
				supply.fleet[family] += 1
			}
		}
	}
	for _, family := range clientScoreSupplyFamilies {
		supply.fleet[family] += 0
	}
	return supply
}

// A lowercase two-letter code, the location table's country vocabulary.
func clientScoreSupplyCountry(country string) bool {
	return len(country) == 2 && 'a' <= country[0] && country[0] <= 'z' && 'a' <= country[1] && country[1] <= 'z'
}

func clientScoreSupplyFamily(family string) bool {
	switch family {
	case "any", "v4", "v6":
		return true
	default:
		return false
	}
}

var (
	clientScoreAdmittedProvidersDesc = prometheus.NewDesc(
		"urnetwork_client_score_admitted_providers",
		"Admitted providers of each country target by request IP family, as the latest score export on this process decided them; published for an hour after that export",
		[]string{"country", "ip_family"}, nil,
	)
	clientScoreAdmittedProvidersFleetDesc = prometheus.NewDesc(
		"urnetwork_client_score_admitted_providers_fleet",
		"Admitted providers of all country targets by request IP family, from the same export",
		[]string{"ip_family"}, nil,
	)
	clientScoreSupplyMeasuredDesc = prometheus.NewDesc(
		"urnetwork_client_score_supply_measured_timestamp_seconds",
		"Unix time of the score export that measured the admitted-provider supply",
		nil, nil,
	)
)

// Publishes the latest measurement atomically: a scrape sees one export's
// counts or none.
type clientScoreSupplyCollector struct {
	stateLock sync.Mutex
	supply    *clientScoreSupply
	now       func() time.Time
}

var clientScoreSupplyMetrics = &clientScoreSupplyCollector{now: server.NowUtc}

func init() {
	prometheus.MustRegister(clientScoreSupplyMetrics)
}

func (self *clientScoreSupplyCollector) publish(supply *clientScoreSupply) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.supply = &clientScoreSupply{
		measuredAt: supply.measuredAt,
		countries:  maps.Clone(supply.countries),
		fleet:      maps.Clone(supply.fleet),
	}
}

// The measurement while it is current, else nil.
func (self *clientScoreSupplyCollector) current() *clientScoreSupply {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.supply == nil || clientScoreSupplyMaxAge < self.now().Sub(self.supply.measuredAt) {
		return nil
	}
	return self.supply
}

func (self *clientScoreSupplyCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- clientScoreAdmittedProvidersDesc
	ch <- clientScoreAdmittedProvidersFleetDesc
	ch <- clientScoreSupplyMeasuredDesc
}

func (self *clientScoreSupplyCollector) Collect(ch chan<- prometheus.Metric) {
	supply := self.current()
	if supply == nil {
		return
	}
	for key, count := range supply.countries {
		if clientScoreSupplyCountry(key.country) && clientScoreSupplyFamily(key.family) {
			ch <- prometheus.MustNewConstMetric(clientScoreAdmittedProvidersDesc, prometheus.GaugeValue, float64(count), key.country, key.family)
		}
	}
	for family, count := range supply.fleet {
		if clientScoreSupplyFamily(family) {
			ch <- prometheus.MustNewConstMetric(clientScoreAdmittedProvidersFleetDesc, prometheus.GaugeValue, float64(count), family)
		}
	}
	ch <- prometheus.MustNewConstMetric(clientScoreSupplyMeasuredDesc, prometheus.GaugeValue, float64(supply.measuredAt.Unix()))
}
