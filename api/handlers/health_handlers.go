package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/urnetwork/server/model"
)

// Health is the public network health status that ur.io shows: healthy, the
// level and reason an operator set, or unknown when the status cannot be read
// (model/network_health_model.go). Redis only, through the process cache, and
// shared caches may keep it for thirty seconds; unknown is never cached, so
// the next page load asks again.
func Health(w http.ResponseWriter, r *http.Request) {
	result := model.GetNetworkHealth(r.Context())
	if result.Status == model.NetworkHealthUnknown {
		w.Header().Set("Cache-Control", "no-store")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=30")
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(result); err != nil {
		return
	}
}
