package sidecarhost

// §22.5's op-counting Redis: a sidecar.Redis that counts each call through it by name before passing it
// on, so a hot path's operations are counted on the real store as T2's fake counts them on its own
// (sidecartest.FakeRedis.OpCounts): get, mget, set, setnx, cas, cad, incr, del, expire, hget, hgetall,
// hset, hdel, hexpire, xadd, xrange, xgroup, xreadgroup, xack, xautoclaim, and a script as
// script:<name>. A Find is one script (§9.8), and TEST-M1-09's InlinePath fails a path that makes more
// calls than its ledger allows. A call counts when it is made, whatever it answers, so a refused or
// failed call still counts against its path. Registering a key-event callback is no call. Safe for
// concurrent use.

import (
	"context"
	"maps"
	"sync"
	"time"

	"github.com/urnetwork/sidecar"
)

var _ sidecar.Redis = (*CountingRedis)(nil)

// Counts calls to the Redis it wraps. NewCountingRedis makes one.
type CountingRedis struct {
	redis sidecar.Redis

	stateLock sync.Mutex
	opCounts  map[string]int
}

// Counts the calls made to redis through it.
func NewCountingRedis(redis sidecar.Redis) *CountingRedis {
	return &CountingRedis{
		redis:    redis,
		opCounts: map[string]int{},
	}
}

// Counts one call by name.
func (self *CountingRedis) count(op string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.opCounts[op] += 1
}

// The calls so far, by name.
func (self *CountingRedis) OpCounts() map[string]int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return maps.Clone(self.opCounts)
}

// Every call so far, whatever its name.
func (self *CountingRedis) OpCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	count := 0
	for _, opCount := range self.opCounts {
		count += opCount
	}
	return count
}

// Forgets the counts, as at the start of a path a test measures.
func (self *CountingRedis) ResetOpCounts() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	clear(self.opCounts)
}

// Counted as get.
func (self *CountingRedis) Get(ctx context.Context, key string) ([]byte, bool, error) {
	self.count("get")
	return self.redis.Get(ctx, key)
}

// Counted as mget.
func (self *CountingRedis) GetMany(ctx context.Context, keys []string) ([][]byte, error) {
	self.count("mget")
	return self.redis.GetMany(ctx, keys)
}

// Counted as set.
func (self *CountingRedis) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	self.count("set")
	return self.redis.Set(ctx, key, value, ttl)
}

// Counted as setnx.
func (self *CountingRedis) SetIfAbsent(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error) {
	self.count("setnx")
	return self.redis.SetIfAbsent(ctx, key, value, ttl)
}

// Counted as cas.
func (self *CountingRedis) CompareAndSet(ctx context.Context, key string, expected []byte, value []byte, ttl time.Duration) (bool, error) {
	self.count("cas")
	return self.redis.CompareAndSet(ctx, key, expected, value, ttl)
}

// Counted as cad.
func (self *CountingRedis) CompareAndDelete(ctx context.Context, key string, expected []byte) (bool, error) {
	self.count("cad")
	return self.redis.CompareAndDelete(ctx, key, expected)
}

// Counted as incr.
func (self *CountingRedis) Increment(ctx context.Context, key string, ttl time.Duration) (int64, error) {
	self.count("incr")
	return self.redis.Increment(ctx, key, ttl)
}

// Counted as del.
func (self *CountingRedis) Delete(ctx context.Context, keys []string) error {
	self.count("del")
	return self.redis.Delete(ctx, keys)
}

// Counted as expire.
func (self *CountingRedis) Expire(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	self.count("expire")
	return self.redis.Expire(ctx, key, ttl)
}

// Counted as hget.
func (self *CountingRedis) HashGet(ctx context.Context, key string, field string) ([]byte, bool, error) {
	self.count("hget")
	return self.redis.HashGet(ctx, key, field)
}

// Counted as hgetall.
func (self *CountingRedis) HashGetAll(ctx context.Context, key string) (map[string][]byte, error) {
	self.count("hgetall")
	return self.redis.HashGetAll(ctx, key)
}

// Counted as hset.
func (self *CountingRedis) HashSet(ctx context.Context, key string, fieldValues map[string][]byte) error {
	self.count("hset")
	return self.redis.HashSet(ctx, key, fieldValues)
}

// Counted as hdel.
func (self *CountingRedis) HashDelete(ctx context.Context, key string, fields []string) error {
	self.count("hdel")
	return self.redis.HashDelete(ctx, key, fields)
}

// Counted as hexpire.
func (self *CountingRedis) HashExpire(ctx context.Context, key string, fields []string, ttl time.Duration) error {
	self.count("hexpire")
	return self.redis.HashExpire(ctx, key, fields, ttl)
}

// Counted as xadd.
func (self *CountingRedis) StreamAdd(ctx context.Context, key string, maxLength int64, fieldValues map[string][]byte) (string, error) {
	self.count("xadd")
	return self.redis.StreamAdd(ctx, key, maxLength, fieldValues)
}

// Counted as xrange.
func (self *CountingRedis) StreamRange(ctx context.Context, key string, afterEntryId string, count int64) ([]*sidecar.RedisStreamEntry, error) {
	self.count("xrange")
	return self.redis.StreamRange(ctx, key, afterEntryId, count)
}

// Counted as xgroup.
func (self *CountingRedis) StreamGroupCreate(ctx context.Context, key string, group string) error {
	self.count("xgroup")
	return self.redis.StreamGroupCreate(ctx, key, group)
}

// Counted as xreadgroup.
func (self *CountingRedis) StreamGroupRead(ctx context.Context, key string, group string, consumer string, count int64, block time.Duration) ([]*sidecar.RedisStreamEntry, error) {
	self.count("xreadgroup")
	return self.redis.StreamGroupRead(ctx, key, group, consumer, count, block)
}

// Counted as xack.
func (self *CountingRedis) StreamGroupAck(ctx context.Context, key string, group string, entryIds []string) error {
	self.count("xack")
	return self.redis.StreamGroupAck(ctx, key, group, entryIds)
}

// Counted as xautoclaim.
func (self *CountingRedis) StreamGroupClaim(ctx context.Context, key string, group string, consumer string, minIdle time.Duration, cursor string, count int64) ([]*sidecar.RedisStreamEntry, string, error) {
	self.count("xautoclaim")
	return self.redis.StreamGroupClaim(ctx, key, group, consumer, minIdle, cursor, count)
}

// Counted as script:<name>.
func (self *CountingRedis) RunScript(ctx context.Context, script *sidecar.RedisScript, keys []string, args []string) (*sidecar.RedisReply, error) {
	name := ""
	if script != nil {
		name = script.Name
	}
	self.count("script:" + name)
	return self.redis.RunScript(ctx, script, keys, args)
}

// Not counted: no call to Redis.
func (self *CountingRedis) AddKeyEventCallback(keyPattern string, callback sidecar.RedisKeyEventFunction) (func(), error) {
	return self.redis.AddKeyEventCallback(keyPattern, callback)
}
