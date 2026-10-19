package router

import (
	"net/http"
)

// TemporarilyUnavailable short-circuits a route with 503 Service Unavailable
// before any auth, cache, or model work runs, so the endpoint sheds its entire
// cost while staying registered. The replaced handler is kept as an argument
// so re-enabling is deleting the wrapper at the route site.
func TemporarilyUnavailable(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// ask pollers to back off; this wrapper exists to shed load
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error":{"message":"This api is temporarily unavailable."}}`))
	}
}
