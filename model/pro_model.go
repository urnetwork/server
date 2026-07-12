package model

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/urnetwork/server"
)

// pro_model is the single place a network's Pro entitlement is tracked. Nothing
// else should infer Pro from balances, subscriptions, or payments.
//
// Source of truth (postgres): a network is Pro iff it has an IN-WINDOW
// transfer_balance with pro = true. The monthly Pro grant and subscription
// activation both write pro = true; the data-only grants (data codes, the daily
// free grant, referral bonuses) write pro = false, so buying data never makes a
// network Pro.
//
// Entitlement is TIME-based, not byte-based: a subscriber who spends their whole
// 10 TiB is still Pro until the balance window ends. (The `active` column on
// transfer_balance is GENERATED AS (0 < balance_byte_count) -- bytes remaining --
// and must not be used for this.) The monthly grant's window runs one day past the
// end of the month, so a renewing subscriber never has a gap, and a lapsed one
// drops to free a day after their last period.
//
// The lookup sits on hot paths (client auth, connect nomination, proxy config), so
// it is cached in redis per network. Writers call UpdateProNetwork so an upgrade is
// visible immediately; the TTL bounds staleness for the case with no writer -- a
// balance simply expiring.

// The cache is TWO TIERS: a small in-process map in front of redis.
//
// The in-process tier exists for the PER-CONNECTION callers. The proxy checks a
// network's entitlement on every SOCKS/HTTP connection it accepts, and a scraping
// fan-out opens a lot of connections. A redis round-trip per connection (~0.5-1ms of
// pure latency, plus the load) is not something to put on that path. A map read is
// free, so the entitlement check costs nothing where it is checked most.
//
// Staleness is bounded by TWO ttls, and they are deliberately different:
//
//   - ProLocalCacheTtl (short): the window in which ONE process can serve a stale
//     answer. An upgrade calls UpdateProNetwork, which clears this process's entry --
//     but it cannot reach into OTHER processes, so their local entries live out this
//     ttl. It is therefore the worst-case delay between paying and, say, SOCKS being
//     issued on some other proxy instance. Keep it small.
//   - ProCacheTtl (longer): the shared redis window. This is what bounds the case with
//     no writer at all -- a Pro balance simply expiring -- since nothing calls
//     UpdateProNetwork then.
const ProCacheTtl = 60 * time.Second
const ProLocalCacheTtl = 5 * time.Second

type proLocalEntry struct {
	pro    bool
	expiry time.Time
}

var proLocalCacheMutex sync.Mutex
var proLocalCache = map[server.Id]proLocalEntry{}

func proNetworkKey(networkId server.Id) string {
	return fmt.Sprintf("pro:%s", networkId)
}

func getProNetworkLocal(networkId server.Id) (pro bool, ok bool) {
	proLocalCacheMutex.Lock()
	defer proLocalCacheMutex.Unlock()

	entry, found := proLocalCache[networkId]
	if !found || !server.NowUtc().Before(entry.expiry) {
		return false, false
	}
	return entry.pro, true
}

func setProNetworkLocal(networkId server.Id, pro bool) {
	proLocalCacheMutex.Lock()
	defer proLocalCacheMutex.Unlock()

	// Bound the map. This is a cache, not a registry: if a single process sees more
	// distinct networks than this between evictions, dropping the whole thing is fine
	// and strictly better than growing without limit. Entries are cheap to rebuild.
	if proLocalCacheMaxSize <= len(proLocalCache) {
		proLocalCache = map[server.Id]proLocalEntry{}
	}

	proLocalCache[networkId] = proLocalEntry{
		pro:    pro,
		expiry: server.NowUtc().Add(ProLocalCacheTtl),
	}
}

const proLocalCacheMaxSize = 8192

func clearProNetworkLocal(networkId server.Id) {
	proLocalCacheMutex.Lock()
	defer proLocalCacheMutex.Unlock()
	delete(proLocalCache, networkId)
}

// IsProNetwork reports whether a network currently holds the Pro entitlement.
//
// Reads through the in-process cache, then redis, and loads from the db only on a
// miss of both. Safe to call on a hot path -- that is what the local tier is for.
func IsProNetwork(ctx context.Context, networkId server.Id) bool {
	if pro, ok := getProNetworkLocal(networkId); ok {
		return pro
	}
	if pro, ok := getProNetworkCached(ctx, networkId); ok {
		setProNetworkLocal(networkId, pro)
		return pro
	}
	pro := loadProNetwork(ctx, networkId)
	setProNetworkCached(ctx, networkId, pro)
	setProNetworkLocal(networkId, pro)
	return pro
}

// UpdateProNetwork recomputes a network's entitlement from the db and refreshes both
// cache tiers, returning the new value. Call this whenever a network's pro balances
// change -- subscription activation, the monthly Pro grant, an x402 payment -- so the
// upgrade takes effect immediately instead of after a ttl.
//
// Note this can only clear THIS process's local tier. Other processes keep their own
// entry until ProLocalCacheTtl expires, which is why that ttl is short.
func UpdateProNetwork(ctx context.Context, networkId server.Id) bool {
	pro := loadProNetwork(ctx, networkId)
	setProNetworkCached(ctx, networkId, pro)
	setProNetworkLocal(networkId, pro)
	return pro
}

// UpdateProNetworks refreshes a batch of networks, used by the monthly Pro grant
// which upgrades many networks at once.
func UpdateProNetworks(ctx context.Context, networkIds ...server.Id) {
	for _, networkId := range networkIds {
		UpdateProNetwork(ctx, networkId)
	}
}

// InvalidateProNetwork drops the cached entitlement from BOTH tiers so the next read
// reloads it from the db.
func InvalidateProNetwork(ctx context.Context, networkId server.Id) {
	clearProNetworkLocal(networkId)
	server.Redis(ctx, func(r server.RedisClient) {
		r.Del(ctx, proNetworkKey(networkId))
	})
}

// loadProNetwork reads the entitlement from the source of truth.
func loadProNetwork(ctx context.Context, networkId server.Id) (pro bool) {
	server.Db(ctx, func(conn server.PgConn) {
		now := server.NowUtc()
		result, err := conn.Query(
			ctx,
			`
				SELECT EXISTS (
					SELECT 1
					FROM transfer_balance
					WHERE
						network_id = $1 AND
						pro = true AND
						start_time <= $2 AND
						$2 < end_time
				)
			`,
			networkId,
			now,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&pro))
			}
		})
	})
	return
}

func getProNetworkCached(ctx context.Context, networkId server.Id) (pro bool, ok bool) {
	server.Redis(ctx, func(r server.RedisClient) {
		value, err := r.Get(ctx, proNetworkKey(networkId)).Result()
		if err != nil {
			// a miss, or a cache error -- either way fall back to the db, which is
			// the source of truth. The cache must never be able to deny Pro.
			return
		}
		pro = (value == "1")
		ok = true
	})
	return
}

func setProNetworkCached(ctx context.Context, networkId server.Id, pro bool) {
	value := "0"
	if pro {
		value = "1"
	}
	server.Redis(ctx, func(r server.RedisClient) {
		r.Set(ctx, proNetworkKey(networkId), value, ProCacheTtl)
	})
}

// ----- the proxy hot path -----
//
// The proxy checks a network's entitlement on EVERY SOCKS/HTTP connection it accepts,
// and it only knows the connection's proxy id. Turning that into an answer needs two
// lookups: proxy -> network, and network -> pro. Both are cached, so a connection
// costs no I/O at all once warm.
//
// The proxy -> network mapping is cached FOREVER, deliberately: a proxy client belongs
// to exactly one network for its whole life, so the mapping is immutable and a ttl
// would only buy pointless re-queries. Only the pro answer can change, and that is
// what the (short) local ttl above is for.

var proxyNetworkCacheMutex sync.Mutex
var proxyNetworkCache = map[server.Id]server.Id{}

const proxyNetworkCacheMaxSize = 16384

// networkIdForProxy resolves the network behind a proxy client, memoized in-process.
func networkIdForProxy(ctx context.Context, proxyId server.Id) (networkId server.Id, ok bool) {
	proxyNetworkCacheMutex.Lock()
	cached, found := proxyNetworkCache[proxyId]
	proxyNetworkCacheMutex.Unlock()
	if found {
		return cached, true
	}

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT network_client.network_id
				FROM proxy_client
				INNER JOIN network_client ON
					network_client.client_id = proxy_client.client_id
				WHERE proxy_client.proxy_id = $1
			`,
			proxyId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&networkId))
				ok = true
			}
		})
	})

	if !ok {
		return server.Id{}, false
	}

	proxyNetworkCacheMutex.Lock()
	// bound it; see the pro cache above for why dropping the whole map is fine
	if proxyNetworkCacheMaxSize <= len(proxyNetworkCache) {
		proxyNetworkCache = map[server.Id]server.Id{}
	}
	proxyNetworkCache[proxyId] = networkId
	proxyNetworkCacheMutex.Unlock()

	return networkId, true
}

// ProxyFeatureAllowed reports whether the network behind a proxy client may USE a proxy
// feature (model.FeatureSocksProxy, FeatureWireguardProxy, ...) right now.
//
// This is the per-connection gate, and it is built to be free:
//
//   - While enforce_features is dark it returns true immediately, touching neither
//     redis nor the db. Shipping the gate costs nothing until the rollout.
//   - Once on, both lookups it needs are served from in-process caches, so an accepted
//     connection still does no I/O.
//
// It FAILS OPEN. If the proxy cannot be resolved to a network, the connection is
// allowed: a lookup failure must never masquerade as "you are not entitled to this",
// which would look to a paying customer exactly like their plan being revoked.
//
// Note this is enforcement at the CONNECTION. Credential issuance is gated separately
// in AuthNetworkClient (a free client is issued no SOCKS url and no WireGuard config);
// this is what stops a client that already HOLDS credentials from continuing to use
// them after its plan no longer includes the feature.
func ProxyFeatureAllowed(ctx context.Context, proxyId server.Id, feature string) bool {
	// Dark, or no pro.yml at all -> ALLOWED, with zero i/o. This runs per connection, so
	// the not-enforcing case must not touch redis or the db.
	c := Pro()
	if !c.EnforceFeatures {
		return true
	}

	networkId, ok := networkIdForProxy(ctx, proxyId)
	if !ok {
		return true
	}

	return c.FeatureAllowed(IsProNetwork(ctx, networkId), feature)
}
