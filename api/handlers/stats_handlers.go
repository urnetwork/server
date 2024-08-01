package handlers

import (
	"net/http"

	"bringyour.com/bringyour/model"
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
