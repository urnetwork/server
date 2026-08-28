package oauth

// Per-caller rate limiting for the authorization server's writable endpoints.
//
// Dynamic client registration is openly writable by design (rfc 7591): anyone
// can POST a metadata document and get a client id. Without a limit that is a
// free way to fill the client table. The oauth spec deprecates dcr in favor of
// client id metadata documents precisely because it is awkward to operate, but
// we support it for clients that predate cimd, so it needs a bound.
//
// Counter ownership and infrastructure exclusions live in server/ip_ratelimit.go
// so every writable route follows the same address-family and bypass rules.
//
// It differs in one way, deliberately: connect DELAYS a caller over the limit,
// because a connection can afford to wait. An http endpoint cannot -- holding
// the request open is itself the resource being exhausted -- so this rejects
// and the handler answers 429.

import (
	"context"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

func DefaultRateLimitSettings() *RateLimitSettings {
	return &RateLimitSettings{
		// dcr is a setup-time call: a legitimate client registers once and
		// reuses the id, so even a handful an hour is generous
		RegistrationBurstDuration: 1 * time.Hour,
		RegistrationBurstCount:    20,
	}
}

type RateLimitSettings struct {
	RegistrationBurstDuration time.Duration
	RegistrationBurstCount    int
}

var rateLimitSettings = DefaultRateLimitSettings()

// Reports whether a registration from this caller is within the limit.
//
// A redis failure allows the request. The limit protects a table from being
// filled, not a secret from being guessed, so failing open on an infrastructure
// outage is the right trade: refusing every registration because redis blipped
// would be a worse outage than the abuse it prevents.
func AllowRegistration(ctx context.Context, clientAddress string) bool {
	result, err := server.CheckRateLimitWindow(
		ctx,
		clientAddress,
		server.RateLimitWindowSettings{
			Namespace: "oauth",
			Name:      "registration",
			Duration:  rateLimitSettings.RegistrationBurstDuration,
			Limit:     int64(rateLimitSettings.RegistrationBurstCount),
		},
	)
	if err != nil {
		// A malformed address cannot be safely metered. A Redis failure still
		// fails open because this protects table growth, not authentication.
		if _, parseErr := server.NewRateLimitClient(clientAddress); parseErr != nil {
			return false
		}
		glog.Errorf("[oauth]rate limit unavailable, allowing: %s\n", err)
		return true
	}
	if !result.Allowed {
		if glog.V(1) {
			glog.Infof("[oauth]registration rate limit @%d\n", result.Count)
		}
	}
	return result.Allowed
}
