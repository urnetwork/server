package model

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/server"
)

// A network's tier (D-QUOTA-1, DESIGN.md 20.5): free (Community), pro or team, the
// highest tier an in-window balance confers. Like Pro's entitlement it is time-based,
// start_time <= now < end_time, never `active`, which is bytes left, so a spent Pro
// balance still answers pro until its window ends.
//
// It rides Pro's cache: the same two tiers, the same version order and the same ttls
// (pro_model.go), under a key of its own, network_tier_versioned:<network_id>, holding
// "<tier>:<version>". The pro_versioned: value keeps its "<0|1>:<version>" format, since
// a rolling deploy has both releases reading that key and its script matches only that
// format. UpdateProNetworkInConn refreshes both keys from one load, after the writer's
// commit and never inside its transaction, so an upgrade reaches NetworkTierOf at once
// in this process and within ProLocalCacheTtl in the others.
//
// The tier reads transfer_balance.pro alone in a binary that does not link MG6, which adds
// transfer_balance.tier, the Team grant's mark (QUOTA-M6-01): there a Pro balance answers
// pro and team never appears. Where MG6 is linked one read answers both, team for an
// in-window balance with pro = true and tier 'team', and since the Team grant also writes
// pro = true, IsProNetwork's readers stay right.

func networkTierKey(networkId server.Id) string {
	return fmt.Sprintf("network_tier_versioned:%s", networkId)
}

// A network's tier, with the version of the read that produced it, as proEntitlement.
type networkTierEntitlement struct {
	tier NetworkTier
	// microseconds since the unix epoch, postgres clock (pro_model.go's ordering note)
	version int64
}

// Whether tier is one this build knows.
func isNetworkTier(tier NetworkTier) bool {
	switch tier {
	case NetworkTierFree, NetworkTierPro, NetworkTierTeam:
		return true
	default:
		return false
	}
}

// The tier a load of Pro's entitlement gives, before MG6's tier column: pro or free.
func networkTierOfProEntitlement(entitlement proEntitlement) networkTierEntitlement {
	tier := NetworkTierFree
	if entitlement.pro {
		tier = NetworkTierPro
	}
	return networkTierEntitlement{
		tier:    tier,
		version: entitlement.version,
	}
}

// The redis value of a tier, "<tier>:<version>".
func formatNetworkTierEntitlement(entitlement networkTierEntitlement) string {
	return string(entitlement.tier) + ":" + strconv.FormatInt(entitlement.version, 10)
}

// The tier a redis value holds, ok false for a value in any other format or a tier
// this build does not know.
func parseNetworkTierEntitlement(value string) (entitlement networkTierEntitlement, ok bool) {
	tier, versionString, found := strings.Cut(value, ":")
	if !found || !isNetworkTier(NetworkTier(tier)) {
		return networkTierEntitlement{}, false
	}
	version, err := strconv.ParseInt(versionString, 10, 64)
	if err != nil {
		return networkTierEntitlement{}, false
	}
	return networkTierEntitlement{
		tier:    NetworkTier(tier),
		version: version,
	}, true
}

type networkTierLocalEntry struct {
	entitlement networkTierEntitlement
	expiry      time.Time
}

// This process's tier, in front of redis, as Pro's local tier. Safe for concurrent use.
type networkTierLocalCache struct {
	stateLock sync.Mutex
	entries   map[server.Id]networkTierLocalEntry
}

func newNetworkTierLocalCache() *networkTierLocalCache {
	return &networkTierLocalCache{
		entries: map[server.Id]networkTierLocalEntry{},
	}
}

// The live entry for the network, ok false when it holds none.
func (self *networkTierLocalCache) get(networkId server.Id) (entitlement networkTierEntitlement, ok bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	entry, found := self.entries[networkId]
	if !found || !server.NowUtc().Before(entry.expiry) {
		return networkTierEntitlement{}, false
	}
	return entry.entitlement, true
}

// Stores the entitlement unless a live entry holds a newer version.
func (self *networkTierLocalCache) set(networkId server.Id, entitlement networkTierEntitlement) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	now := server.NowUtc()
	if entry, found := self.entries[networkId]; found && now.Before(entry.expiry) && entitlement.version < entry.entitlement.version {
		return
	}
	// a cache, not a registry: dropping it whole when full is fine (pro_model.go)
	if proLocalCacheMaxSize <= len(self.entries) {
		self.entries = map[server.Id]networkTierLocalEntry{}
	}
	self.entries[networkId] = networkTierLocalEntry{
		entitlement: entitlement,
		expiry:      now.Add(ProLocalCacheTtl),
	}
}

func (self *networkTierLocalCache) remove(networkId server.Id) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	delete(self.entries, networkId)
}

var networkTierLocal = newNetworkTierLocalCache()

// The network's tier: the highest in-window tier, through this process's cache, then
// redis, loading from the db only on a miss of both. Safe on a hot path, as IsProNetwork
// is. Never for a value baked into a token: that is NetworkTierOfFresh.
func NetworkTierOf(ctx context.Context, networkId server.Id) NetworkTier {
	if entitlement, ok := networkTierLocal.get(networkId); ok {
		return entitlement.tier
	}
	if entitlement, ok := getNetworkTierCached(ctx, networkId); ok {
		networkTierLocal.set(networkId, entitlement)
		return entitlement.tier
	}
	_, entitlement := loadNetworkTierInConn(nil, ctx, networkId)
	observeProNetworkLoaded(networkId)
	storeNetworkTier(ctx, networkId, entitlement)
	return entitlement.tier
}

// NetworkTierOf for a caller holding a transaction, whose miss loads through tx rather
// than a second connection (connect/CODESTYLE.md, Locking). A load through tx writes
// neither cache: tx may hold its own uncommitted balance change, which a rollback would
// leave cached. So call it only where the cache is usually warm.
func NetworkTierOfInTx(ctx context.Context, tx server.PgTx, networkId server.Id) NetworkTier {
	return networkTierOfWithQuery(ctx, tx, networkId)
}

// NetworkTierOfInTx for any connection or transaction the caller holds.
func networkTierOfWithQuery(ctx context.Context, query server.PgCanQuery, networkId server.Id) NetworkTier {
	if entitlement, ok := networkTierLocal.get(networkId); ok {
		return entitlement.tier
	}
	if entitlement, ok := getNetworkTierCached(ctx, networkId); ok {
		networkTierLocal.set(networkId, entitlement)
		return entitlement.tier
	}
	_, entitlement := loadNetworkTierWithConn(ctx, query, networkId)
	return entitlement.tier
}

// The tier from the source of truth, refreshing both cache tiers, for anything baked
// into a durable artifact such as a token, as IsProNetworkFresh says: a stale read
// frozen into a token outlives every ttl.
func NetworkTierOfFresh(ctx context.Context, networkId server.Id) NetworkTier {
	return updateNetworkEntitlementInConn(nil, ctx, networkId).tier
}

// Loads the network's entitlement once and refreshes both keys, Pro's and the tier's,
// in both cache tiers, returning what it loaded. UpdateProNetworkInConn's body: call it
// after the transaction that changed the balances commits, never inside it.
func updateNetworkEntitlementInConn(connOwner server.PgConn, ctx context.Context, networkId server.Id) networkTierEntitlement {
	entitlement, tierEntitlement := loadNetworkTierInConn(connOwner, ctx, networkId)
	observeProNetworkLoaded(networkId)
	storeProNetwork(ctx, networkId, entitlement)
	storeNetworkTier(ctx, networkId, tierEntitlement)
	return tierEntitlement
}

// Pro's entitlement and the network's tier from one read of the source of truth, with that
// read's version, on the caller's connection or a new one when connOwner is nil.
func loadNetworkTierInConn(connOwner server.PgConn, ctx context.Context, networkId server.Id) (entitlement proEntitlement, tierEntitlement networkTierEntitlement) {
	server.DbInConn(ctx, connOwner, func(conn server.PgConn) {
		entitlement, tierEntitlement = loadNetworkTierWithConn(ctx, conn, networkId)
	})
	return
}

// loadNetworkTierInConn on any connection or transaction the caller holds. Without MG6 it is
// Pro's own read, and the tier is pro or free; with MG6 the same read also asks for an in-window
// team grant, which implies pro.
func loadNetworkTierWithConn(ctx context.Context, query server.PgCanQuery, networkId server.Id) (entitlement proEntitlement, tierEntitlement networkTierEntitlement) {
	if !server.SidecarMg6Linked() {
		entitlement = loadProNetworkWithConn(ctx, query, networkId)
		return entitlement, networkTierOfProEntitlement(entitlement)
	}
	now := server.NowUtc()
	team := false
	result, err := query.Query(
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
				EXISTS (
					SELECT 1
					FROM transfer_balance
					WHERE
						network_id = $1 AND
						pro = true AND
						tier = 'team' AND
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
			server.Raise(result.Scan(&entitlement.pro, &team, &readTime))
			entitlement.version = readTime.UnixMicro()
		}
	})
	tierEntitlement = networkTierOfProEntitlement(entitlement)
	if team {
		tierEntitlement.tier = NetworkTierTeam
	}
	return
}

// Writes a loaded tier to both tiers of the cache, neither of which takes it over a
// newer one, as storeProNetwork does.
func storeNetworkTier(ctx context.Context, networkId server.Id, entitlement networkTierEntitlement) {
	if stored, ok := setNetworkTierCached(ctx, networkId, entitlement); ok {
		entitlement = stored
	}
	networkTierLocal.set(networkId, entitlement)
}

// The redis entry for the network, ok false on a miss, an error or a value in another
// format. The cache never decides a tier on its own: a miss loads from the db.
func getNetworkTierCached(ctx context.Context, networkId server.Id) (entitlement networkTierEntitlement, ok bool) {
	proxyAdmissionRedis(ctx, func(r server.RedisClient) {
		value, err := r.Get(ctx, networkTierKey(networkId)).Result()
		if err != nil {
			return
		}
		entitlement, ok = parseNetworkTierEntitlement(value)
	})
	return
}

// proNetworkCacheSetScript's shape for the tier's key: stores ARGV[1], an entry with
// version ARGV[2], for ARGV[3] milliseconds, unless the key holds an entry with a newer
// version, and returns the entry the key holds afterwards. Versions compare as decimal
// strings, so Lua number rounding never enters. One key, so one slot.
const networkTierCacheSetScript = `
local current = redis.call('GET', KEYS[1])
if current then
    local version = string.match(current, '^[a-z]+:(%d+)$')
    if version and (#version > #ARGV[2] or (#version == #ARGV[2] and version > ARGV[2])) then
        return current
    end
end
redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[3])
return ARGV[1]
`

// Stores the tier in redis unless redis holds a newer one, and returns the tier redis
// holds afterwards. ok is false on a cache error.
func setNetworkTierCached(ctx context.Context, networkId server.Id, entitlement networkTierEntitlement) (stored networkTierEntitlement, ok bool) {
	proxyAdmissionRedis(ctx, func(r server.RedisClient) {
		value, err := r.Eval(
			ctx,
			networkTierCacheSetScript,
			[]string{networkTierKey(networkId)},
			formatNetworkTierEntitlement(entitlement),
			strconv.FormatInt(entitlement.version, 10),
			ProCacheTtl.Milliseconds(),
		).Text()
		if err != nil {
			return
		}
		stored, ok = parseNetworkTierEntitlement(value)
	})
	return
}

// Reads both cache tiers for a network's tier without loading it or filling either
// tier, for tests that pin when a writer refreshes the cache. An ok is false when that
// tier holds no live entry.
func Testing_NetworkTierCacheEntries(ctx context.Context, networkId server.Id) (localTier NetworkTier, localOk bool, cachedTier NetworkTier, cachedOk bool) {
	var localEntitlement, cachedEntitlement networkTierEntitlement
	localEntitlement, localOk = networkTierLocal.get(networkId)
	cachedEntitlement, cachedOk = getNetworkTierCached(ctx, networkId)
	return localEntitlement.tier, localOk, cachedEntitlement.tier, cachedOk
}

// Drops the network's tier from both cache tiers, as both ttls running out would, for
// tests of a balance that ends with no writer.
func Testing_ExpireNetworkTierCache(ctx context.Context, networkId server.Id) {
	networkTierLocal.remove(networkId)
	server.Redis(ctx, func(r server.RedisClient) {
		r.Del(ctx, networkTierKey(networkId))
	})
}
