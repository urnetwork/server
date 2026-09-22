package controller

// the public network stats collector.
// periodically refreshes the urnetwork_stats_* gauges behind the warp
// grafana front's public /stats.json feed (warp repo, grafana/stats.go),
// which implements the network operator stats contract consumed by ur.xyz:
//
//	urnetwork_stats_total_networks                   total networks (db)
//	urnetwork_stats_block_users                      unique top-level identities with contract usage this block (db)
//	urnetwork_stats_online_providers                 connected valid public providers (db)
//	urnetwork_stats_countries                        countries with connected valid providers (db)
//	urnetwork_stats_staked_alpha                     cumulative α staked in the ST contract (chain)
//	urnetwork_stats_block_demand_deposits_alpha      demand deposits this block (st_event mirror)
//	urnetwork_stats_block_miner_emissions_alpha      miner emission captured this block (st_event mirror)
//	urnetwork_stats_alpha_usd                        α price in USD (geckoterminal, sourced server side)
//	urnetwork_stats_prev_block_users                 users of the last finished block (redis snapshot)
//	urnetwork_stats_prev_block_demand_deposits_alpha demand deposits of the last finished block
//	urnetwork_stats_prev_block_miner_emissions_alpha miner emission of the last finished block
//
// and the public grafana dashboard (grafana/dashboards/public-traffic.json)
// additionally reads:
//
//	urnetwork_stats_users_24h                        unique top-level identities with contract usage in the last 24h (db)
//	urnetwork_stats_online_providers_by_country      the same, per country {country_code, country} (db)
//	urnetwork_stats_provider_regions                 distinct regions with a connected valid public provider (db)
//	urnetwork_stats_provider_cities                  distinct cities with a connected valid public provider (db)
//	urnetwork_stats_online_extenders                 active extenders with at least one active address (db)
//	urnetwork_stats_online_extenders_by_country      the same, per country {country_code, country} (db)
//	urnetwork_stats_online_providers_by_ip_family    online providers by proven family {ip_family} (db)
//	urnetwork_stats_online_extenders_by_ip_family    online extenders by active address family {ip_family} (db)
//	urnetwork_stats_block_number                     the current subnet block number (clock)
//	urnetwork_stats_block_start_seconds              unix open time of the current subnet block (clock)
//	urnetwork_stats_block_end_seconds                unix close time of the current subnet block (clock)
//	urnetwork_stats_block_miner_claims_alpha         alpha claimed by miners this block (st_event mirror)
//	urnetwork_stats_block_miners_claimed             distinct miner coldkeys that claimed this block (st_event mirror)
//	urnetwork_stats_prev_block_miner_claims_alpha    alpha claimed by miners in the last finished block
//	urnetwork_stats_prev_block_miners_claimed        distinct miner coldkeys that claimed in the last finished block
//
// the contract gauges are internal (connect/EXTENDER.md M3, M4) and are read
// only by grafana/dashboards/providers.json:
//
//	urnetwork_stats_open_contracts                   transfer contracts open now (db)
//	urnetwork_stats_contracts_24h                    transfer contracts created in the trailing 24 hours (db + redis hour buckets)
//	urnetwork_stats_open_contracts_with_extender     of the open contracts, those with an extender party (db)
//	urnetwork_stats_contracts_with_extender_24h      the same over the trailing 24 hours (db + redis hour buckets)
//	urnetwork_stats_open_disputes                    disputes raised and not yet decided (db)
//	urnetwork_stats_disputes_24h                     contracts created in the trailing 24 hours that are disputed (db + redis hour buckets)
//
// the 24 hour contract numbers are summed from one hour buckets of
// create_time; a complete bucket is computed once by whichever host needs it
// first and cached in redis, so a refresh normally scans only the current
// partial hour (model/contract_stats_model.go).
//
// the block accumulators reset when a block rolls over, so the feed also
// serves the finished block as a stable reference. deposits and emissions
// recompute exactly from the st_event mirror by chain block range; users
// come from a per-block redis snapshot frozen forward by this collector
// (a last-seen activity marker cannot reconstruct a past window), so
// prev_block_users starts publishing at the first rollover after deploy.
//
// StartStatsCollector is called only by the taskworker
// (cli/taskworker/main.go), so only taskworker registries export these
// series, and every taskworker host recomputes the same values on its own
// clock — the feed reads each gauge with max across hosts. a gauge
// registers with the default registry (pushed by server.StartStatsPusher)
// on its first set, so a stat that never has a value here (st disabled,
// no netuid) is never exported at all rather than exported as 0.
//
// the "block" here is the subnet block clock (model/sn_model.go): 1-based,
// 7 days per block, every block opening and closing at 00:00 UTC Sunday.
// it is unrelated to the warp deploy block label and to the ST contract
// epoch; st_event rows are windowed by mapping the block open time to a
// chain block through the ~12s/block head anchor.

import (
	"context"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	stconn "github.com/urnetwork/server/v2026/st"
)

const statsCollectorInterval = 60 * time.Second

// the db counts scan the hot network_client table, so they refresh on
// every statsCollectorDbTicks'th tick rather than every tick
const statsCollectorDbTicks = 5

// statsGauge registers with the default registry on first set, so an
// unset gauge is never pushed. set only from the collector goroutine
type statsGauge struct {
	gauge      prometheus.Gauge
	registered bool
}

func newStatsGauge(name string, help string) *statsGauge {
	return &statsGauge{
		gauge: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "urnetwork",
			Subsystem: "stats",
			Name:      name,
			Help:      help,
		}),
	}
}

func (self *statsGauge) set(value float64) {
	if !self.registered {
		prometheus.MustRegister(self.gauge)
		self.registered = true
	}
	self.gauge.Set(value)
}

// statsGaugeVec is a labeled stats gauge with the same lazy registration.
// each refresh replaces the whole label set: series for labels absent from
// the new set are deleted so they go stale in the store instead of pushing
// their last value forever. set only from the collector goroutine
type statsGaugeVec struct {
	gauge      *prometheus.GaugeVec
	registered bool
	// the label values currently set, keyed by their joined values
	current map[string][]string
}

func newStatsGaugeVec(name string, help string, labelNames ...string) *statsGaugeVec {
	return &statsGaugeVec{
		gauge: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "urnetwork",
			Subsystem: "stats",
			Name:      name,
			Help:      help,
		}, labelNames),
		current: map[string][]string{},
	}
}

// statsLabeledValue is one series of a statsGaugeVec: label values in the
// vec's label name order and the value
type statsLabeledValue struct {
	labelValues []string
	value       float64
}

func statsLabelKey(labelValues []string) string {
	return strings.Join(labelValues, "\x00")
}

// replace sets every given series and deletes the series that were set by
// the previous replace and are absent now
func (self *statsGaugeVec) replace(values []statsLabeledValue) {
	if !self.registered {
		prometheus.MustRegister(self.gauge)
		self.registered = true
	}
	next := map[string][]string{}
	for _, value := range values {
		self.gauge.WithLabelValues(value.labelValues...).Set(value.value)
		next[statsLabelKey(value.labelValues)] = value.labelValues
	}
	for key, labelValues := range self.current {
		if _, ok := next[key]; !ok {
			self.gauge.DeleteLabelValues(labelValues...)
		}
	}
	self.current = next
}

var statsTotalNetworksGauge = newStatsGauge(
	"total_networks",
	"Total networks at the operator",
)
var statsBlockUsersGauge = newStatsGauge(
	"block_users",
	"Unique top-level clients with contract usage this block",
)
var statsCountriesGauge = newStatsGauge(
	"countries",
	"Countries with a connected valid provider",
)
var statsStakedAlphaGauge = newStatsGauge(
	"staked_alpha",
	"Cumulative alpha staked in the ST contract (buybackTotal)",
)
var statsBlockDemandDepositsAlphaGauge = newStatsGauge(
	"block_demand_deposits_alpha",
	"Alpha demand deposits this block",
)
var statsBlockMinerEmissionsAlphaGauge = newStatsGauge(
	"block_miner_emissions_alpha",
	"Alpha miner emission captured this block",
)
var statsAlphaUsdGauge = newStatsGauge(
	"alpha_usd",
	"Alpha price in USD as the operator observes it",
)
var statsPrevBlockUsersGauge = newStatsGauge(
	"prev_block_users",
	"Unique top-level clients with contract usage in the last finished block",
)
var statsPrevBlockDemandDepositsAlphaGauge = newStatsGauge(
	"prev_block_demand_deposits_alpha",
	"Alpha demand deposits in the last finished block",
)
var statsPrevBlockMinerEmissionsAlphaGauge = newStatsGauge(
	"prev_block_miner_emissions_alpha",
	"Alpha miner emission captured in the last finished block",
)
var statsUsers24hGauge = newStatsGauge(
	"users_24h",
	"Unique top-level clients with contract usage in the last 24 hours",
)
var statsOnlineProvidersGauge = newStatsGauge(
	"online_providers",
	"Connected valid providers holding a public provide key",
)
var statsOnlineProvidersByCountryGauge = newStatsGaugeVec(
	"online_providers_by_country",
	"Connected valid providers holding a public provide key, per country",
	"country_code",
	"country",
)
var statsProviderRegionsGauge = newStatsGauge(
	"provider_regions",
	"Distinct regions with a connected valid public provider",
)
var statsProviderCitiesGauge = newStatsGauge(
	"provider_cities",
	"Distinct cities with a connected valid public provider",
)
var statsOnlineExtendersGauge = newStatsGauge(
	"online_extenders",
	"Active extenders with at least one active address",
)
var statsOnlineExtendersByCountryGauge = newStatsGaugeVec(
	"online_extenders_by_country",
	"Active extenders with at least one active address, per country",
	"country_code",
	"country",
)
var statsOnlineProvidersByIpFamilyGauge = newStatsGaugeVec(
	"online_providers_by_ip_family",
	"Connected valid public providers by the ip families they have proven",
	"ip_family",
)
var statsOnlineExtendersByIpFamilyGauge = newStatsGaugeVec(
	"online_extenders_by_ip_family",
	"Online extenders by the ip families they have an active address on",
	"ip_family",
)
var statsOpenContractsGauge = newStatsGauge(
	"open_contracts",
	"Transfer contracts open now",
)
var statsContracts24hGauge = newStatsGauge(
	"contracts_24h",
	"Transfer contracts created in the last 24 hours",
)
var statsOpenContractsWithExtenderGauge = newStatsGauge(
	"open_contracts_with_extender",
	"Open transfer contracts with at least one extender party",
)
var statsContractsWithExtender24hGauge = newStatsGauge(
	"contracts_with_extender_24h",
	"Transfer contracts created in the last 24 hours with at least one extender party",
)
var statsOpenDisputesGauge = newStatsGauge(
	"open_disputes",
	"Disputed contracts with no outcome yet",
)
var statsDisputes24hGauge = newStatsGauge(
	"disputes_24h",
	"Transfer contracts created in the last 24 hours that are disputed",
)

// the extender gauges are internal and are read only by
// grafana/dashboards/extenders.json.
//
// every label here is a bounded enum or a fixed rank, so the series count does
// not grow with the extender population -- which is open, so a label carrying
// an extender id per series would be unbounded. the one gauge that does name
// extenders is the leaderboard, and it is capped at statsExtenderTopContracts.
var statsExtenderGossipPendingGauge = newStatsGaugeVec(
	"extender_gossip_pending",
	"Extender gossip messages written but not yet released to the network, per kind",
	"kind",
)
var statsExtenderGossipOldestPendingGauge = newStatsGaugeVec(
	"extender_gossip_oldest_pending_seconds",
	"Age of the oldest unreleased extender gossip message, per kind",
	"kind",
)
var statsExtenderGossipReleased24hGauge = newStatsGaugeVec(
	"extender_gossip_released_24h",
	"Extender gossip messages released to the network in the last 24 hours, per kind",
	"kind",
)
var statsExtenderGossipExtenders24hGauge = newStatsGaugeVec(
	"extender_gossip_extenders_24h",
	"Distinct extenders behind the gossip messages released in the last 24 hours, per kind",
	"kind",
)

// statsExtenderContractsCounter is the one metric that names extenders, and it
// is a COUNTER rather than a windowed gauge on purpose.
//
// Prometheus already stores history, so a monotonic per-extender total gives
// every past window for free: the leaderboard is
// `topk(20, increase(...[24h]))`, the load distribution is
// `count(increase(...[24h]) > 10)`, and either can be re-asked over any range
// without a new metric. Publishing a 24h gauge instead would have frozen the
// window into the exporter.
//
// It is fed from closed hour buckets, so the accumulation is one grouped query
// per hour rather than a cumulative rescan on every refresh.
//
// The cost, stated plainly: one series per extender that has carried a
// contract, which unlike the other stats gauges is not a bounded enum. It is
// bounded in practice by the extender population and, per process, by the fact
// that a counter series lives only as long as the process. A restart resets it
// to zero, which is exactly what prometheus counter-reset detection is for --
// and `grafana.go` already gives each process its own instance label so a
// redeploy's overlapping processes never share a series.
var statsExtenderContractsCounter = newStatsCounterVec(
	"extender_contracts_total",
	"Contracts carried by each extender, accumulated from closed hour buckets",
	"extender_id",
)

// The provider latency attestations the operator accepted
// (connect/DESIGNNOTES4.md §3), read only by the extenders dashboard. The
// window gauges are bounded; the per-extender counter has the same shape,
// cost and reset behaviour as the contracts counter above, and is fed from
// the same closed hour buckets.
var statsExtenderProviderPings24hGauge = newStatsGauge(
	"extender_provider_pings_24h",
	"Provider latency attestations accepted in the last 24 hours",
)
var statsExtenderPingProviders24hGauge = newStatsGauge(
	"extender_ping_providers_24h",
	"Distinct providers whose latency attestations were accepted in the last 24 hours",
)
var statsExtenderPingedExtenders24hGauge = newStatsGauge(
	"extender_pinged_extenders_24h",
	"Distinct extenders with an accepted latency attestation in the last 24 hours",
)
var statsExtenderProviderPingsCounter = newStatsCounterVec(
	"extender_provider_pings_total",
	"Provider latency attestations accepted for each extender, accumulated from closed hour buckets",
	"extender_id",
)

// statsCounterVec is a labeled monotonic counter with the same lazy
// registration as the gauges. Unlike a gauge vec it is never replaced: a
// counter's whole contract is that a series only ever goes up, so a label that
// stops being fed keeps its last total and goes stale rather than resetting.
type statsCounterVec struct {
	counter    *prometheus.CounterVec
	registered sync.Once
}

func newStatsCounterVec(name string, help string, labelNames ...string) *statsCounterVec {
	return &statsCounterVec{
		counter: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "urnetwork",
			Subsystem: "stats",
			Name:      name,
			Help:      help,
		}, labelNames),
	}
}

func (self *statsCounterVec) add(value float64, labelValues ...string) {
	if value < 0 {
		// a counter cannot go down, and a negative here would mean the source
		// query changed shape rather than that work was undone
		return
	}
	self.registered.Do(func() {
		prometheus.MustRegister(self.counter)
	})
	self.counter.WithLabelValues(labelValues...).Add(value)
}

// statsHourBucketSettle is how long an hour must have been over before its
// rows are counted. An insert whose transaction opened before the hour turned
// commits with a create_time inside it, so counting the instant the hour ends
// can miss it -- and for a counter a miss is permanent, since the hour is
// never revisited.
const statsHourBucketSettle = 1 * time.Minute

// statsHourBucketMaxCatchup bounds how far back one refresh will walk.
const statsHourBucketMaxCatchup = 24 * time.Hour

// statsClosedHour is the newest hour that has closed and settled by `now`.
func statsClosedHour(now time.Time) time.Time {
	currentHour := now.UTC().Truncate(time.Hour)
	if now.UTC().Sub(currentHour) < statsHourBucketSettle {
		// the hour that just ended has not settled; nothing new is countable
		return currentHour.Add(-time.Hour)
	}
	return currentHour
}

// statsClosedHoursSince lists the hours after `last` that have closed and
// settled by `now`, oldest first, so each is counted exactly once. The walk
// is bounded: a collector that was down for a week does not issue a week of
// queries in one refresh, the gap is lost rather than the refresh stalling,
// and a counter gap reads as a flat line rather than a false spike.
func statsClosedHoursSince(last time.Time, now time.Time) []time.Time {
	closedHour := statsClosedHour(now)
	if oldest := closedHour.Add(-statsHourBucketMaxCatchup); last.Before(oldest) {
		last = oldest
	}
	hours := []time.Time{}
	for hour := last.Add(time.Hour); !hour.After(closedHour); hour = hour.Add(time.Hour) {
		hours = append(hours, hour)
	}
	return hours
}

// the ip family label values, in publication order. every family gauge
// publishes all three on every refresh, so a family with no members is a zero
// rather than an absent series (M4)
var statsIpFamilies = []string{"ipv4", "ipv6", "dualstack"}

func statsIpFamilyValues(ipv4 int64, ipv6 int64, dualstack int64) []statsLabeledValue {
	counts := map[string]int64{
		"ipv4":      ipv4,
		"ipv6":      ipv6,
		"dualstack": dualstack,
	}
	values := make([]statsLabeledValue, 0, len(statsIpFamilies))
	for _, ipFamily := range statsIpFamilies {
		values = append(values, statsLabeledValue{
			labelValues: []string{ipFamily},
			value:       float64(counts[ipFamily]),
		})
	}
	return values
}

var statsBlockNumberGauge = newStatsGauge(
	"block_number",
	"The current subnet block number (1-based, 7 days per block from the genesis Sunday 00:00 UTC)",
)
var statsBlockStartSecondsGauge = newStatsGauge(
	"block_start_seconds",
	"Unix time the current subnet block opened",
)
var statsBlockEndSecondsGauge = newStatsGauge(
	"block_end_seconds",
	"Unix time the current subnet block closes and the next opens",
)
var statsBlockMinerClaimsAlphaGauge = newStatsGauge(
	"block_miner_claims_alpha",
	"Alpha claimed by miners this block",
)
var statsBlockMinersClaimedGauge = newStatsGauge(
	"block_miners_claimed",
	"Distinct miner coldkeys that claimed this block",
)
var statsPrevBlockMinerClaimsAlphaGauge = newStatsGauge(
	"prev_block_miner_claims_alpha",
	"Alpha claimed by miners in the last finished block",
)
var statsPrevBlockMinersClaimedGauge = newStatsGauge(
	"prev_block_miners_claimed",
	"Distinct miner coldkeys that claimed in the last finished block",
)

// statsRaoToAlpha converts rao (1e-9 α) to whole α
func statsRaoToAlpha(rao *big.Int) float64 {
	value, _ := new(big.Float).Quo(new(big.Float).SetInt(rao), big.NewFloat(1e9)).Float64()
	return value
}

// StartStatsCollector refreshes the public stats gauges until the context
// is done. each refresh is isolated so a transient db, chain, or fetch
// error never stops the collector
func StartStatsCollector(ctx context.Context) {
	go server.HandleError(func() {
		dbTick := 0
		for {
			statsRefreshBlockClock()
			if dbTick == 0 {
				server.HandleError(func() {
					statsRefreshDb(ctx)
				})
			}
			dbTick = (dbTick + 1) % statsCollectorDbTicks
			server.HandleError(func() {
				statsRefreshChain(ctx)
			})
			server.HandleError(func() {
				statsRefreshPrice(ctx)
			})
			select {
			case <-ctx.Done():
				return
			case <-time.After(statsCollectorInterval):
			}
		}
	})
}

// statsRefreshBlockClock publishes the subnet block clock. it needs no
// data source, so it refreshes on every tick and is the first stat a fresh
// deploy exports
func statsRefreshBlockClock() {
	now := time.Now()
	blockStart := model.SubnetBlockStart(now)
	statsBlockNumberGauge.set(float64(model.SubnetBlockNumber(now)))
	statsBlockStartSecondsGauge.set(float64(blockStart.Unix()))
	statsBlockEndSecondsGauge.set(float64(blockStart.Add(model.SubnetBlockDuration).Unix()))
}

func statsRefreshDb(ctx context.Context) {
	now := time.Now()
	blockNumber := model.SubnetBlockNumber(now)

	statsTotalNetworksGauge.set(float64(model.CountNetworks(ctx)))

	// one scan of the connected valid public provider population gives the
	// per-country counts, the country count, and the region and city reach
	providerCountries := model.CountProvidersByCountry(ctx)
	byCountry := make([]statsLabeledValue, 0, len(providerCountries))
	var providers int64
	var regions int64
	var cities int64
	var providersIpv4 int64
	var providersIpv6 int64
	var providersDualstack int64
	for _, providerCountry := range providerCountries {
		byCountry = append(byCountry, statsLabeledValue{
			labelValues: []string{providerCountry.CountryCode, providerCountry.Country},
			value:       float64(providerCountry.Count),
		})
		providers += providerCountry.Count
		regions += providerCountry.RegionCount
		cities += providerCountry.CityCount
		providersIpv4 += providerCountry.Ipv4Count
		providersIpv6 += providerCountry.Ipv6Count
		providersDualstack += providerCountry.DualstackCount
	}
	statsCountriesGauge.set(float64(len(providerCountries)))
	statsOnlineProvidersGauge.set(float64(providers))
	statsOnlineProvidersByCountryGauge.replace(byCountry)
	statsProviderRegionsGauge.set(float64(regions))
	statsProviderCitiesGauge.set(float64(cities))
	// the family split comes from the same per-country scan, so the population
	// is never counted twice (M4)
	statsOnlineProvidersByIpFamilyGauge.replace(statsIpFamilyValues(
		providersIpv4,
		providersIpv6,
		providersDualstack,
	))

	// one scan of the online extenders, grouped by country with the same
	// family filters (M2). an extender whose activation resolved no country
	// counts in the population and its family but has no country to label, so
	// it is left out of the per-country gauge rather than published under an
	// empty code
	extenderCountries := model.CountExtendersByCountry(ctx)
	extendersByCountry := make([]statsLabeledValue, 0, len(extenderCountries))
	var extenders int64
	var extendersIpv4 int64
	var extendersIpv6 int64
	var extendersDualstack int64
	for _, extenderCountry := range extenderCountries {
		extenders += extenderCountry.Count
		extendersIpv4 += extenderCountry.Ipv4Count
		extendersIpv6 += extenderCountry.Ipv6Count
		extendersDualstack += extenderCountry.DualstackCount
		if extenderCountry.CountryCode == "" {
			continue
		}
		extendersByCountry = append(extendersByCountry, statsLabeledValue{
			labelValues: []string{extenderCountry.CountryCode, extenderCountry.Country},
			value:       float64(extenderCountry.Count),
		})
	}
	statsOnlineExtendersGauge.set(float64(extenders))
	statsOnlineExtendersByCountryGauge.replace(extendersByCountry)
	statsOnlineExtendersByIpFamilyGauge.replace(statsIpFamilyValues(
		extendersIpv4,
		extendersIpv6,
		extendersDualstack,
	))

	// the extender gossip and popularity gauges (grafana/dashboards/extenders.json)
	statsRefreshExtenders(ctx, now)

	// the contract gauges, each open now and over the trailing 24 hours (M3)
	contracts := model.CountContracts(ctx, now)
	statsOpenContractsGauge.set(float64(contracts.OpenContracts))
	statsOpenContractsWithExtenderGauge.set(float64(contracts.OpenContractsWithExtender))
	statsOpenDisputesGauge.set(float64(contracts.OpenDisputes))
	statsContracts24hGauge.set(float64(contracts.Contracts24h))
	statsContractsWithExtender24hGauge.set(float64(contracts.ContractsWithExtender24h))
	statsDisputes24hGauge.set(float64(contracts.Disputes24h))

	statsUsers24hGauge.set(float64(model.CountTopLevelClientsWithContractSince(ctx, now.Add(-24*time.Hour))))

	users := model.CountTopLevelClientsWithContractSince(ctx, model.SubnetBlockStart(now))
	statsBlockUsersGauge.set(float64(users))
	// freeze the running count forward; the last write before rollover is
	// the finished block's final value (see model.SetBlockUsersSnapshot)
	model.SetBlockUsersSnapshot(ctx, blockNumber, users)
	if prevUsers, ok := model.GetBlockUsersSnapshot(ctx, blockNumber-1); ok {
		statsPrevBlockUsersGauge.set(float64(prevUsers))
	}
}

// statsRefreshExtenders publishes the gossip release queue and the contract
// leaderboard.
//
// The leaderboard is replaced whole on every refresh rather than updated in
// place: an extender that falls out of the top N must stop publishing, or its
// last value would sit there forever looking current. `replace` already does
// that for a gauge vec, which is why the rank is a label rather than a series
// per extender.
func statsRefreshExtenders(ctx context.Context, now time.Time) {
	gossipKindNames := map[int]string{
		model.NetworkExtenderPublishKindRecord:     "record",
		model.NetworkExtenderPublishKindRevocation: "revocation",
	}
	pending := []statsLabeledValue{}
	oldestPending := []statsLabeledValue{}
	released := []statsLabeledValue{}
	releasedExtenders := []statsLabeledValue{}
	for _, count := range model.CountExtenderGossipPublish(ctx, now) {
		kind, ok := gossipKindNames[count.Kind]
		if !ok {
			continue
		}
		pending = append(pending, statsLabeledValue{
			labelValues: []string{kind},
			value:       float64(count.Pending),
		})
		oldestPending = append(oldestPending, statsLabeledValue{
			labelValues: []string{kind},
			value:       count.OldestPendingSeconds,
		})
		released = append(released, statsLabeledValue{
			labelValues: []string{kind},
			value:       float64(count.Released24h),
		})
		releasedExtenders = append(releasedExtenders, statsLabeledValue{
			labelValues: []string{kind},
			value:       float64(count.Extenders24h),
		})
	}
	statsExtenderGossipPendingGauge.replace(pending)
	statsExtenderGossipOldestPendingGauge.replace(oldestPending)
	statsExtenderGossipReleased24hGauge.replace(released)
	statsExtenderGossipExtenders24hGauge.replace(releasedExtenders)

	pings := model.CountExtenderProviderPings(ctx, now)
	statsExtenderProviderPings24hGauge.set(float64(pings.Pings24h))
	statsExtenderPingProviders24hGauge.set(float64(pings.Providers24h))
	statsExtenderPingedExtenders24hGauge.set(float64(pings.Extenders24h))

	statsRefreshExtenderContracts(ctx, now)
	statsRefreshExtenderPings(ctx, now)
}

// statsExtenderContractsHour is the last closed hour already added to the
// counter. Zero until the first refresh, which seeds it without backfilling:
// a counter that starts at zero and grows from now is correct, and backfilling
// would invent a step increase that never happened.
var statsExtenderContractsHour time.Time

// statsRefreshExtenderContracts adds every hour that closed since the last
// refresh to the per-extender counter.
//
// Only closed hours are counted, and each exactly once. Counting the current
// partial hour would mean adding the same contracts again on the next refresh,
// which for a counter is not a small error -- it compounds, and `increase()`
// would report several times the real number.
func statsRefreshExtenderContracts(ctx context.Context, now time.Time) {
	if statsExtenderContractsHour.IsZero() {
		// seed without backfilling
		statsExtenderContractsHour = statsClosedHour(now)
		return
	}
	for _, hour := range statsClosedHoursSince(statsExtenderContractsHour, now) {
		for _, count := range model.CountExtenderContractsByHour(ctx, hour) {
			statsExtenderContractsCounter.add(
				float64(count.Contracts),
				count.ExtenderId.String(),
			)
		}
		// advanced per hour, so a refresh that raises part way resumes at
		// the first hour it did not finish rather than recounting
		statsExtenderContractsHour = hour
	}
}

// statsExtenderPingsHour is the counter cursor of the provider pings, kept
// separately from the contracts cursor so the two counters seed and resume
// independently.
var statsExtenderPingsHour time.Time

// statsRefreshExtenderPings adds every hour that closed since the last refresh
// to the per-extender pings counter, on the same rules as the contracts.
func statsRefreshExtenderPings(ctx context.Context, now time.Time) {
	if statsExtenderPingsHour.IsZero() {
		statsExtenderPingsHour = statsClosedHour(now)
		return
	}
	for _, hour := range statsClosedHoursSince(statsExtenderPingsHour, now) {
		for _, count := range model.CountExtenderProviderPingsByHour(ctx, hour) {
			statsExtenderProviderPingsCounter.add(
				float64(count.Pings),
				count.ExtenderId.String(),
			)
		}
		statsExtenderPingsHour = hour
	}
}

func statsRefreshChain(ctx context.Context) {
	if !StEnabled() {
		return
	}
	cfg, client, err := stRequire()
	if err != nil {
		return
	}

	buybackRao, err := client.BuybackTotal(ctx)
	if err != nil {
		glog.Infof("[stats]buybackTotal read error (%s)\n", err)
		return
	}
	statsStakedAlphaGauge.set(statsRaoToAlpha(buybackRao))

	// map the block open time to a chain block through the head anchor to
	// window the mirrored events
	state, err := client.Epoch(ctx)
	if err != nil {
		glog.Infof("[stats]chain head read error (%s)\n", err)
		return
	}
	// chain blocks back from the head for a wall-clock time, clamped to
	// the chain genesis
	now := time.Now()
	chainBlockAt := func(t time.Time) uint64 {
		blocksBack := uint64(int64(now.Sub(t)/time.Second) / cfg.BlockSeconds)
		if state.HeadBlock <= blocksBack {
			return 0
		}
		return state.HeadBlock - blocksBack
	}

	blockStart := model.SubnetBlockStart(now)
	windowStartBlock := chainBlockAt(blockStart)
	deploymentKey := cfg.DeploymentKey()
	statsBlockDemandDepositsAlphaGauge.set(statsRaoToAlpha(model.SumStDepositedInBlockRangeRao(ctx, deploymentKey, windowStartBlock, state.HeadBlock+1)))
	statsBlockMinerEmissionsAlphaGauge.set(statsRaoToAlpha(model.SumStPoolSweptMeasuredInBlockRangeRao(ctx, deploymentKey, windowStartBlock, state.HeadBlock+1)))
	claimsRao, minersClaimed := model.SumStMinerClaimedInBlockRange(ctx, deploymentKey, windowStartBlock, state.HeadBlock+1)
	statsBlockMinerClaimsAlphaGauge.set(statsRaoToAlpha(claimsRao))
	statsBlockMinersClaimedGauge.set(float64(minersClaimed))

	// the last finished block, recomputed exactly from the event mirror.
	// block 1 has no predecessor
	if model.SubnetBlockGenesis.Before(blockStart) {
		prevStartBlock := chainBlockAt(blockStart.Add(-model.SubnetBlockDuration))
		statsPrevBlockDemandDepositsAlphaGauge.set(statsRaoToAlpha(model.SumStDepositedInBlockRangeRao(ctx, deploymentKey, prevStartBlock, windowStartBlock)))
		statsPrevBlockMinerEmissionsAlphaGauge.set(statsRaoToAlpha(model.SumStPoolSweptMeasuredInBlockRangeRao(ctx, deploymentKey, prevStartBlock, windowStartBlock)))
		prevClaimsRao, prevMinersClaimed := model.SumStMinerClaimedInBlockRange(ctx, deploymentKey, prevStartBlock, windowStartBlock)
		statsPrevBlockMinerClaimsAlphaGauge.set(statsRaoToAlpha(prevClaimsRao))
		statsPrevBlockMinersClaimedGauge.set(float64(prevMinersClaimed))
	}
}

// Returns the external market URL only for mainnet alpha. Testnet alpha has no
// USD market and querying the mainnet catalogue for its netuid is a false 404.
func statsAlphaPriceURL(cfg *StConfig) string {
	if cfg == nil || cfg.Netuid == 0 || cfg.Profile != stconn.ProfileMainnet {
		return ""
	}
	return fmt.Sprintf("https://api.geckoterminal.com/api/v2/networks/bittensor/pools/0-%d", cfg.Netuid)
}

// Refreshes the last-known mainnet market price without clearing it on a
// transient catalogue failure.
func statsRefreshPrice(ctx context.Context) {
	cfg := stConfig()
	url := statsAlphaPriceURL(cfg)
	if url == "" {
		return
	}
	// the subnet pool on geckoterminal; the site's browser-side fallback
	// reads the same pool through coingecko
	type poolResponse struct {
		Data struct {
			Attributes struct {
				BaseTokenPriceUsd string `json:"base_token_price_usd"`
			} `json:"attributes"`
		} `json:"data"`
	}
	response, err := server.HttpGetRequireStatusOk(ctx, url, server.NoCustomHeaders, server.ResponseJsonObject[poolResponse])
	if err != nil {
		// keep the last good value; the series only goes stale if the
		// pusher itself stops
		glog.Infof("[stats]alpha price fetch error (%s)\n", err)
		return
	}
	price, err := strconv.ParseFloat(response.Data.Attributes.BaseTokenPriceUsd, 64)
	if err != nil || price <= 0 {
		return
	}
	statsAlphaUsdGauge.set(price)
}
