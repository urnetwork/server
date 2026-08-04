package oauth

// Per-caller rate limiting for the authorization server's writable endpoints.
//
// Dynamic client registration is openly writable by design (rfc 7591): anyone
// can POST a metadata document and get a client id. Without a limit that is a
// free way to fill the client table. The oauth spec deprecates dcr in favor of
// client id metadata documents precisely because it is awkward to operate, but
// we support it for clients that predate cimd, so it needs a bound.
//
// This follows the connect transport's rate limit (server/connect/
// transport_rate_limit.go): a redis counter per caller ip per window, keyed
// with a per-ip hash tag so the counters for different ips spread across
// cluster slots instead of piling onto one, and with the same
// `IsLimitExcludeAddr` exemption for our own infrastructure.
//
// It differs in one way, deliberately: connect DELAYS a caller over the limit,
// because a connection can afford to wait. An http endpoint cannot -- holding
// the request open is itself the resource being exhausted -- so this rejects
// and the handler answers 429.

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/netip"
	"time"

	"github.com/redis/go-redis/v9"

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
	return allowBurst(
		ctx,
		clientAddress,
		"reg",
		rateLimitSettings.RegistrationBurstDuration,
		rateLimitSettings.RegistrationBurstCount,
	)
}

func allowBurst(
	ctx context.Context,
	clientAddress string,
	name string,
	burstDuration time.Duration,
	burstCount int,
) bool {
	clientIp, _, err := server.SplitClientAddress(clientAddress)
	if err != nil {
		// an unparseable caller address cannot be counted, and letting it
		// through would be an unmetered path; refuse instead
		return false
	}
	clientAddr, err := netip.ParseAddr(clientIp)
	if err != nil {
		return false
	}

	// our own infrastructure is not rate limited against itself
	if server.IsLimitExcludeAddr(clientAddr) {
		return true
	}

	clientIpHash := server.ClientIpHashForAddr(clientAddr)
	clientIpHashHex := hex.EncodeToString(clientIpHash[:])

	// the hash tag is per ip so counters spread across cluster slots; a shared
	// tag would make one slot the hot spot for every caller
	burstKey := fmt.Sprintf(
		"{oauth_%s}%s_%d",
		clientIpHashHex,
		name,
		server.NowUtc().Unix()/int64(burstDuration/time.Second),
	)

	var count int64
	server.Redis(ctx, func(r server.RedisClient) {
		var burstCmd *redis.IntCmd
		r.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			burstCmd = pipe.Incr(ctx, burstKey)
			// the window key expires on its own, so nothing accumulates
			pipe.Expire(ctx, burstKey, burstDuration)
			return nil
		})
		count, err = burstCmd.Result()
	})
	if err != nil {
		glog.Errorf("[oauth]rate limit unavailable, allowing: %s\n", err)
		return true
	}

	if int64(burstCount) < count {
		if glog.V(1) {
			glog.Infof("[oauth][%s]%s rate limit @%d\n", clientIpHashHex, name, count)
		}
		return false
	}

	return true
}
