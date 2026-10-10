// Local dispatch records bounded operations without retaining URL contents.
package localclient

import (
	"strings"

	"github.com/urnetwork/server/v2026/session"
)

// Typed and raw callers use the same operation buckets. The public-key bucket
// covers the existing authenticated path until the separate parity fix lands.
func authorityStateQuerySource(method, path string) session.StateQuerySource {
	switch method + " " + path {
	case "POST /connect/control":
		return session.StateQueryHostedControl
	case "POST /network/auth-client":
		return session.StateQueryHostedMint
	case "POST /network/remove-client":
		return session.StateQueryHostedRetire
	case "POST /network/find-providers2", "POST /network/find-provider-locations", "POST /network/find-locations", "GET /network/provider-locations":
		return session.StateQueryHostedDiscovery
	case "GET /auth/refresh":
		return session.StateQueryHostedRefresh
	case "GET /network/clients", "GET /network/peers", "GET /subscription/balance":
		return session.StateQueryHostedRead
	}
	if method == "GET" && strings.HasPrefix(path, "/key/") {
		return session.StateQueryHostedPublicKey
	}
	return session.StateQueryHostedOther
}
