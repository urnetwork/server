package model

// network_verified (§21.7, owner decisions 41 and 96; Q-ABUSE-2; ABUSE-M5-07): what public names (§12.8, §13.9)
// and internet mail (§14.7) always need. A network is verified while either holds:
//
//   - a payment method: an in-window transfer_balance row with paid true (start_time <= now < end_time;
//     server/db_migrations.go:1653-1655), never `active`, which is bytes left;
//   - its owner user's verified phone or email, Apple and Google sign-ins counting: a network_user_auth_password
//     row of the network's admin_user_id, email or phone, with verified true, or a network_user_auth_sso row of
//     that user for Apple or Google (plan-ABUSE-small-readings #19). The owner is network.admin_user_id until
//     TEAM-M6-02 names an Owner.
//
// Wallet, seed phrase and guest sign-ins never count, so a network whose only sign-ins are anonymous stays
// unverified while still leaving probation by age (owner decision 96).
//
// It is cached as Pro is (pro_model.go): an in-process tier in front of api-13's Redis key
// {sq_<network_id>}verified, which §2.8's api-1 and api-13 rows list (plan-ABUSE-metadata-rows #1), holding
// "<0|1>:<version>", the version being the Postgres time at which the read that produced it began, so a read
// that loaded before a verification committed never writes over the refresh after it, in either tier. The two
// windows are Pro's, ProLocalCacheTtl in process and ProCacheTtl in Redis, as the rows say. A paid balance's
// window ends with no writer, so the ttls, not only the writers' refreshes, bound how long a stale true serves.
//
// UpdateNetworkVerified refreshes both tiers from Postgres after a writer commits. It runs wherever
// UpdateProNetwork runs (updateNetworkEntitlementInConn), which every payment path calls after its commit and
// IsProFresh calls at each sign-in, a verification's included, so a balance landing or a login verifying
// reaches the cache at once in this process and within LocalCacheTtl in the others.
//
// Nothing here is read on an inline packet or frame path: a gate reads network_verified from NewAbuseGate's
// memory (abuse_gate.go), which loads it off the path (I-12).

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/server"
)

// The network's verification as one boolean, beside the network row it reads from, for a query whose FROM names
// `network` and whose $2 is the time it asks at. A missing admin user or balance reads false.
const networkVerifiedSql = `
	(
		EXISTS (
			SELECT 1
			FROM transfer_balance
			WHERE
				transfer_balance.network_id = network.network_id AND
				transfer_balance.paid AND
				transfer_balance.start_time <= $2 AND
				$2 < transfer_balance.end_time
		) OR
		EXISTS (
			SELECT 1
			FROM network_user_auth_password
			WHERE
				network_user_auth_password.user_id = network.admin_user_id AND
				network_user_auth_password.auth_type IN ('email', 'phone') AND
				network_user_auth_password.verified
		) OR
		EXISTS (
			SELECT 1
			FROM network_user_auth_sso
			WHERE
				network_user_auth_sso.user_id = network.admin_user_id AND
				network_user_auth_sso.auth_type IN ('apple', 'google')
		)
	)
`

// Tunables of network_verified's cache.
type NetworkVerifiedSettings struct {
	// The shared Redis window: ProCacheTtl, 60 s, as §2.8's api-13 row publishes. It bounds the case with no writer,
	// a paid balance simply ending. Non-positive stores nothing in Redis.
	CacheTtl time.Duration
	// The in-process window: ProLocalCacheTtl, 5 s, the worst-case delay between a verification and another
	// process seeing it. Non-positive keeps no in-process entry.
	LocalCacheTtl time.Duration
	// The most networks the in-process tier holds before it starts again empty: a cache, not a registry, as Pro's.
	LocalCacheMaxCount int
}

// Pro's two windows, which §2.8's api-1 row names for this cache.
func DefaultNetworkVerifiedSettings() *NetworkVerifiedSettings {
	return &NetworkVerifiedSettings{
		CacheTtl:           ProCacheTtl,
		LocalCacheTtl:      ProLocalCacheTtl,
		LocalCacheMaxCount: 8192,
	}
}

// A network's verification, with the version of the read that produced it.
type networkVerifiedEntitlement struct {
	verified bool
	// microseconds since the unix epoch, Postgres clock (pro_model.go's ordering note)
	version int64
}

type networkVerifiedLocalEntry struct {
	entitlement networkVerifiedEntitlement
	expiry      time.Time
}

// network_verified's two tiers. Safe for concurrent use.
type networkVerifiedCache struct {
	settings *NetworkVerifiedSettings
	now      func() time.Time

	stateLock sync.Mutex
	entries   map[server.Id]networkVerifiedLocalEntry
}

func newNetworkVerifiedCache(settings *NetworkVerifiedSettings, now func() time.Time) *networkVerifiedCache {
	if settings == nil {
		settings = DefaultNetworkVerifiedSettings()
	}
	if now == nil {
		now = server.NowUtc
	}
	return &networkVerifiedCache{
		settings: settings,
		now:      now,
		entries:  map[server.Id]networkVerifiedLocalEntry{},
	}
}

// This process's cache.
var networkVerified = newNetworkVerifiedCache(nil, nil)

// api-13's key: the network's {sq_} slot, with its quota family (quota_rate_script.go).
func networkVerifiedKey(networkId server.Id) string {
	return fmt.Sprintf("{sq_%s}verified", networkId)
}

// The Redis value, "<0|1>:<version>".
func formatNetworkVerifiedEntitlement(entitlement networkVerifiedEntitlement) string {
	verified := "0"
	if entitlement.verified {
		verified = "1"
	}
	return verified + ":" + strconv.FormatInt(entitlement.version, 10)
}

// The entitlement of a Redis value, ok false for any other format.
func parseNetworkVerifiedEntitlement(value string) (entitlement networkVerifiedEntitlement, ok bool) {
	verified, versionString, found := strings.Cut(value, ":")
	if !found || (verified != "0" && verified != "1") {
		return networkVerifiedEntitlement{}, false
	}
	version, err := strconv.ParseInt(versionString, 10, 64)
	if err != nil {
		return networkVerifiedEntitlement{}, false
	}
	return networkVerifiedEntitlement{
		verified: verified == "1",
		version:  version,
	}, true
}

// The live in-process entry, ok false when there is none.
func (self *networkVerifiedCache) getLocal(networkId server.Id) (entitlement networkVerifiedEntitlement, ok bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	entry, found := self.entries[networkId]
	if !found || !self.now().Before(entry.expiry) {
		return networkVerifiedEntitlement{}, false
	}
	return entry.entitlement, true
}

// Stores the entitlement unless a live entry holds a newer version.
func (self *networkVerifiedCache) setLocal(networkId server.Id, entitlement networkVerifiedEntitlement) {
	if self.settings.LocalCacheTtl <= 0 {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	now := self.now()
	if entry, found := self.entries[networkId]; found && now.Before(entry.expiry) && entitlement.version < entry.entitlement.version {
		return
	}
	if self.settings.LocalCacheMaxCount <= len(self.entries) {
		// a cache, not a registry: starting again empty is cheap and bounded (pro_model.go)
		self.entries = map[server.Id]networkVerifiedLocalEntry{}
	}
	self.entries[networkId] = networkVerifiedLocalEntry{
		entitlement: entitlement,
		expiry:      now.Add(self.settings.LocalCacheTtl),
	}
}

// The Redis entry, ok false on a miss, an error or a value in another format: the cache never decides a
// verification on its own, and a miss loads from Postgres.
func (self *networkVerifiedCache) getCached(ctx context.Context, networkId server.Id) (entitlement networkVerifiedEntitlement, ok bool) {
	if self.settings.CacheTtl <= 0 {
		return networkVerifiedEntitlement{}, false
	}
	server.Redis(ctx, func(r server.RedisClient) {
		value, err := r.Get(ctx, networkVerifiedKey(networkId)).Result()
		if err != nil {
			return
		}
		entitlement, ok = parseNetworkVerifiedEntitlement(value)
	})
	return
}

// proNetworkCacheSetScript's shape for this key: stores ARGV[1], an entry with version ARGV[2], for ARGV[3]
// milliseconds, unless the key holds an entry with a newer version, and returns the entry the key holds
// afterwards. Versions compare as decimal strings, so Lua number rounding never enters. One key, so one slot.
const networkVerifiedCacheSetScript = `
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

// Stores the entitlement in Redis unless Redis holds a newer one, and returns what Redis holds afterwards. ok is
// false on a cache error, or when nothing is stored in Redis.
func (self *networkVerifiedCache) setCached(ctx context.Context, networkId server.Id, entitlement networkVerifiedEntitlement) (stored networkVerifiedEntitlement, ok bool) {
	if self.settings.CacheTtl <= 0 {
		return networkVerifiedEntitlement{}, false
	}
	server.Redis(ctx, func(r server.RedisClient) {
		value, err := r.Eval(
			ctx,
			networkVerifiedCacheSetScript,
			[]string{networkVerifiedKey(networkId)},
			formatNetworkVerifiedEntitlement(entitlement),
			strconv.FormatInt(entitlement.version, 10),
			self.settings.CacheTtl.Milliseconds(),
		).Text()
		if err != nil {
			return
		}
		stored, ok = parseNetworkVerifiedEntitlement(value)
	})
	return
}

// Writes a loaded entitlement to both tiers, neither taking it over a newer one. The in-process tier keeps what
// Redis holds afterwards, so this process agrees with the others; with Redis unavailable it keeps this one.
func (self *networkVerifiedCache) store(ctx context.Context, networkId server.Id, entitlement networkVerifiedEntitlement) {
	if stored, ok := self.setCached(ctx, networkId, entitlement); ok {
		entitlement = stored
	}
	self.setLocal(networkId, entitlement)
}

// Through the in-process tier, then Redis, loading from Postgres only on a miss of both.
func (self *networkVerifiedCache) isVerified(ctx context.Context, networkId server.Id) bool {
	if entitlement, ok := self.getLocal(networkId); ok {
		return entitlement.verified
	}
	if entitlement, ok := self.getCached(ctx, networkId); ok {
		self.setLocal(networkId, entitlement)
		return entitlement.verified
	}
	return self.update(nil, ctx, networkId)
}

// Loads from Postgres, on connOwner or a new connection when it is nil, and refreshes both tiers.
func (self *networkVerifiedCache) update(connOwner server.PgConn, ctx context.Context, networkId server.Id) bool {
	var entitlement networkVerifiedEntitlement
	server.DbInConn(ctx, connOwner, func(conn server.PgConn) {
		entitlement = loadNetworkVerifiedWithConn(ctx, conn, networkId, self.now())
	})
	self.store(ctx, networkId, entitlement)
	return entitlement.verified
}

// The network's verification at now from the source of truth, with the version of the read. The query runs alone in
// its own implicit transaction, so transaction_timestamp() is when this read began, before the snapshot it reads.
// Keep it on the primary, as Pro's is. A network that does not exist reads false.
func loadNetworkVerifiedWithConn(ctx context.Context, query server.PgCanQuery, networkId server.Id, now time.Time) (entitlement networkVerifiedEntitlement) {
	result, err := query.Query(
		ctx,
		`
			SELECT
				COALESCE((
					SELECT `+networkVerifiedSql+`
					FROM network
					WHERE network.network_id = $1
				), false),
				transaction_timestamp()
		`,
		networkId,
		now.UTC(),
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			var readTime time.Time
			server.Raise(result.Scan(&entitlement.verified, &readTime))
			entitlement.version = readTime.UnixMicro()
		}
	})
	return
}

// Whether the network is verified (§21.7): through this process's tier, then Redis, loading from Postgres only on a
// miss of both. Not for an inline path, which reads NewAbuseGate's memory instead (I-12).
func IsNetworkVerified(ctx context.Context, networkId server.Id) bool {
	return networkVerified.isVerified(ctx, networkId)
}

// Recomputes the network's verification from Postgres and refreshes both tiers, returning it. Call it after the
// transaction that changed a balance or a sign-in commits, never inside it, as UpdateProNetwork's doc says: inside, it
// would cache what the transaction cannot see yet, and a rollback would leave the cache written. It can replace only
// this process's tier; the others' live out LocalCacheTtl.
func UpdateNetworkVerified(ctx context.Context, networkId server.Id) bool {
	return networkVerified.update(nil, ctx, networkId)
}

// UpdateNetworkVerified on the caller's Postgres session; nil selects the outer acquisition boundary.
func UpdateNetworkVerifiedInConn(connOwner server.PgConn, ctx context.Context, networkId server.Id) bool {
	return networkVerified.update(connOwner, ctx, networkId)
}

// Both tiers of the network's entry, without loading or filling either, for tests that pin when a writer refreshes
// the cache. An ok is false when that tier holds no live entry.
func Testing_NetworkVerifiedCacheEntries(ctx context.Context, networkId server.Id) (localVerified bool, localOk bool, cachedVerified bool, cachedOk bool) {
	var localEntitlement, cachedEntitlement networkVerifiedEntitlement
	localEntitlement, localOk = networkVerified.getLocal(networkId)
	cachedEntitlement, cachedOk = networkVerified.getCached(ctx, networkId)
	return localEntitlement.verified, localOk, cachedEntitlement.verified, cachedOk
}
