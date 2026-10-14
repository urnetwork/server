// Connection and proxy leases share the existing exact-address bijection.
// Each Redis script keeps lease ownership and its index in one cluster slot;
// readers retain the forward-owner and one-reverse-egress-per-family checks.
// Writers feed only the v4 reverse index, whose fields name each address and
// its family, and an IPv6 address's /64 beside its own forward entry; the v2
// index is withdrawn but never fed.
package model

import (
	"context"
	"net/netip"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server"
)

// Proxy allocation refreshes have one durable owner, separate from transports.
const verifyProxyEgressLeaseOwner = "proxy"

// Places reverse leases beside a client's reverse index of either version.
func verifyClientEgressLeaseKey(reverseKey string, egressHashHex string) string {
	return reverseKey + "leases_" + egressHashHex
}

// The hash tag matches the complete existing untagged forward key.
func verifyForwardEgressLeaseKey(egressHashHex string) string {
	return "{" + verifyEgressKeyFromHex(egressHashHex) + "}leases"
}

// Registers or refreshes exactly one accepted transport's source lease.
func FeedVerifyConnectionEgress(ctx context.Context, clientId, connectionId server.Id, ip netip.Addr, settings *VerifySettings) {
	feedVerifyEgressLease(ctx, clientId, ip, connectionId.String(), settings)
}

// A joined transport cleanup cannot withdraw an overlapping connection's lease.
func ClearVerifyConnectionEgress(ctx context.Context, clientId, connectionId server.Id, ip netip.Addr, settings *VerifySettings) {
	clearVerifyEgressLease(ctx, clientId, ip, connectionId.String(), settings)
}

// Prunes expired owners and publishes the latest remaining reverse expiry
// under the address's reverse field (ARGV[4]), followed by the value suffix
// ARGV[5], or by the field's current suffix when ARGV[5] is empty (a
// withdrawal, which does not know the address). Updating or clearing an owner
// and its reverse entry is one atomic operation.
const verifyReverseEgressLeaseScript = `
local now = tonumber(ARGV[2])
local expires = tonumber(ARGV[3])
if expires > now then redis.call('HSET', KEYS[2], ARGV[1], ARGV[3])
else redis.call('HDEL', KEYS[2], ARGV[1]) end
local entries = redis.call('HGETALL', KEYS[2])
local latest = 0
for i = 1, #entries, 2 do
  local deadline = tonumber(entries[i+1]) or 0
  if deadline <= now then redis.call('HDEL', KEYS[2], entries[i])
  else latest = math.max(latest, deadline) end
end
if latest > now then
  local suffix = ARGV[5]
  if suffix == '' then
    local current = redis.call('HGET', KEYS[1], ARGV[4])
    if current then suffix = string.match(current, '_.*$') or '' end
  end
  redis.call('HSET', KEYS[1], ARGV[4], string.format('%.0f', latest) .. suffix)
  redis.call('PEXPIRE', KEYS[2], latest-now)
  local ttl = redis.call('PTTL', KEYS[1])
  if ttl < latest-now then redis.call('PEXPIRE', KEYS[1], latest-now) end
else
  redis.call('HDEL', KEYS[1], ARGV[4])
  redis.call('DEL', KEYS[2])
end
return latest`

// Guards forward deletion with the same connection ownership as the reverse
// index. Conflicting clients still poison the address for the original TTL;
// cleanup never selects another claimant or shortens that ambiguity window.
//
// Only a feed to an unmarked address and a clear by the attributed client can
// change the forward entry, so only they rescan the owners. Any other update
// records or withdraws just its own owner: every connect, refresh and close
// behind a marked shared address (many clients behind one NAT), and a close
// while another client or nobody is attributed. Rescanning on every update made
// one shared address quadratic work on its one cluster slot. The deferred path
// only grows the owner hash TTL, so it still covers every recorded owner;
// expired owners leave at the next rescan (the first feed after the marker
// lapses) or with the hash. Returns the caller's latest live deadline, or 0
// when not rescanned.
const verifyForwardEgressLeaseScript = `
local client = ARGV[1]
local owner = client .. '/' .. ARGV[2]
local now = tonumber(ARGV[3])
local expires = tonumber(ARGV[4])
local cur = redis.call('GET', KEYS[1])
if (expires > now and cur == ARGV[5]) or (expires <= now and cur ~= client) then
  if expires > now then
    redis.call('HSET', KEYS[2], owner, ARGV[4])
    if redis.call('PTTL', KEYS[2]) < expires-now then redis.call('PEXPIRE', KEYS[2], expires-now) end
  else redis.call('HDEL', KEYS[2], owner) end
  return 0
end
if expires > now then redis.call('HSET', KEYS[2], owner, ARGV[4])
else redis.call('HDEL', KEYS[2], owner) end
local entries = redis.call('HGETALL', KEYS[2])
local latest = 0
local clientLatest = 0
local otherLive = false
for i = 1, #entries, 2 do
  local deadline = tonumber(entries[i+1]) or 0
  if deadline <= now then redis.call('HDEL', KEYS[2], entries[i])
  else
    latest = math.max(latest, deadline)
    if string.sub(entries[i], 1, #client+1) == client .. '/' then
      clientLatest = math.max(clientLatest, deadline)
    else otherLive = true end
  end
end
if latest > now then redis.call('PEXPIRE', KEYS[2], latest-now)
else redis.call('DEL', KEYS[2]) end
if expires > now then
  if otherLive or (cur ~= false and cur ~= client) then
    redis.call('SET', KEYS[1], ARGV[5], 'PX', expires-now)
  else redis.call('SET', KEYS[1], client, 'PX', clientLatest-now) end
elseif cur == client then
  if clientLatest > now then redis.call('PEXPIRE', KEYS[1], clientLatest-now)
  else redis.call('DEL', KEYS[1]) end
end
return clientLatest`

// Completes both slot-local updates before making provider eligibility visible.
// A partial Redis failure remains fail-closed through the existing bijection.
func feedVerifyEgressLease(ctx context.Context, clientId server.Id, ip netip.Addr, owner string, settings *VerifySettings) {
	if !ip.IsValid() {
		return
	}
	field, _ := verifyClientEgressField(ip, settings)
	now := server.NowUtc()
	server.Redis(ctx, func(r server.RedisClient) {
		updateVerifyEgressLease(ctx, r, verifyClientEgressKey(clientId), clientId, field, verifyClientEgressValueSuffix(ip, settings), owner, now, now.Add(settings.EgressTtl))
	})
	updateVerifyEligibleMembership(ctx, clientId)
}

// Withdraws only one owner; other owners retain both their exact address and TTL.
func clearVerifyEgressLease(ctx context.Context, clientId server.Id, ip netip.Addr, owner string, settings *VerifySettings) {
	if !ip.IsValid() {
		return
	}
	field, _ := verifyClientEgressField(ip, settings)
	server.Redis(ctx, func(r server.RedisClient) {
		updateVerifyEgressLease(ctx, r, verifyClientEgressKey(clientId), clientId, field, "", owner, server.NowUtc(), time.Time{})
	})
	updateVerifyEligibleMembership(ctx, clientId)
}

// The explicit clock makes expiration and refresh interleavings deterministic
// in tests; all production callers use server UTC and the configured TTL.
// `reverseKey` and `field` name the address's reverse entry, and
// `valueSuffix` follows its expiry (verifyClientEgressValueSuffix), or ""
// keeps the current one; only withdrawals name the v2 index. Every forward
// entry the address feeds takes the same owner.
func updateVerifyEgressLease(ctx context.Context, r server.RedisClient, reverseKey string, clientId server.Id, field, valueSuffix, owner string, now, expires time.Time) {
	reverseKeys, reverseArgs := verifyReverseEgressLeaseCall(reverseKey, field, valueSuffix, owner, now, expires)
	server.Raise(r.Eval(ctx, verifyReverseEgressLeaseScript, reverseKeys, reverseArgs...).Err())
	for _, egressHashHex := range parseVerifyClientEgressField(field).forwardHashes() {
		forwardKeys, forwardArgs := verifyForwardEgressLeaseCall(clientId, egressHashHex, owner, now, expires)
		server.Raise(r.Eval(ctx, verifyForwardEgressLeaseScript, forwardKeys, forwardArgs...).Err())
	}
}

// The keys and arguments of one owner's reverse lease update.
func verifyReverseEgressLeaseCall(reverseKey, field, valueSuffix, owner string, now, expires time.Time) ([]string, []any) {
	return []string{reverseKey, verifyClientEgressLeaseKey(reverseKey, parseVerifyClientEgressField(field).address)},
		[]any{owner, now.UnixMilli(), expires.UnixMilli(), field, valueSuffix}
}

// The keys and arguments of one owner's update of one forward entry.
func verifyForwardEgressLeaseCall(clientId server.Id, egressHashHex, owner string, now, expires time.Time) ([]string, []any) {
	return []string{verifyEgressKeyFromHex(egressHashHex), verifyForwardEgressLeaseKey(egressHashHex)},
		[]any{clientId.String(), owner, now.UnixMilli(), expires.UnixMilli(), verifyEgressAmbiguous}
}

// Reaping is stronger than disconnect: all of the removed client's owners are
// withdrawn, but another client's forward owner and leases are preserved.
func removeVerifyEgressAddressForClient(ctx context.Context, r redis.Cmdable, clientId server.Id, egressHashHex string) *redis.Cmd {
	return r.Eval(ctx, `
local prefix = ARGV[1] .. '/'
for _, owner in ipairs(redis.call('HKEYS', KEYS[2])) do
  if string.sub(owner, 1, #prefix) == prefix then redis.call('HDEL', KEYS[2], owner) end
end
if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('DEL', KEYS[1]) end
return 0`, []string{verifyEgressKeyFromHex(egressHashHex), verifyForwardEgressLeaseKey(egressHashHex)}, clientId.String())
}

// A refresh after the reader's snapshot must survive stale expiry cleanup.
func pruneVerifyExpiredEgressHashes(ctx context.Context, r server.RedisClient, clientId server.Id, expiredHashExpiries map[string]string) {
	server.Raise(queueVerifyExpiredEgressHashPrune(ctx, r, clientId, expiredHashExpiries).Err())
}

// The prune as one command, which a pipeline queues and a client runs at once.
func queueVerifyExpiredEgressHashPrune(ctx context.Context, r redis.Cmdable, clientId server.Id, expiredHashExpiries map[string]string) *redis.Cmd {
	args := make([]any, 0, 2*len(expiredHashExpiries))
	for hash, expiry := range expiredHashExpiries {
		args = append(args, hash, expiry)
	}
	return r.Eval(ctx, `
for i = 1, #ARGV, 2 do
  if redis.call('HGET', KEYS[1], ARGV[i]) == ARGV[i+1] then
    redis.call('HDEL', KEYS[1], ARGV[i])
  end
end
return 0`, []string{verifyClientEgressKey(clientId)}, args...)
}

// Proxy release does not own simultaneous direct-transport leases. The reverse
// index supplies the keyed address hashes without loading controller settings;
// a proxy lease a v2 writer left in the v2 index is withdrawn the same way.
func clearVerifyProxyEgressForClient(ctx context.Context, clientId server.Id) {
	server.Redis(ctx, func(r server.RedisClient) {
		for _, reverseKey := range verifyClientEgressKeys(clientId) {
			entries, err := r.HGetAll(ctx, reverseKey).Result()
			server.Raise(err)
			for field := range entries {
				updateVerifyEgressLease(ctx, r, reverseKey, clientId, field, "", verifyProxyEgressLeaseOwner, server.NowUtc(), time.Time{})
			}
		}
	})
	updateVerifyEligibleMembership(ctx, clientId)
}
