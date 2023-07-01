package router

import (
	"net/http"
	"encoding/json"

	"bringyour.com/bringyour"
)


func WarpStatus(w http.ResponseWriter, r *http.Request) {
	type WarpStatus struct {
		Version *string `json:"version,omitempty"`
		ConfigVersion *string `json:"configVersion,omitempty"`
		Status string `json:"status"`
	}

	var warpVersion *string
	if version, err := bringyour.Version(); err == nil {
		warpVersion = &version
	} else {
		warpVersion = nil
	}

	var warpConfigVersion *string
	if configVersion, err := bringyour.ConfigVersion(); err == nil {
		warpConfigVersion = &configVersion
	} else {
		warpConfigVersion = nil
	}

    result := WarpStatus{
        Version: warpVersion,
        ConfigVersion: warpConfigVersion,
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
