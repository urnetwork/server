// The provider liveness lease: whether a provider's connect announcement is
// alive now, which FindProviders2 needs when it answers. The pool it draws from
// is exported every 30 to 65 minutes, a connection row can stay connected after
// its transport is gone, and the request's own live read covers only the
// Quality natives it reads, so about 5-10% of the providers it lent were
// offline when it answered.
//
// Every announcement of a client that provides for discovery (Public or
// Network mode) refreshes the client's lease once per sync round
// (connect/transport_announce_liveness_lease.go). A lease is the client's
// member in one of providerLivenessLeaseShardCount sorted sets,
// `{provider_live:NN}`, scored by its last refresh in unix milliseconds of the
// Redis node's clock (TIME in the refresh script), so the clocks of the connect
// hosts never enter it. The braces are the whole key, so each shard is one
// cluster slot and a read pipelines its commands shard by shard. A refresh
// prunes its shard of members older than an hour now and then.
//
// Every connect process also stamps its handler in the writer set,
// `{provider_live_writers}`, every few seconds (PublishProviderLivenessWriter).
// The reader (provider_liveness_lease_read.go) enforces leases only while every
// live connect handler is stamped, so a connect process without this writer,
// mid-rollout, rolled back or built without it, fails every read open rather
// than leaving its providers without a lease.
//
// The lease is the client's, not one connection's. A connection that ends
// leaves it to lapse within the window: another connection of the client may
// still hold it, and a client that reconnects, after every drain or network
// change, needs it until the new connection's first refresh, about one sync
// round after that connection announces. Only a client that stops providing has
// its lease released at once, by each of its connections as its next read sees
// that; provide modes are the client's, so none of its connections still
// provides.
//
// The shard function, the keys and the scores are shared by connect writers and
// API readers of different builds, so they never change.
package model

import (
	"context"
	"fmt"
	"hash/fnv"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server/v2026"
)

// The sorted sets the leases are spread over. Each holds a few thousand
// members and takes about 60 refreshes a second at Main's scale.
const providerLivenessLeaseShardCount = 32

// How long a refresh keeps a provider live: five sync rounds of 30 s. It
// exceeds the longest round, SyncRetryMaxTimeout of two minutes, so a
// connection whose sync reads back off still keeps its lease with each round.
const providerLivenessLeaseWindow = 150 * time.Second

// A refresh prunes members older than this from its shard.
const providerLivenessLeasePruneAge = time.Hour

// A process prunes each shard at most once in this interval.
const providerLivenessLeasePruneInterval = time.Minute

// How often a connect process stamps its handler in the writer set.
const ProviderLivenessWriterInterval = 5 * time.Second

// A handler's stamp counts as current within this window. It is shorter than
// the lease window less a sync round (120 s), so a process whose Redis writes
// stop loses its stamp, and with it enforcement, before any lease it refreshed
// lapses.
const providerLivenessWriterWindow = 60 * time.Second

// The key of the writer set. Its hash tag is the whole key: one cluster slot.
const providerLivenessWritersKey = "{provider_live_writers}"

// The shard of a client's lease: FNV-1a of the id's 16 bytes.
func providerLivenessLeaseShard(clientId server.Id) int {
	hash := fnv.New32a()
	hash.Write(clientId[:])
	return int(hash.Sum32() % providerLivenessLeaseShardCount)
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

// Scores the client's lease with the Redis node's clock. Each connection of a
// client that provides for discovery calls it once per sync round. The error
// is a failed write, which the caller counts and writes again next round; a
// lease outlives four missed rounds.
func RefreshProviderLivenessLease(ctx context.Context, clientId server.Id) error {
	shard := providerLivenessLeaseShard(clientId)
	pruneAge := int64(0)
	if providerLivenessLeasePruneDue(shard, time.Now()) {
		pruneAge = providerLivenessLeasePruneAge.Milliseconds()
	}
	return providerLivenessLeaseRedis(ctx, func(r server.RedisClient) error {
		return providerLivenessLeaseStampScript.Run(
			ctx,
			r,
			[]string{providerLivenessLeaseKey(shard)},
			clientId.String(),
			pruneAge,
		).Err()
	})
}

// Removes the client's lease. Only a client that stopped providing is
// released; a connection that ends leaves the lease to lapse within the window
// (see the file comment).
func ReleaseProviderLivenessLease(ctx context.Context, clientId server.Id) error {
	return providerLivenessLeaseRedis(ctx, func(r server.RedisClient) error {
		return r.ZRem(ctx, providerLivenessLeaseKey(providerLivenessLeaseShard(clientId)), clientId.String()).Err()
	})
}

// Stamps the connect handler in the writer set with the Redis node's clock,
// pruning stamps older than the prune age. A connect process calls it every
// ProviderLivenessWriterInterval for as long as its handler lives, starting
// as soon as the handler exists.
func PublishProviderLivenessWriter(ctx context.Context, handlerId server.Id) error {
	return providerLivenessLeaseRedis(ctx, func(r server.RedisClient) error {
		return providerLivenessLeaseStampScript.Run(
			ctx,
			r,
			[]string{providerLivenessWritersKey},
			handlerId.String(),
			providerLivenessLeasePruneAge.Milliseconds(),
		).Err()
	})
}

// The last prune of each shard by this process, in unix milliseconds.
var providerLivenessLeasePruneTimes [providerLivenessLeaseShardCount]atomic.Int64

// Whether a refresh of the shard at now prunes it. The refresh that finds the
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

// Runs one lease operation on the shared pool without retries and returns its
// failure, returned or raised, as an error. A lease operation is never worth a
// retry loop: a writer writes again next round, and a reader fails open. A
// programming fault still raises.
func providerLivenessLeaseRedis(ctx context.Context, callback func(server.RedisClient) error) (returnErr error) {
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		if _, fault := r.(runtime.Error); fault {
			panic(r)
		}
		if err, ok := r.(error); ok {
			returnErr = err
		} else {
			returnErr = fmt.Errorf("provider liveness lease: %v", r)
		}
	}()
	server.Redis(ctx, func(r server.RedisClient) {
		returnErr = callback(r)
	}, server.OptNoRetry())
	return
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
