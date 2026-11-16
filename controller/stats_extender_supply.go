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

func statsRefreshExtenderSupply(ctx context.Context, now time.Time) {
	supply := model.CountExtenderSupply(ctx, now)
	statsExtendersCreated24hGauge.set(float64(supply.Created))
	statsExtendersRevoked24hGauge.set(float64(supply.Revoked))
	statsWebRtcOnlyExtendersGauge.set(float64(model.CountActiveWebRtcOnlyNetworkExtenders(ctx, nil)))
}
