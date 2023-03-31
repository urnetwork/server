package handlers

import (
    "net/http"

    "bringyour.com/bringyour/model"
)


func StatsLast90(w http.ResponseWriter, r *http.Request) {
    statsLast90Json := model.GetExportedStatsJson(90)


    // responseBody, err := json.Marshal(ApiResponse{
    //     Success: true,
    // })
    // if err != nil {
    //     http.Error(w, err.Error(), http.StatusInternalServerError)
    //     return
    // }
    w.Header().Set("Content-Type", "application/json")
    w.Write(statsLast90Json)
}

