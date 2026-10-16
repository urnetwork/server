// The provider liveness lease: whether a provider's connect announcement is
// alive now, which FindProviders2 needs when it answers. The pool it draws from
// is exported every 30 to 65 minutes, a connection row can stay connected after
// its transport is gone, and the request's own live read covers only the
// Quality natives it reads, so about 5-10% of the providers it lent were
// offline when it answered.
//
// Each connect process writes the leases of all its connections, shard by
// shard, once every ProviderLivenessLeaseRefreshInterval and at once for a
// connection new to it (connect/transport_announce_liveness_lease.go). A lease
// is the client's member in one of providerLivenessLeaseShardCount sorted sets,
// `{provider_live:NN}`, scored by its last refresh in unix milliseconds of the
// Redis node's clock (TIME in the shard script), so the clocks of the connect
// hosts never enter it. The braces are the whole key, so each shard is one
// cluster slot and a read pipelines its commands shard by shard. A shard write
// prunes its shard of members older than an hour now and then.
//
// Every connect process also stamps its handler in the writer set,
// `{provider_live_writers:2}`, every few seconds (PublishProviderLivenessWriter),
// while each of its connections has been written within
// ProviderLivenessWriterWithholdAge. The reader (provider_liveness_lease_read.go)
// enforces leases only while every live connect handler is stamped, so a
// connect process without this writer, mid-rollout, rolled back or built
// without it, or one whose lease writes stopped, fails every read open rather
// than leaving its providers without a lease. The set's generation suffix
// leaves out the stamps of earlier builds, whose stamps vouched for less.
//
// The lease is the client's, not one connection's. A connection that ends
// leaves it to lapse within the window: another connection of the client may
// still hold it, and a client that reconnects, after every drain or network
// change, needs it until the new connection's first write. Only a client that
// stops providing has its lease released at once, as a read sees that;
// provide modes are the client's, so none of its connections still provides.
//
// The shard function, the keys and the scores are shared by connect writers and
// API readers of different builds, so they never change.
package model

import (
	"context"
	"fmt"
	"hash/fnv"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server"
)

// The sorted sets the leases are spread over. Each holds a few thousand
// members at Main's scale.
const providerLivenessLeaseShardCount = 32

// How long a refresh keeps a provider live: five refresh intervals of 30 s.
const providerLivenessLeaseWindow = 150 * time.Second

// A shard write prunes members older than this from its shard.
const providerLivenessLeasePruneAge = time.Hour

// A process prunes each shard at most once in this interval.
const providerLivenessLeasePruneInterval = time.Minute

// How often a connect process stamps its handler in the writer set.
const ProviderLivenessWriterInterval = 5 * time.Second

// The constants below are the cross-build contract between connect writers and
// API readers; TestProviderLivenessBudgetStaysUnderTheLeaseWindow holds them
// to it.

// How long a refresh keeps a provider live.
const ProviderLivenessLeaseWindow = providerLivenessLeaseWindow

// How long a handler's stamp counts as current.
const ProviderLivenessWriterWindow = providerLivenessWriterWindow

// How often a connect process writes the leases of all its connections,
// whatever their sync rounds do, from the start of one pass to the next.
const ProviderLivenessLeaseRefreshInterval = 25 * time.Second

// The most a connect process adds to each refresh interval at random, so the
// processes of a fleet restarted together do not write in step.
const ProviderLivenessLeaseRefreshJitter = 3 * time.Second

// The bound of one pass of a connect process's writer over all its shards.
// Shards a pass leaves unwritten when it ends are written by the next.
const ProviderLivenessLeasePassTimeout = 10 * time.Second

// The most clients one shard script writes; a larger shard write is split.
const providerLivenessLeaseScriptMaxClients = 1000

// The bound of one lease write or stamp, which is never retried: the next
// interval writes again.
const ProviderLivenessLeaseWriteTimeout = 2 * time.Second

// A connect process withholds its handler's stamp while any of its
// connections that provide, or may, has gone this long without a successful
// write of its lease, so a process whose lease writes stop, for one shard or
// all, stops vouching for its leases before any of them lapses.
const ProviderLivenessWriterWithholdAge = 75 * time.Second

// How long an API process reuses its verdict on writer coverage.
const ProviderLivenessCoverageCacheAge = 5 * time.Second

// The budget of one API read of the lease: the writer set or the shards.
const ProviderLivenessReadTimeout = 150 * time.Millisecond

// The bound of an API process's read of the live connect handlers.
const ProviderLivenessHandlersReadTimeout = time.Second

// The bound of an API process's judging of writer coverage: the live handlers,
// then the writer set.
const ProviderLivenessCoverageJudgeTimeout = ProviderLivenessHandlersReadTimeout + ProviderLivenessReadTimeout

// A handler's stamp counts as current within this window. With the withhold
// age, the writes' bounds and the readers' coverage cache it stays under the
// lease window, so a process whose lease writes stop loses its stamp, and with
// it enforcement, before any lease it refreshed lapses.
const providerLivenessWriterWindow = 60 * time.Second

// The key of the writer set, generation 2: the stamp vouches for every lease
// write of the process. Its hash tag is the whole key: one cluster slot.
const providerLivenessWritersKey = "{provider_live_writers:2}"

// The shard of a client's lease: FNV-1a of the id's 16 bytes.
func providerLivenessLeaseShard(clientId server.Id) int {
	hash := fnv.New32a()
	hash.Write(clientId[:])
	return int(hash.Sum32() % providerLivenessLeaseShardCount)
}

// The shard of a client's lease, for writers that group their writes by shard.
func ProviderLivenessLeaseShard(clientId server.Id) int {
	return providerLivenessLeaseShard(clientId)
}

// The shard's sorted set. Its hash tag is the whole key: one cluster slot.
func providerLivenessLeaseKey(shard int) string {
	return fmt.Sprintf("{provider_live:%02d}", shard)
}

// Scores ARGV[1] with the node's clock in unix milliseconds and returns that
// score. With ARGV[2] above zero it first removes the members older than that
// many milliseconds. One key, so one slot.
var providerLivenessLeaseStampScript = redis.NewScript(`
local time = redis.call('TIME')
local now = tonumber(time[1]) * 1000 + math.floor(tonumber(time[2]) / 1000)
local pruneAge = tonumber(ARGV[2])
if 0 < pruneAge then
  redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', '(' .. string.format('%.0f', now - pruneAge))
end
redis.call('ZADD', KEYS[1], string.format('%.0f', now), ARGV[1])
return now
`)

// Writes one shard at the node's clock. ARGV[1] is the prune age in
// milliseconds, 0 for none; ARGV[2], ARGV[3] and ARGV[4] count the members
// that follow to score, to extend and to remove, in that order. A member to
// extend is scored only if the shard holds it. Returns, for each member to
// extend, 1 if it was held and 0 if not. One key, so one slot.
var providerLivenessLeaseShardScript = redis.NewScript(`
local time = redis.call('TIME')
local now = tonumber(time[1]) * 1000 + math.floor(tonumber(time[2]) / 1000)
local score = string.format('%.0f', now)
local pruneAge = tonumber(ARGV[1])
if 0 < pruneAge then
  redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', '(' .. string.format('%.0f', now - pruneAge))
end
local scoreCount = tonumber(ARGV[2])
local extendCount = tonumber(ARGV[3])
local removeCount = tonumber(ARGV[4])
local i = 5
for _ = 1, scoreCount do
  redis.call('ZADD', KEYS[1], score, ARGV[i])
  i = i + 1
end
local held = {}
for _ = 1, extendCount do
  if redis.call('ZSCORE', KEYS[1], ARGV[i]) then
    redis.call('ZADD', KEYS[1], score, ARGV[i])
    table.insert(held, 1)
  else
    table.insert(held, 0)
  end
  i = i + 1
end
for _ = 1, removeCount do
  redis.call('ZREM', KEYS[1], ARGV[i])
  i = i + 1
end
return held
`)

// Writes one shard's leases, never retried: scores the clients of
// scoredClientIds, extends those of extendedClientIds that the shard holds, and
// removes those of removedClientIds, in bounded scripts of at most
// providerLivenessLeaseScriptMaxClients clients each. held says, for each
// extended client, whether the shard held it. Every client must be in shard.
// An error is the first script's that failed; the scripts before it wrote.
func WriteProviderLivenessLeaseShard(
	ctx context.Context,
	shard int,
	scoredClientIds []server.Id,
	extendedClientIds []server.Id,
	removedClientIds []server.Id,
) (held []bool, returnErr error) {
	for _, clientIds := range [][]server.Id{scoredClientIds, extendedClientIds, removedClientIds} {
		for _, clientId := range clientIds {
			if providerLivenessLeaseShard(clientId) != shard {
				return nil, fmt.Errorf("client %s is not in lease shard %d", clientId, shard)
			}
		}
	}
	held = make([]bool, 0, len(extendedClientIds))
	for first := true; first || 0 < len(scoredClientIds)+len(extendedClientIds)+len(removedClientIds); first = false {
		// the next chunk: scored first, then extended, then removed
		budget := providerLivenessLeaseScriptMaxClients
		take := func(clientIds *[]server.Id) []server.Id {
			n := min(budget, len(*clientIds))
			budget -= n
			chunk := (*clientIds)[:n]
			*clientIds = (*clientIds)[n:]
			return chunk
		}
		scoredChunk := take(&scoredClientIds)
		extendedChunk := take(&extendedClientIds)
		removedChunk := take(&removedClientIds)
		chunkHeld, err := writeProviderLivenessLeaseShardScript(ctx, shard, scoredChunk, extendedChunk, removedChunk)
		if err != nil {
			return nil, err
		}
		held = append(held, chunkHeld...)
	}
	return held, nil
}

// One bounded shard script.
func writeProviderLivenessLeaseShardScript(
	ctx context.Context,
	shard int,
	scoredClientIds []server.Id,
	extendedClientIds []server.Id,
	removedClientIds []server.Id,
) (held []bool, returnErr error) {
	pruneAge := int64(0)
	if providerLivenessLeasePruneDue(shard, time.Now()) {
		pruneAge = providerLivenessLeasePruneAge.Milliseconds()
	}
	args := make([]any, 0, 4+len(scoredClientIds)+len(extendedClientIds)+len(removedClientIds))
	args = append(args, pruneAge, len(scoredClientIds), len(extendedClientIds), len(removedClientIds))
	for _, clientIds := range [][]server.Id{scoredClientIds, extendedClientIds, removedClientIds} {
		for _, clientId := range clientIds {
			args = append(args, clientId.String())
		}
	}
	returnErr = providerLivenessLeaseWrite(ctx, func(ctx context.Context, r server.RedisClient) error {
		flags, err := providerLivenessLeaseShardScript.Run(
			ctx,
			r,
			[]string{providerLivenessLeaseKey(shard)},
			args...,
		).Int64Slice()
		if err != nil {
			return err
		}
		if len(flags) != len(extendedClientIds) {
			return fmt.Errorf("lease shard %d answered %d extensions for %d", shard, len(flags), len(extendedClientIds))
		}
		held = make([]bool, len(flags))
		for i, flag := range flags {
			held[i] = flag == 1
		}
		return nil
	})
	if returnErr != nil {
		return nil, returnErr
	}
	return held, nil
}

// Scores the client's lease with the Redis node's clock, bounded and never
// retried like every lease write.
func RefreshProviderLivenessLease(ctx context.Context, clientId server.Id) error {
	_, err := WriteProviderLivenessLeaseShard(ctx, providerLivenessLeaseShard(clientId), []server.Id{clientId}, nil, nil)
	return err
}

// Removes the client's lease, bounded like a refresh. Only a client that
// stopped providing is released; a connection that ends leaves the lease to
// lapse within the window (see the file comment).
func ReleaseProviderLivenessLease(ctx context.Context, clientId server.Id) error {
	_, err := WriteProviderLivenessLeaseShard(ctx, providerLivenessLeaseShard(clientId), nil, nil, []server.Id{clientId})
	return err
}

// Stamps the connect handler in the writer set with the Redis node's clock,
// pruning stamps older than the prune age, bounded like a refresh. A connect
// process calls it every ProviderLivenessWriterInterval for as long as its
// handler lives, unless it withholds the stamp.
func PublishProviderLivenessWriter(ctx context.Context, handlerId server.Id) error {
	return providerLivenessLeaseWrite(ctx, func(ctx context.Context, r server.RedisClient) error {
		return providerLivenessLeaseStampScript.Run(
			ctx,
			r,
			[]string{providerLivenessWritersKey},
			handlerId.String(),
			providerLivenessLeasePruneAge.Milliseconds(),
		).Err()
	})
}

// Runs one lease write within ProviderLivenessLeaseWriteTimeout on the
// deadline-bound pool, which retries no command or redirect and applies the
// deadline to its socket I/O: a write never waits out the shared pool's 15 s
// timeouts and retries. Each write is a one-key script, which that pool's
// routing serves, and skips the pool's preflight ping, which would reach an
// arbitrary node. The callback must use the context it is given.
func providerLivenessLeaseWrite(ctx context.Context, callback func(context.Context, server.RedisClient) error) error {
	writeCtx, writeCancel := context.WithTimeout(ctx, ProviderLivenessLeaseWriteTimeout)
	defer writeCancel()
	return server.RedisWithDeadline(writeCtx, func(r server.RedisClient) error {
		return callback(writeCtx, r)
	}, server.OptRedisNoPreflight())
}

// The last prune of each shard by this process, in unix milliseconds.
var providerLivenessLeasePruneTimes [providerLivenessLeaseShardCount]atomic.Int64

// Whether a write of the shard at now prunes it. The write that finds the
// prune due claims it for the next interval.
func providerLivenessLeasePruneDue(shard int, now time.Time) bool {
	pruneTime := &providerLivenessLeasePruneTimes[shard]
	previous := pruneTime.Load()
	nowMillis := now.UnixMilli()
	if previous != 0 && nowMillis-previous < providerLivenessLeasePruneInterval.Milliseconds() {
		return false
	}
	return pruneTime.CompareAndSwap(previous, nowMillis)
}

// The client's last refresh by the Redis node's clock, and whether it has a
// lease, for tests of the writers.
func Testing_GetProviderLivenessLease(ctx context.Context, clientId server.Id) (refreshTime time.Time, ok bool) {
	server.Redis(ctx, func(r server.RedisClient) {
		score, err := r.ZScore(ctx, providerLivenessLeaseKey(providerLivenessLeaseShard(clientId)), clientId.String()).Result()
		if err == redis.Nil {
			return
		}
		server.Raise(err)
		refreshTime, ok = time.UnixMilli(int64(score)).UTC(), true
	})
	return
}

// The handler's last stamp in the writer set by the Redis node's clock, and
// whether it has one, for tests of the writers.
func Testing_GetProviderLivenessWriter(ctx context.Context, handlerId server.Id) (stampTime time.Time, ok bool) {
	server.Redis(ctx, func(r server.RedisClient) {
		score, err := r.ZScore(ctx, providerLivenessWritersKey, handlerId.String()).Result()
		if err == redis.Nil {
			return
		}
		server.Raise(err)
		stampTime, ok = time.UnixMilli(int64(score)).UTC(), true
	})
	return
}
