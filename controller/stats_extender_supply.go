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

func statsRefreshExtenderSupply(ctx context.Context, now time.Time) {
	supply := model.CountExtenderSupply(ctx, now)
	statsExtendersCreated24hGauge.set(float64(supply.Created))
	statsExtendersRevoked24hGauge.set(float64(supply.Revoked))
}
