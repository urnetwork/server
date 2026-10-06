package model

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urnetwork/server/v2026"
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
//     answer. An upgrade calls UpdateProNetwork, which replaces this process's entry --
//     but it cannot reach into OTHER processes, so their local entries live out this
//     ttl. It is therefore the worst-case delay between paying and, say, SOCKS being
//     issued on some other proxy instance. Keep it small.
//   - ProCacheTtl (longer): the shared redis window. This is what bounds the case with
//     no writer at all -- a Pro balance simply expiring -- since nothing calls
//     UpdateProNetwork then.
const ProCacheTtl = 60 * time.Second
const ProLocalCacheTtl = 5 * time.Second

// Writes to both tiers are ordered. A read loads the entitlement and then writes it to
// the cache, and any number of reads and refreshes can be in flight for one network at
// once; without an order, a read that loaded "not Pro" just before an upgrade committed
// could write that over the refresh made after the commit, and every process would
// read "not Pro" for up to ProCacheTtl.
//
// So every entry carries a version: the postgres time at which the transaction of the
// read that produced it began (transaction_timestamp(), in microseconds). That time
// comes before the snapshot the read sees, so a read that begins after a commit has a
// later version than any read whose snapshot missed the commit. Each tier keeps the
// entry with the newest version: redis compares and sets in one script, and the local
// tier compares under its mutex.
//
// The comparison needs the newer entry to still be in redis, and an entry lives
// ProCacheTtl. A read whose write lands after that entry is gone (a stall of a minute
// or more after its snapshot, or redis evicting the entry early) can still put back an
// older entitlement, for at most ProCacheTtl, the bound the cache always had.

// A network's entitlement, with the version of the read that produced it.
type proEntitlement struct {
	pro bool
	// microseconds since the unix epoch, postgres clock; see the ordering note above
	version int64
}

type proLocalEntry struct {
	entitlement proEntitlement
	expiry      time.Time
}

var proLocalCacheMutex sync.Mutex
var proLocalCache = map[server.Id]proLocalEntry{}

// The entry is "<pro>:<version>", with pro "1" or "0". The key changed when the
// version was added, so a process still on the unversioned "pro:<network id>" key
// never reads one of these, and the other way round.
func proNetworkKey(networkId server.Id) string {
	return fmt.Sprintf("pro_versioned:%s", networkId)
}

// The redis value of an entitlement, "<pro>:<version>".
func formatProEntitlement(entitlement proEntitlement) string {
	pro := "0"
	if entitlement.pro {
		pro = "1"
	}
	return pro + ":" + strconv.FormatInt(entitlement.version, 10)
}

// The entitlement of a redis value, ok false for a value in any other format.
func parseProEntitlement(value string) (entitlement proEntitlement, ok bool) {
	pro, versionString, found := strings.Cut(value, ":")
	if !found || (pro != "0" && pro != "1") {
		return proEntitlement{}, false
	}
	version, err := strconv.ParseInt(versionString, 10, 64)
	if err != nil {
		return proEntitlement{}, false
	}
	return proEntitlement{
		pro:     pro == "1",
		version: version,
	}, true
}

// This process's live entry for the network, ok false when it holds none.
func getProNetworkLocal(networkId server.Id) (entitlement proEntitlement, ok bool) {
	proLocalCacheMutex.Lock()
	defer proLocalCacheMutex.Unlock()

	entry, found := proLocalCache[networkId]
	if !found || !server.NowUtc().Before(entry.expiry) {
		return proEntitlement{}, false
	}
	return entry.entitlement, true
}

// Stores the entitlement unless this process holds a live entry with a newer
// version.
func setProNetworkLocal(networkId server.Id, entitlement proEntitlement) {
	proLocalCacheMutex.Lock()
	defer proLocalCacheMutex.Unlock()

	now := server.NowUtc()
	if entry, found := proLocalCache[networkId]; found && now.Before(entry.expiry) && entitlement.version < entry.entitlement.version {
		return
	}

	// Bound the map. This is a cache, not a registry: if a single process sees more
	// distinct networks than this between evictions, dropping the whole thing is fine
	// and strictly better than growing without limit. Entries are cheap to rebuild.
	if proLocalCacheMaxSize <= len(proLocalCache) {
		proLocalCache = map[server.Id]proLocalEntry{}
	}

	proLocalCache[networkId] = proLocalEntry{
		entitlement: entitlement,
		expiry:      now.Add(ProLocalCacheTtl),
	}
}

const proLocalCacheMaxSize = 8192

// IsProNetwork reports whether a network currently holds the Pro entitlement.
//
// Reads through the in-process cache, then redis, and loads from the db only on a
// miss of both. Safe to call on a hot path -- that is what the local tier is for.
func IsProNetwork(ctx context.Context, networkId server.Id) bool {
	if entitlement, ok := getProNetworkLocal(networkId); ok {
		return entitlement.pro
	}
	if entitlement, ok := getProNetworkCached(ctx, networkId); ok {
		setProNetworkLocal(networkId, entitlement)
		return entitlement.pro
	}
	return refreshProNetwork(ctx, networkId)
}

// UpdateProNetwork recomputes a network's entitlement from the db and refreshes both
// cache tiers, returning the new value. Call this whenever a network's pro balances
// change -- subscription activation, the monthly Pro grant, an x402 payment -- so the
// upgrade takes effect immediately instead of after a ttl. A read that loaded before
// the change cannot overwrite the refresh afterwards (see the ordering note above).
//
// Call it after the transaction that changed the balances commits, never inside it.
// It reads on its own connection, so inside the tx it would load the entitlement from
// before the change and cache that for up to ProCacheTtl, and a tx that then rolled
// back would still have written the cache. InTx writers return what changed and leave
// the refresh to whoever commits.
//
// Note this can only replace this process's local tier. Other processes keep their own
// entry until ProLocalCacheTtl expires, which is why that ttl is short.
func UpdateProNetwork(ctx context.Context, networkId server.Id) bool {
	return refreshProNetwork(ctx, networkId)
}

// UpdateProNetworks refreshes a batch of networks, used by the monthly Pro grant
// which upgrades many networks at once.
func UpdateProNetworks(ctx context.Context, networkIds ...server.Id) {
	for _, networkId := range networkIds {
		UpdateProNetwork(ctx, networkId)
	}
}

// IsProNetworkFresh reads the entitlement from the source of truth (the db), bypassing
// the read-through local/redis cache, and refreshes the cache with the result. It returns
// the same value UpdateProNetwork does.
//
// Use this — NOT IsProNetwork — whenever the result is baked into a durable artifact,
// above all a ByJwt (which lasts expiryDuration = 30 days). IsProNetwork is a self-healing
// read-through cache: a stale `false` (e.g. a node whose entry predates the upgrade) is
// harmless for an ephemeral feature check because it corrects within the ttl. But stamping
// that stale `false` into a 30-day token freezes it for the token's whole lifetime — the
// network silently loses Pro until the next refresh re-issues the token. A token is not
// ephemeral, so it must read the source of truth.
func IsProNetworkFresh(ctx context.Context, networkId server.Id) bool {
	return UpdateProNetwork(ctx, networkId)
}

// Makes the cached entitlement current in both tiers. It reloads and stores the
// entitlement (UpdateProNetwork) rather than deleting the entries: deleting would
// also drop the newest version, and a read that loaded before the change could then
// write its older entitlement back.
func InvalidateProNetwork(ctx context.Context, networkId server.Id) {
	UpdateProNetwork(ctx, networkId)
}

// Reads both cache tiers for a network without loading
// the entitlement or filling either tier, for tests that pin when a writer refreshes
// the cache. An ok is false when that tier holds no live entry.
func Testing_ProNetworkCacheEntries(ctx context.Context, networkId server.Id) (localPro bool, localOk bool, cachedPro bool, cachedOk bool) {
	var localEntitlement, cachedEntitlement proEntitlement
	localEntitlement, localOk = getProNetworkLocal(networkId)
	cachedEntitlement, cachedOk = getProNetworkCached(ctx, networkId)
	return localEntitlement.pro, localOk, cachedEntitlement.pro, cachedOk
}

// When set, runs after a refresh has loaded a network's entitlement and before it
// writes the cache. It exists so a test can hold a read there and land a commit and
// its refresh in between. Test only, and never set in production.
var testingProNetworkLoaded atomic.Pointer[func(networkId server.Id)]

// Loads the entitlement from the db and stores it in both tiers, returning the loaded
// value.
func refreshProNetwork(ctx context.Context, networkId server.Id) bool {
	entitlement := loadProNetwork(ctx, networkId)
	if loaded := testingProNetworkLoaded.Load(); loaded != nil {
		(*loaded)(networkId)
	}
	storeProNetwork(ctx, networkId, entitlement)
	return entitlement.pro
}

// Writes a loaded entitlement to both tiers, neither of which takes it over a newer
// one. The local tier stores what redis holds afterwards (this entitlement or a newer
// one), so this process agrees with the others; with redis unavailable it stores this
// entitlement.
func storeProNetwork(ctx context.Context, networkId server.Id, entitlement proEntitlement) {
	if stored, ok := setProNetworkCached(ctx, networkId, entitlement); ok {
		entitlement = stored
	}
	setProNetworkLocal(networkId, entitlement)
}

// Reads the entitlement from the source of truth, with its version. The query runs
// alone in its own implicit transaction, so transaction_timestamp() is when this read
// began, before the snapshot it reads. Keep it on the primary (server.Db): a
// replica's clock and replay lag would not order its reads against the primary's.
func loadProNetwork(ctx context.Context, networkId server.Id) (entitlement proEntitlement) {
	server.Db(ctx, func(conn server.PgConn) {
		now := server.NowUtc()
		result, err := conn.Query(
			ctx,
			`
				SELECT
					EXISTS (
						SELECT 1
						FROM transfer_balance
						WHERE
							network_id = $1 AND
							pro = true AND
							start_time <= $2 AND
							$2 < end_time
					),
					transaction_timestamp()
			`,
			networkId,
			now,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				var readTime time.Time
				server.Raise(result.Scan(&entitlement.pro, &readTime))
				entitlement.version = readTime.UnixMicro()
			}
		})
	})
	return
}

// The redis entry for the network, ok false on a miss, an error or a value in
// another format.
func getProNetworkCached(ctx context.Context, networkId server.Id) (entitlement proEntitlement, ok bool) {
	server.Redis(ctx, func(r server.RedisClient) {
		value, err := r.Get(ctx, proNetworkKey(networkId)).Result()
		if err != nil {
			// a miss, or a cache error -- either way fall back to the db, which is
			// the source of truth. The cache must never be able to deny Pro.
			return
		}
		entitlement, ok = parseProEntitlement(value)
	})
	return
}

// Stores ARGV[1], an entry with version ARGV[2], for ARGV[3] milliseconds, unless the
// key holds an entry with a newer version, and returns the entry the key holds
// afterwards. Versions are compared as decimal strings (length, then digits), so Lua
// number rounding never enters the comparison. One key, so one slot.
const proNetworkCacheSetScript = `
local current = redis.call('GET', KEYS[1])
if current then
    local version = string.match(current, '^[01]:(%d+)$')
    if version and (#version > #ARGV[2] or (#version == #ARGV[2] and version > ARGV[2])) then
        return current
    end
end
redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[3])
return ARGV[1]
`

// Stores the entitlement in redis unless redis holds a newer one, and returns the
// entitlement redis holds afterwards. ok is false on a cache error.
func setProNetworkCached(ctx context.Context, networkId server.Id, entitlement proEntitlement) (stored proEntitlement, ok bool) {
	server.Redis(ctx, func(r server.RedisClient) {
		value, err := r.Eval(
			ctx,
			proNetworkCacheSetScript,
			[]string{proNetworkKey(networkId)},
			formatProEntitlement(entitlement),
			strconv.FormatInt(entitlement.version, 10),
			ProCacheTtl.Milliseconds(),
		).Text()
		if err != nil {
			return
		}
		stored, ok = parseProEntitlement(value)
	})
	return
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
