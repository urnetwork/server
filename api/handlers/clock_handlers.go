package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/urnetwork/server/model"
)

// Clock is a public, Redis-only endpoint intended for polling. The one-second
// shared cache window lets HTTP caches collapse fast repeat polls while the
// API still reflects finalized contracts essentially in real time.
func Clock(w http.ResponseWriter, r *http.Request) {
	result, ok := model.GetClock(r.Context())
	if !ok {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Retry-After", "5")
		http.Error(w, "Clock is not initialized.", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=1, stale-while-revalidate=4")
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(result); err != nil {
		return
	}
}
