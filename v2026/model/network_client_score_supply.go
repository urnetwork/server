// The admitted provider supply each score export measures per country target
// and request IP family (SIGNALS.md §2.9a), and how many of those providers
// the country floor admitted to Online below the URL bar
// (network_client_score_country_floor.go). Admission is decided here, in the
// export UpdateClientScores runs on the taskworker. The export runs on
// whichever taskworker claims it, so within an hour several processes each
// hold a measurement; only the one holding the newest publishes it, by a
// lease in Redis, so the fleet exports one copy of the series. The
// provider-count probe reads it to tell a country that lost its supply, or
// answers empty despite plenty, from one that never had any. No client,
// network, provider or location identity leaves the export: a country code,
// a family and a count.
package model

import (
	"context"
	"fmt"
	"maps"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
)

// How long a measurement is published after its export. Exports run back to
// back, a few minutes apart; a process whose last export is older stopped
// exporting, and its count must not pass for the fleet's current supply.
const clientScoreSupplyMaxAge = time.Hour

// The lease on publishing the supply, held by the process with the newest
// measurement. Its value names the measurement's time and the process
// (clientScoreSupplyLease). One key, so its hash tag spreads nothing.
const clientScoreSupplyPublisherKey = "{client_score_supply_publisher}"

// How long the lease outlives its holder's last claim. Every process with a
// current measurement claims on each scrape, every 15 seconds (the stats push
// interval), and the holder's claim renews the lease, so it lapses only once
// its holder stopped: drained, crashed or cut off from Redis. The newest
// measurement left then takes it within this and one scrape, inside the five
// minutes Mimir keeps reading a series that stopped, so the probe sees no gap.
const clientScoreSupplyPublisherLeaseTtl = time.Minute

// How long one claim decides this process's scrapes. Shorter than the scrape
// interval, so every scrape claims afresh and a process a newer measurement
// displaced stops at its next scrape, while scrapes in a burst share a claim.
const clientScoreSupplyPublisherClaimMaxAge = 5 * time.Second

// A claim's Redis deadline, the longest a scrape waits for it.
const clientScoreSupplyPublisherTimeout = time.Second

// Claims the lease for ARGV[1], the lease of a measurement taken at ARGV[2]
// (unix milliseconds), for ARGV[3] milliseconds, and returns the lease after
// the claim. The holder's own claim renews it. A lease of a measurement at
// least as new stays with its holder; one that lapsed, is older or does not
// parse passes to the claim. Times compare as decimal strings, length first,
// so Lua number rounding never enters. One key, so one slot.
var clientScoreSupplyPublisherClaimScript = redis.NewScript(`
local current = redis.call('GET', KEYS[1])
if current == ARGV[1] then
  redis.call('PEXPIRE', KEYS[1], ARGV[3])
  return current
end
if current then
  local measured = string.match(current, '^(%d+):')
  if measured and (#measured > #ARGV[2] or (#measured == #ARGV[2] and measured >= ARGV[2])) then
    return current
  end
end
redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[3])
return ARGV[1]
`)

// The lease value of a measurement taken at measuredAt by publisher: the
// measurement's unix milliseconds, then the process.
func clientScoreSupplyLease(measuredAt time.Time, publisher string) string {
	return fmt.Sprintf("%d:%s", measuredAt.UnixMilli(), publisher)
}

// Claims the lease in Redis for lease, the lease of a measurement taken at
// measuredAt, and returns the lease after the claim. One bounded script,
// never retried.
func claimClientScoreSupplyPublisher(ctx context.Context, lease string, measuredAt time.Time) (holder string, returnErr error) {
	claimCtx, cancel := context.WithTimeout(ctx, clientScoreSupplyPublisherTimeout)
	defer cancel()
	returnErr = server.RedisWithDeadline(claimCtx, func(r server.RedisClient) error {
		value, err := clientScoreSupplyPublisherClaimScript.Run(
			claimCtx,
			r,
			[]string{clientScoreSupplyPublisherKey},
			lease,
			strconv.FormatInt(measuredAt.UnixMilli(), 10),
			clientScoreSupplyPublisherLeaseTtl.Milliseconds(),
		).Text()
		if err != nil {
			return err
		}
		holder = value
		return nil
	}, server.OptRedisNoPreflight())
	return
}

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
	// The country floor's admissions to Online
	// (network_client_score_country_floor.go), which countries and fleet
	// count with the rest: by country and decision, a country without one
	// absent, and by decision, zero included. Nil for none.
	countryFloor      map[clientScoreCountryFloorKey]int
	countryFloorFleet map[string]int
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
		"Admitted providers of each country target by request IP family, as the newest score export decided them; published for an hour after that export by the taskworker holding it",
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
	// a country the floor admitted nothing to has no series
	clientScoreCountryFloorProvidersDesc = prometheus.NewDesc(
		"urnetwork_client_score_country_floor_providers",
		"Providers the country floor admitted to Online below the URL bar, by country target and decision (strict: the facets every request reads; classified: the capability facets of selector-mode requests), from the same export",
		[]string{"country", "decision"}, nil,
	)
	clientScoreCountryFloorProvidersFleetDesc = prometheus.NewDesc(
		"urnetwork_client_score_country_floor_providers_fleet",
		"Providers the country floor admitted to Online below the URL bar in all country targets, by decision, from the same export",
		[]string{"decision"}, nil,
	)
)

// Publishes this process's latest measurement atomically, so a scrape sees
// one export's counts or none, and only while the process holds the newest
// measurement: it holds the publisher lease, or cannot claim it.
type clientScoreSupplyCollector struct {
	now func() time.Time
	// this process in the lease
	publisher string
	// claims the lease for the lease of a measurement and returns the lease
	// after the claim (claimClientScoreSupplyPublisher)
	claim func(ctx context.Context, lease string, measuredAt time.Time) (string, error)
	// log the start of a window of failed claims, and its end
	errorf func(format string, args ...any)
	infof  func(format string, args ...any)

	stateLock sync.Mutex
	supply    *clientScoreSupply
	// the last claim, nil before the first
	claimed *clientScoreSupplyClaim
	// the last claim failed, and its failure window has been logged
	failing bool
}

// One claim's outcome for a measurement.
type clientScoreSupplyClaim struct {
	supply    *clientScoreSupply
	claimTime time.Time
	// this process publishes the measurement: it holds the lease, or the
	// claim failed
	publishes bool
}

var clientScoreSupplyMetrics = &clientScoreSupplyCollector{
	now: server.NowUtc,
	// host, block and process id, as the ARIN canary names the process that
	// publishes its decision
	publisher: ArindbCanaryPublisher(),
	claim:     claimClientScoreSupplyPublisher,
	errorf:    glog.Errorf,
	infof:     glog.Infof,
}

func init() {
	prometheus.MustRegister(clientScoreSupplyMetrics)
}

// Holds an export's measurement and claims the lease for it at once, so the
// process holding an older one stops at its next scrape.
func (self *clientScoreSupplyCollector) publish(ctx context.Context, supply *clientScoreSupply) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.supply = &clientScoreSupply{
			measuredAt:        supply.measuredAt,
			countries:         maps.Clone(supply.countries),
			fleet:             maps.Clone(supply.fleet),
			countryFloor:      maps.Clone(supply.countryFloor),
			countryFloorFleet: maps.Clone(supply.countryFloorFleet),
		}
	}()
	self.publication(ctx)
}

// The measurement while it is current, else nil.
func (self *clientScoreSupplyCollector) current() *clientScoreSupply {
	supply, _ := self.state(self.now())
	return supply
}

// The measurement while it is current at now, else nil, and the claim that
// decides it, nil when it needs a new one.
func (self *clientScoreSupplyCollector) state(now time.Time) (*clientScoreSupply, *clientScoreSupplyClaim) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.supply == nil || clientScoreSupplyMaxAge < now.Sub(self.supply.measuredAt) {
		return nil, nil
	}
	claimed := self.claimed
	if claimed == nil || claimed.supply != self.supply || clientScoreSupplyPublisherClaimMaxAge <= now.Sub(claimed.claimTime) {
		claimed = nil
	}
	return self.supply, claimed
}

// The measurement this process publishes now: the current one, while this
// process holds the lease or cannot claim it; else nil. A process without a
// current measurement claims nothing.
func (self *clientScoreSupplyCollector) publication(ctx context.Context) *clientScoreSupply {
	now := self.now()
	supply, claimed := self.state(now)
	if supply == nil {
		return nil
	}
	if claimed == nil {
		claimed = self.claimLease(ctx, supply, now)
	}
	if !claimed.publishes {
		return nil
	}
	return supply
}

// Claims the lease for supply and keeps the outcome for the scrapes within
// clientScoreSupplyPublisherClaimMaxAge of now. A failed claim publishes, as
// every process did before the lease: the probe reads the newest of the
// measurements published, so a duplicate costs series where silence would
// cost the supply.
func (self *clientScoreSupplyCollector) claimLease(ctx context.Context, supply *clientScoreSupply, now time.Time) *clientScoreSupplyClaim {
	lease := clientScoreSupplyLease(supply.measuredAt, self.publisher)
	holder, err := func() (holder string, err error) {
		// a claim that panics fails like any other
		defer func() {
			if recovered := recover(); recovered != nil {
				holder, err = "", fmt.Errorf("the claim failed: %v", recovered)
			}
		}()
		// the outcome serves every scrape in the window, so the caller's own
		// cancellation must not fail it; the claim keeps its own deadline
		return self.claim(context.WithoutCancel(ctx), lease, supply.measuredAt)
	}()
	claimed := &clientScoreSupplyClaim{
		supply:    supply,
		claimTime: now,
		publishes: err != nil || holder == lease,
	}
	failureStarted, failureEnded := func() (bool, bool) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		// a claim for a measurement since replaced decides nothing
		if self.supply == supply {
			self.claimed = claimed
		}
		failureStarted := err != nil && !self.failing
		failureEnded := err == nil && self.failing
		self.failing = err != nil
		return failureStarted, failureEnded
	}()
	if failureStarted {
		self.errorf("[nclm]client score supply publisher lease: %s; this process publishes its measurement until a claim succeeds\n", err)
	}
	if failureEnded {
		self.infof("[nclm]client score supply publisher lease: claims succeed again\n")
	}
	return claimed
}

func (self *clientScoreSupplyCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- clientScoreAdmittedProvidersDesc
	ch <- clientScoreAdmittedProvidersFleetDesc
	ch <- clientScoreSupplyMeasuredDesc
	ch <- clientScoreCountryFloorProvidersDesc
	ch <- clientScoreCountryFloorProvidersFleetDesc
}

func (self *clientScoreSupplyCollector) Collect(ch chan<- prometheus.Metric) {
	supply := self.publication(context.Background())
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
	for key, count := range supply.countryFloor {
		if clientScoreSupplyCountry(key.country) && 0 < count {
			ch <- prometheus.MustNewConstMetric(clientScoreCountryFloorProvidersDesc, prometheus.GaugeValue, float64(count), key.country, key.decision)
		}
	}
	for decision, count := range supply.countryFloorFleet {
		ch <- prometheus.MustNewConstMetric(clientScoreCountryFloorProvidersFleetDesc, prometheus.GaugeValue, float64(count), decision)
	}
	ch <- prometheus.MustNewConstMetric(clientScoreSupplyMeasuredDesc, prometheus.GaugeValue, float64(supply.measuredAt.Unix()))
}
