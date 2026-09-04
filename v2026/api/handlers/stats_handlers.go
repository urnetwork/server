package handlers

import (
	"net/http"
	"time"

	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/router"
)

func StatsLast90(w http.ResponseWriter, r *http.Request) {
	statsLast90Json := model.GetExportedStatsJson(r.Context(), 90)
	if statsLast90Json == nil {
		http.Error(w, "Could not fetch stats.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(*statsLast90Json))
}

// StatsProvidersMap serves the cached provider-density map:
// country code -> region -> { provider_count, lat, lon }. Like StatsLast90 it
// serves a redis-exported blob (stats.providers-map, no ttl) refreshed by the
// ExportProvidersMap task, so it never hits the db on the request path.
func StatsProvidersMap(w http.ResponseWriter, r *http.Request) {
	providersMapJson := model.GetExportedProvidersMapJson(r.Context())
	if providersMapJson == nil {
		http.Error(w, "Could not fetch providers map.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(*providersMapJson))
}

func TransferStats(w http.ResponseWriter, r *http.Request) {
	// paid/unpaid totals move only on sweeps and payouts, and GetTransferStats
	// aggregates three tables per call — serve repeat polls from a per-network
	// cache (the totals depend only on network_id)
	router.WrapRequireAuth(
		router.CacheWithNetworkAuth(
			controller.TransferStats,
			"api_transfer_stats",
			60*time.Second,
		),
		w,
		r,
	)
}
