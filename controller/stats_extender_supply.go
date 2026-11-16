package controller

import (
	"context"
	"time"

	"github.com/urnetwork/server/model"
)

// The extender supply gauges (monitor/SIGNALS.md §28.5), read by the extender
// health dashboard (grafana/dashboards/extender-health.json) and the monitor's
// extender-supply signal, beside urnetwork_stats_online_extenders.
//
// From 2026-11-02 to 11-11 revocations outnumbered creations about 2 to 1, and
// 75% of the revoked records died within 72 hours of their creation, from
// uptime-probe failures the prober itself caused
// (factury/research/extender-health-2026-11-11.md §4 item 5). The online count
// alone shows only the net of the two, so a fleet that churns through its
// extenders reads the same as one that holds them. Two plain counts with no
// label, so the series count is fixed.
var statsExtendersCreated24hGauge = newStatsGauge(
	"extenders_created_24h",
	"Extender identity keys first activated in the last 24 hours",
)
var statsExtendersRevoked24hGauge = newStatsGauge(
	"extenders_revoked_24h",
	"Extenders that lost their last active address in the last 24 hours and have not activated again since",
)

// The extenders accepted on the webrtc carrier without tcp (connect/EXTENDER.md
// S8), the population the activation's cap bounds
// (ExtenderActivateSettings.MaxWebRtcOnlyExtenders), read by the extenders
// dashboard beside the activations the cap refused
// (urnetwork_extender_activations_total{stage="webrtc",outcome="cap"}), so a
// cap that binds is seen, and how close the population is to it. One plain
// count with no label.
var statsWebRtcOnlyExtendersGauge = newStatsGauge(
	"webrtc_only_extenders",
	"Active extenders with an active address accepted on the webrtc carrier without tcp, the population the activation's cap bounds",
)

// How much of the active extender pool one network holds (sn WHITEPAPER
// §8.6.3, model.ExtenderConcentration), read by the extenders dashboard beside
// urnetwork_stats_online_extenders. The §8.6 pool pays per unit, an
// extender's ipv4 /29 or ipv6 /48, so the top network's share of units is what
// it would take of the pool, and the whitepaper names that share growing as
// the condition for coarser units or a weighting by use. On 2026-11-16 one
// network held 1,803 of 3,370 units and two networks of one fork build 3,033.
// Four plain counts with no label, so the series count is fixed and no network
// is named.
var statsExtenderNetworksGauge = newStatsGauge(
	"extender_networks",
	"Networks with an active extender that has an active address",
)
var statsExtenderTopNetworkExtendersGauge = newStatsGauge(
	"extender_top_network_extenders",
	"Active extenders with an active address of the network that holds the most",
)
var statsExtenderUnitsGauge = newStatsGauge(
	"extender_units",
	"Extender pool units holding an active extender: an extender's active ipv4 address's /29, else its active ipv6 address's /48",
)
var statsExtenderTopNetworkUnitsGauge = newStatsGauge(
	"extender_top_network_units",
	"Extender pool units held by the network whose extenders hold the most",
)

func statsRefreshExtenderSupply(ctx context.Context, now time.Time) {
	supply := model.CountExtenderSupply(ctx, now)
	statsExtendersCreated24hGauge.set(float64(supply.Created))
	statsExtendersRevoked24hGauge.set(float64(supply.Revoked))
	statsWebRtcOnlyExtendersGauge.set(float64(model.CountActiveWebRtcOnlyNetworkExtenders(ctx, nil)))
	concentration := model.CountExtenderConcentration(ctx)
	statsExtenderNetworksGauge.set(float64(concentration.NetworkCount))
	statsExtenderTopNetworkExtendersGauge.set(float64(concentration.TopNetworkExtenderCount))
	statsExtenderUnitsGauge.set(float64(concentration.UnitCount))
	statsExtenderTopNetworkUnitsGauge.set(float64(concentration.TopNetworkUnitCount))
}
