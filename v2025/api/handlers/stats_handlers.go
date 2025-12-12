package handlers

import (
	"net/http"

	"github.com/urnetwork/server/v2025/controller"
	"github.com/urnetwork/server/v2025/model"
	"github.com/urnetwork/server/v2025/router"
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
	router.WrapRequireAuth(controller.TransferStats, w, r)
}
