package model

// §7.9's verified read cache {acl_<networkId>}e (owner decision 136) and §8.9's
// {acl_<networkId>}v, written after each entry api appends by one script, the
// only writer of either: evs and gkm read them (sidecar.AclGateDirectSource)
// and never write. All three of a network's {acl_} keys share its hash tag, so
// the script and the limiter's {acl_}q sit on one slot.
//
//	{acl_}v   INCRed once per entry, its keyspace event the gates' reload
//	          trigger; lives 48 h after its last change (§8.9)
//	{acl_}e   a hash: b, the newest base's position, which only rises; m, the
//	          tip, which only rises; v, the {acl_}v value the hash matches; and
//	          each entry's AuthEntry bytes by its decimal position, from b on,
//	          at most the newest EntryMaxCount; lives 10 min after the
//	          network's last append
//
// The cache decides nothing. An entry below the hash's base is not stored, a
// base drops the entries below it, a gap (another instance's entry whose
// script has not run) stays a hole until that script fills it, and a reader
// that finds a hole, a v that is not {acl_}v's or a position below b reads
// proj/ instead, so a stale or lost hash costs cluster reads only, never a
// lower n (FS-62).

import (
	"context"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol/bridge"

	"github.com/urnetwork/server"
)

// Tunables of the read cache.
type ConfigLogCacheSettings struct {
	// The most entries {acl_}e holds: 1,024, §7.9's fold span, so a reader at
	// the newest base's tail takes it from Redis.
	EntryMaxCount int
	// {acl_}e lives this long after the network's last append: 10 min (§2.8
	// api-7).
	EntryTtl time.Duration
	// {acl_}v lives this long after its last change: 48 h (§8.9).
	VersionTtl time.Duration
	// How long one script waits for Redis.
	RedisTimeout time.Duration
}

func DefaultConfigLogCacheSettings() *ConfigLogCacheSettings {
	return &ConfigLogCacheSettings{
		EntryMaxCount: 1024,
		EntryTtl:      10 * time.Minute,
		VersionTtl:    48 * time.Hour,
		RedisTimeout:  time.Second,
	}
}

// KEYS[1] is {acl_}v and KEYS[2] {acl_}e. ARGV[1] is the entry's position n,
// ARGV[2] the newest base's position at n, ARGV[3] the entry's bytes, ARGV[4]
// the most entries to hold, ARGV[5] {acl_}e's ttl and ARGV[6] {acl_}v's, in
// seconds. Positions are log positions, far below what a Lua number holds
// exactly; {acl_}v's value is written back as INCR answered it. Returns the new
// {acl_}v.
const configLogCacheScriptSource = `
local v = redis.call('INCR', KEYS[1])
redis.call('EXPIRE', KEYS[1], tonumber(ARGV[6]))
local n = tonumber(ARGV[1])
local base = tonumber(ARGV[2])
local maxCount = tonumber(ARGV[4])
local b = tonumber(redis.call('HGET', KEYS[2], 'b'))
local m = tonumber(redis.call('HGET', KEYS[2], 'm'))
if b == nil or m == nil then
    redis.call('DEL', KEYS[2])
    b = base
    m = n
end
if b < base then
    local last = math.min(base - 1, m)
    local first = math.max(b, last - maxCount)
    for k = first, last do
        redis.call('HDEL', KEYS[2], tostring(k))
    end
    b = base
end
if b <= n then
    redis.call('HSET', KEYS[2], tostring(n), ARGV[3])
    if m < n then
        local floor = math.max(b, n - maxCount + 1)
        for k = math.max(b, m - maxCount + 1), floor - 1 do
            redis.call('HDEL', KEYS[2], tostring(k))
        end
        m = n
    end
end
redis.call('HSET', KEYS[2], 'b', tostring(b), 'm', tostring(m), 'v', tostring(v))
redis.call('EXPIRE', KEYS[2], tonumber(ARGV[5]))
return v
`

var configLogCacheScript = redis.NewScript(configLogCacheScriptSource)

// The read cache's writer on server's Redis: the sequencer's EntryCache.
func NewConfigLogRedisEntryCache(settings *ConfigLogCacheSettings) func(ctx context.Context, networkId server.Id, n uint64, base uint64, entryBytes []byte) error {
	if settings == nil {
		settings = DefaultConfigLogCacheSettings()
	}
	return func(ctx context.Context, networkId server.Id, n uint64, base uint64, entryBytes []byte) error {
		boundedCtx, cancel := context.WithTimeout(ctx, settings.RedisTimeout)
		defer cancel()
		return server.RedisWithDeadline(boundedCtx, func(r server.RedisClient) error {
			return configLogCacheScript.Run(
				boundedCtx,
				r,
				[]string{sidecar.AclVersionKey(sidecar.Id(networkId)), sidecar.AclEntryCacheKey(sidecar.Id(networkId))},
				strconv.FormatUint(n, 10),
				strconv.FormatUint(base, 10),
				entryBytes,
				settings.EntryMaxCount,
				int64(settings.EntryTtl/time.Second),
				int64(settings.VersionTtl/time.Second),
			).Err()
		})
	}
}

// The team topic's message types (§6.11's registry, §7.17).
const (
	ConfigLogHintMembershipChanged    = 0x1201
	ConfigLogHintApprovalRequested    = 0x1202
	ConfigLogHintAuthorityHeadChanged = 0x1203
)

// The statement types that change who belongs: membership hints carry them.
var configLogMembershipTypes = map[bridge.SignedType]bool{
	bridge.SignedType_SIGNED_MEMBER_ADMIT:   true,
	bridge.SignedType_SIGNED_MEMBER_REVOKE:  true,
	bridge.SignedType_SIGNED_SIDECAR_ADMIT:  true,
	bridge.SignedType_SIGNED_SIDECAR_REVOKE: true,
}

// The hint kind an entry earns: MembershipChanged for a statement of who
// belongs or a tick's reaps, else AuthorityHeadChanged. ApprovalRequested's
// trigger is §16's, which gives its own.
func configLogHintKind(entryBytes []byte) uint32 {
	entry := &bridge.AuthEntry{}
	if proto.Unmarshal(entryBytes, entry) != nil {
		return ConfigLogHintAuthorityHeadChanged
	}
	switch entry.GetKind() {
	case bridge.AuthEntryKindTick:
		return ConfigLogHintMembershipChanged
	case bridge.AuthEntryKindStatement:
		signed := &bridge.Signed{}
		if proto.Unmarshal(entry.GetBody(), signed) == nil && configLogMembershipTypes[bridge.SignedType(signed.GetType())] {
			return ConfigLogHintMembershipChanged
		}
	}
	return ConfigLogHintAuthorityHeadChanged
}

// Tunables of the team hints.
type ConfigLogHintSettings struct {
	// The most clients one append hints, so an append's work stays bounded; a
	// client past it learns at its next poll or hourly resync.
	ClientMaxCount int
}

func DefaultConfigLogHintSettings() *ConfigLogHintSettings {
	return &ConfigLogHintSettings{
		ClientMaxCount: 10000,
	}
}

// The clients a network's team hints go to: its active sidecar clients, which
// read the log, and its acl_enforcing device clients (§6.8, §18.6), at most
// limit of them. Nothing in a binary that does not link MG0.
func configLogHintClientIds(ctx context.Context, networkId server.Id, limit int) (clientIds []server.Id) {
	if !server.SidecarMg0Linked() {
		return nil
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT network_client.client_id
				FROM network_client
				INNER JOIN network_client_owner ON network_client_owner.client_id = network_client.client_id
				WHERE
					network_client.network_id = $1 AND
					network_client.active = true AND
					(network_client_owner.strict_exp OR network_client_owner.acl_enforcing)
				LIMIT $2
			`,
			networkId,
			limit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				server.Raise(result.Scan(&clientId))
				clientIds = append(clientIds, clientId)
			}
		})
	})
	return
}

// The sequencer's Hint: one team hint per entry to the network's clients, its
// version the entry's position n, which never falls (§7.17). Best effort: a
// failure is logged, and a client missing one reads the log at its next poll
// or resync.
func NewConfigLogTeamHint(settings *ConfigLogHintSettings) func(ctx context.Context, networkId server.Id, pin *bridge.AuthPin, entryBytes []byte) {
	if settings == nil {
		settings = DefaultConfigLogHintSettings()
	}
	return func(ctx context.Context, networkId server.Id, pin *bridge.AuthPin, entryBytes []byte) {
		if r := server.HandleError(func() {
			clientIds := configLogHintClientIds(ctx, networkId, settings.ClientMaxCount)
			if len(clientIds) == 0 {
				return
			}
			err := SidecarHint(ctx, clientIds, &sidecar.ClientHint{
				Topic:   sidecar.PushTopicTeam,
				Version: pin.GetVersion(),
				Kind:    configLogHintKind(entryBytes),
			})
			if err != nil {
				glog.Warningf("[config]%s team hint at %d: %v\n", networkId, pin.GetVersion(), err)
			}
		}); r != nil {
			glog.Warningf("[config]%s team hint at %d: %v\n", networkId, pin.GetVersion(), r)
		}
	}
}

// api's sequencer settings: the defaults, with the read cache's script, the
// append limits and the team hints on server's Redis. What api's process builds
// its sequencer with when it installs one (SetConfigLogSequencer).
func NewApiConfigLogSettings() *ConfigLogSettings {
	settings := DefaultConfigLogSettings()
	settings.EntryCache = NewConfigLogRedisEntryCache(nil)
	settings.Limiter = NewConfigLogRedisLimiter(nil)
	settings.Hint = NewConfigLogTeamHint(nil)
	return settings
}
