package handlers

import (
	"net/http"
	"time"

	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
)

// statsCacheTtl bounds how stale a fresh per-network provider-stats response
// can be. Dashboards poll these endpoints, so a short per-network cache absorbs
// repeated calls. The stats depend only on (network_id, args), which makes
// the network-scoped cache safe. A provider list whose refresh is slower than
// a miss's wait answers its last published value instead, for up to
// router.handlerCacheLastGoodTtl; its created_time says how old it is.
const statsCacheTtl = 30 * time.Second

// A slow provider-list fill keeps its lease while queries run, and runs to
// the end of this budget even when its caller leaves first (the lb cuts an
// upstream at 30 s, and clients give up sooner), so the next request finds
// the list. This permits a fill longer than the 30s cache lease while bounding
// repeated renewal. Response TTL is unchanged.
const providerListStatsFillTimeout = 2 * time.Minute

// GET /stats/providers — all providers in the caller network, last 24h.
func StatsProviders(w http.ResponseWriter, r *http.Request) {
	router.WrapRequireAuth(
		router.CacheWithNetworkAuthRenewingFill(
			model.StatsProviders,
			"api_stats_providers",
			statsCacheTtl,
			providerListStatsFillTimeout,
		),
		w, r,
	)
}

// POST /stats/providers-last-n — all providers in the caller network, last_n hours.
func StatsProvidersLastN(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(
		router.CacheWithNetworkAuthInputRenewingFill(
			model.StatsProvidersLastN,
			"api_stats_providers_last_n",
			statsCacheTtl,
			providerListStatsFillTimeout,
		),
		w, r,
	)
}

// POST /stats/provider-last-n — single provider drill-down, last_n hours.
func StatsProvider(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(
		router.CacheWithNetworkAuthInput(
			model.StatsProvider,
			"api_stats_provider_last_n",
			statsCacheTtl,
		),
		w, r,
	)
}

// POST /stats/providers-overview-last-n — network aggregate time series.
func StatsProvidersOverview(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(
		router.CacheWithNetworkAuthInput(
			model.StatsProvidersOverview,
			"api_stats_providers_overview_last_n",
			statsCacheTtl,
		),
		w, r,
	)
}

// --- legacy aliases (kept for existing callers during the route migration) ---

// GET /stats/providers-overview-last-90
func StatsProvidersOverviewLast90(w http.ResponseWriter, r *http.Request) {
	router.WrapRequireAuth(
		router.CacheWithNetworkAuth(
			model.StatsProvidersOverviewLast90,
			"api_stats_providers_overview_last_90",
			statsCacheTtl,
		),
		w, r,
	)
}

// POST /stats/provider-last-90
func StatsProviderLast90(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(
		router.CacheWithNetworkAuthInput(
			model.StatsProviderLast90,
			"api_stats_provider_last_90",
			statsCacheTtl,
		),
		w, r,
	)
}
