package handlers

import (
	"net/http"
	"time"

	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
)

// statsCacheTtl bounds how stale a per-network provider-stats response can be.
// The underlying aggregates are cheap indexed reads, but dashboards poll these
// endpoints, so a short per-network cache absorbs repeated calls. The stats
// depend only on (network_id, args), which is what makes the network-scoped
// cache safe.
const statsCacheTtl = 30 * time.Second

// GET /stats/providers — all providers in the caller network, last 24h.
func StatsProviders(w http.ResponseWriter, r *http.Request) {
	router.WrapRequireAuth(
		router.CacheWithNetworkAuth(
			model.StatsProviders,
			"api_stats_providers",
			statsCacheTtl,
		),
		w, r,
	)
}

// POST /stats/providers-last-n — all providers in the caller network, last_n hours.
func StatsProvidersLastN(w http.ResponseWriter, r *http.Request) {
	router.WrapWithInputRequireAuth(
		router.CacheWithNetworkAuthInput(
			model.StatsProvidersLastN,
			"api_stats_providers_last_n",
			statsCacheTtl,
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
