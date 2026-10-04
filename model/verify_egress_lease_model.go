// Connection and proxy leases share the existing exact-address bijection.
// Each Redis script keeps lease ownership and its index in one cluster slot;
// readers retain the forward-owner and exactly-one-reverse-address checks.
package model

import (
	"context"
	"encoding/hex"
	"net/netip"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server"
)

// Proxy allocation refreshes have one durable owner, separate from transports.
const verifyProxyEgressLeaseOwner = "proxy"

// Places reverse leases beside the client's existing reverse index.
func verifyClientEgressLeaseKey(clientId server.Id, egressHashHex string) string {
	return verifyClientEgressKey(clientId) + "leases_" + egressHashHex
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

// Prunes expired owners and publishes the latest remaining reverse expiry.
// Updating or clearing an owner and its reverse entry is one atomic operation.
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
  redis.call('HSET', KEYS[1], ARGV[4], string.format('%.0f', latest))
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
const verifyForwardEgressLeaseScript = `
local client = ARGV[1]
local owner = client .. '/' .. ARGV[2]
local now = tonumber(ARGV[3])
local expires = tonumber(ARGV[4])
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
local cur = redis.call('GET', KEYS[1])
if expires > now then
  if cur ~= ARGV[5] then
    if otherLive or (cur ~= false and cur ~= client) then
      redis.call('SET', KEYS[1], ARGV[5], 'PX', expires-now)
    else redis.call('SET', KEYS[1], client, 'PX', clientLatest-now) end
  end
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
	egressHash := VerifyEgressIndexHashWithSettings(ip.Unmap(), settings)
	egressHashHex := hex.EncodeToString(egressHash[:])
	now := server.NowUtc()
	server.Redis(ctx, func(r server.RedisClient) {
		updateVerifyEgressLease(ctx, r, clientId, egressHashHex, owner, now, now.Add(settings.EgressTtl))
	})
	updateVerifyEligibleMembership(ctx, clientId)
}

// Withdraws only one owner; other owners retain both their exact address and TTL.
func clearVerifyEgressLease(ctx context.Context, clientId server.Id, ip netip.Addr, owner string, settings *VerifySettings) {
	if !ip.IsValid() {
		return
	}
	egressHash := VerifyEgressIndexHashWithSettings(ip.Unmap(), settings)
	egressHashHex := hex.EncodeToString(egressHash[:])
	server.Redis(ctx, func(r server.RedisClient) {
		updateVerifyEgressLease(ctx, r, clientId, egressHashHex, owner, server.NowUtc(), time.Time{})
	})
	updateVerifyEligibleMembership(ctx, clientId)
}

// The explicit clock makes expiration and refresh interleavings deterministic
// in tests; all production callers use server UTC and the configured TTL.
func updateVerifyEgressLease(ctx context.Context, r server.RedisClient, clientId server.Id, egressHashHex, owner string, now, expires time.Time) {
	server.Raise(r.Eval(ctx, verifyReverseEgressLeaseScript,
		[]string{verifyClientEgressKey(clientId), verifyClientEgressLeaseKey(clientId, egressHashHex)},
		owner, now.UnixMilli(), expires.UnixMilli(), egressHashHex).Err())
	server.Raise(r.Eval(ctx, verifyForwardEgressLeaseScript,
		[]string{verifyEgressKeyFromHex(egressHashHex), verifyForwardEgressLeaseKey(egressHashHex)},
		clientId.String(), owner, now.UnixMilli(), expires.UnixMilli(), verifyEgressAmbiguous).Err())
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
	args := make([]any, 0, 2*len(expiredHashExpiries))
	for hash, expiry := range expiredHashExpiries {
		args = append(args, hash, expiry)
	}
	server.Raise(r.Eval(ctx, `
for i = 1, #ARGV, 2 do
  if redis.call('HGET', KEYS[1], ARGV[i]) == ARGV[i+1] then
    redis.call('HDEL', KEYS[1], ARGV[i])
  end
end
return 0`, []string{verifyClientEgressKey(clientId)}, args...).Err())
}

// Proxy release does not own simultaneous direct-transport leases. The reverse
// index supplies the keyed address hashes without loading controller settings.
func clearVerifyProxyEgressForClient(ctx context.Context, clientId server.Id) {
	server.Redis(ctx, func(r server.RedisClient) {
		entries, err := r.HGetAll(ctx, verifyClientEgressKey(clientId)).Result()
		server.Raise(err)
		for hash := range entries {
			updateVerifyEgressLease(ctx, r, clientId, hash, verifyProxyEgressLeaseOwner, server.NowUtc(), time.Time{})
		}
	})
	updateVerifyEligibleMembership(ctx, clientId)
}
