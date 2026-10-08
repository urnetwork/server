// Local dispatch records bounded operations without retaining URL contents.
package localclient

import (
	"strings"

	"github.com/urnetwork/server/v2026/jwt"
)

// Typed and raw callers use the same operation buckets. The public-key bucket
// covers the existing authenticated path until the separate parity fix lands.
func authorityStateQuerySource(method, path string) jwt.StateQuerySource {
	switch method + " " + path {
	case "POST /connect/control":
		return jwt.StateQueryHostedControl
	case "POST /network/auth-client":
		return jwt.StateQueryHostedMint
	case "POST /network/remove-client":
		return jwt.StateQueryHostedRetire
	case "POST /network/find-providers2", "POST /network/find-provider-locations", "POST /network/find-locations", "GET /network/provider-locations":
		return jwt.StateQueryHostedDiscovery
	case "GET /auth/refresh":
		return jwt.StateQueryHostedRefresh
	case "GET /network/clients", "GET /network/peers", "GET /subscription/balance":
		return jwt.StateQueryHostedRead
	}
	if method == "GET" && strings.HasPrefix(path, "/key/") {
		return jwt.StateQueryHostedPublicKey
	}
	return jwt.StateQueryHostedOther
}
