package api

import (
	"testing"
)

// §7.8's auth key routes take the network credential (Network until TEAM-M6-03's
// roles): a client token, a sidecar's strict_exp token included, never creates,
// lists or revokes a key.
func TestAuthKeyRoutesTakeTheNetworkCredential(t *testing.T) {
	served := map[string]bool{}
	for _, route := range Routes() {
		served[routeAccessKey(route)] = true
	}
	for _, routeKey := range []string{
		"GET /network/auth-keys",
		"POST /network/auth-keys",
		"POST /network/auth-key-revoke",
	} {
		if !served[routeKey] {
			t.Errorf("%s is not served", routeKey)
		}
		if access, ok := routeAccessByRoute[routeKey]; !ok || access != routeAccessNetwork {
			t.Errorf("%s is classed %d, want Network", routeKey, access)
		}
		if strictExpRoutes[routeKey] {
			t.Errorf("%s admits a strict_exp bearer", routeKey)
		}
	}
}
