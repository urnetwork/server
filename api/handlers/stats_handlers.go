package handlers

import (
    "net/http"
    
    "bringyour.com/bringyour/model"
)


func StatsLast90(w http.ResponseWriter, r *http.Request) {
    statsLast90Json := model.GetExportedStatsJson(90)
    w.Header().Set("Content-Type", "application/json")
    w.Write(statsLast90Json)
}

