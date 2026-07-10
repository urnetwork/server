package handlers

import (
	"net/http"
	"time"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
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

func TransferStats(w http.ResponseWriter, r *http.Request) {
	// paid/unpaid totals move only on sweeps and payouts, and GetTransferStats
	// aggregates three tables per call — serve repeat polls from cache
	router.WrapRequireAuth(
		router.CacheWithAuth(
			controller.TransferStats,
			"api_transfer_stats",
			60*time.Second,
		),
		w,
		r,
	)
}
