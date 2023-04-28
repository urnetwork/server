package handlers

import (
    "net/http"
    "encoding/json"
)


func Health(w http.ResponseWriter, r *http.Request) {
    type HealthResult struct {
        Status string `json:"status"`
    }

    result := HealthResult{
        Status: "ok",
    }

    responseJson, err := json.Marshal(result)
    if err != nil {
        http.Error(w, err.Error(), http.StatusInternalServerError)
        return
    }
    w.Header().Set("Content-Type", "application/json")
    w.Write(responseJson)
}

